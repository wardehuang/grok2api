package gateway

// console_guard.go：Console 账号专用的降智扣流/换号/停用防护。
//
// 判定与决策逻辑从 fork（lij768423-svg/grok2api，commit 8955728）的
// quality_retry.go 忠实复制并重命名；差异点：
//   - 仅作用于 ProviderConsole 的流式请求
//   - withhold 后直接 Enabled=false 真停用账号（无冷却、无二次机会），由管理员手动恢复
//   - 固定 maxAttempts=5（总共 5 个账号）、holdTimeout=30s、恒 fail_closed
//     （5 个账号全部 withhold 时返回 503 quality_degraded，绝不放行降智响应体）
//   - 与 qualityGuard/requestRetry 完全独立，不读取、不修改其任何状态

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

const (
	ConsoleGuardErrorCode         = "console_guard_degraded"
	consoleGuardMaxAttempts       = 5
	consoleGuardHoldTimeout       = 30 * time.Second
	consoleGuardMinOutputTokens   = int64(8)
	lastErrorConsoleGuardDisabled = "console_guard_degraded_disabled"
)

var errConsoleGuardEmptyStream = errors.New("上游流式响应为空")

// ConsoleGuardRuntime 是 console guard 的运行时配置。Zero Enabled 关闭防护。
type ConsoleGuardRuntime struct {
	Enabled         bool
	HoldTimeout     time.Duration
	MinOutputTokens int64
}

// ConsoleGuardSignals 是扣流判定输入。
type ConsoleGuardSignals struct {
	HasThinking bool
	// ReasoningStarted 表示出现了空 reasoning item 或 Chat SSE stub
	// ": grok2api-reasoning-start"。这不是思考证据：降智流同样会发 stub，
	// 随后倾倒可见 token 且 usage 里 reasoning 为 0 或虚报。
	ReasoningStarted bool
	VisibleTokens    int64
	ReasoningTokens  int64
	OutputTokens     int64
	Terminal         bool
	HoldExpired      bool
}

// ConsoleGuardVerdict 是单条上游流的扣流判定。
type ConsoleGuardVerdict string

const (
	ConsoleGuardWait     ConsoleGuardVerdict = "wait"
	ConsoleGuardDeliver  ConsoleGuardVerdict = "deliver"
	ConsoleGuardWithhold ConsoleGuardVerdict = "withhold"
)

// ConsoleGuardAction 是 attempt 循环对 withhold 判定的动作。
type ConsoleGuardAction string

const (
	ConsoleGuardActionDeliver     ConsoleGuardAction = "deliver"
	ConsoleGuardActionDeliverLast ConsoleGuardAction = "deliver_last"
	ConsoleGuardActionRetry       ConsoleGuardAction = "retry"
	ConsoleGuardActionReject      ConsoleGuardAction = "reject"
)

func normalizeConsoleGuard(cfg ConsoleGuardRuntime) ConsoleGuardRuntime {
	if cfg.HoldTimeout <= 0 {
		cfg.HoldTimeout = consoleGuardHoldTimeout
	}
	if cfg.MinOutputTokens <= 0 {
		cfg.MinOutputTokens = consoleGuardMinOutputTokens
	}
	return cfg
}

func (s *Service) UpdateConsoleGuard(cfg ConsoleGuardRuntime) {
	normalized := normalizeConsoleGuard(cfg)
	s.consoleGuard.Store(&normalized)
}

func (s *Service) consoleGuardConfig() ConsoleGuardRuntime {
	if s == nil {
		return normalizeConsoleGuard(ConsoleGuardRuntime{})
	}
	if value := s.consoleGuard.Load(); value != nil {
		return *value
	}
	return normalizeConsoleGuard(ConsoleGuardRuntime{})
}

// ClassifyConsoleGuardHold 决定扣住的流能否转发。与 fork ClassifyQualityHold 同构：
// 流式 thinking 恒放行；reasoning_tokens 单独出现不放行（降智上游会虚报该字段）；
// 已完成且可见输出足够但无 streamed thinking 即扣流；低于 minOutput 的短回答放行；
// hold 超时且无可见输出时不 fail-open：继续等更多字节或流中断，空挂起不会被
// 当作 HTTP 200 冲洗出去。
func ClassifyConsoleGuardHold(sig ConsoleGuardSignals, minOutput int64) ConsoleGuardVerdict {
	if minOutput <= 0 {
		minOutput = consoleGuardMinOutputTokens
	}
	if sig.HasThinking {
		return ConsoleGuardDeliver
	}
	output := sig.OutputTokens
	if output < sig.VisibleTokens {
		output = sig.VisibleTokens
	}
	enough := output >= minOutput
	if sig.ReasoningStarted && !sig.Terminal && !sig.HoldExpired {
		return ConsoleGuardWait
	}
	if sig.Terminal {
		if output <= 0 {
			return ConsoleGuardWait
		}
		if enough {
			return ConsoleGuardWithhold
		}
		return ConsoleGuardDeliver
	}
	if enough {
		return ConsoleGuardWithhold
	}
	if sig.HoldExpired {
		if output <= 0 {
			return ConsoleGuardWait
		}
		if enough {
			return ConsoleGuardWithhold
		}
		return ConsoleGuardDeliver
	}
	return ConsoleGuardWait
}

// consoleGuardPeekAbortError 优先返回 idle-timeout 原因而非裸 context.Canceled，
// 让 attempt 循环按 transport 失败换号而不是当成客户端 499。
func consoleGuardPeekAbortError(ctx context.Context, err error) error {
	if ctx != nil {
		if cause := context.Cause(ctx); neterrorpkg.IsUpstreamStreamIdleTimeout(cause) {
			return cause
		}
	}
	if neterrorpkg.IsUpstreamStreamIdleTimeout(err) {
		return err
	}
	if err != nil {
		return err
	}
	if ctx != nil {
		return ctx.Err()
	}
	return nil
}

// DecideConsoleGuardRetry：withhold 后在 maxAttempts 内换号。最后一个 attempt
// 也扣住时拒绝（fail_closed），不存在 fail_open 兜底。
func DecideConsoleGuardRetry(verdict ConsoleGuardVerdict, attemptIndex, maxAttempts int) ConsoleGuardAction {
	if verdict != ConsoleGuardWithhold {
		return ConsoleGuardActionDeliver
	}
	if maxAttempts <= 0 {
		maxAttempts = consoleGuardMaxAttempts
	}
	if attemptIndex < 0 {
		attemptIndex = 0
	}
	if attemptIndex < maxAttempts-1 {
		return ConsoleGuardActionRetry
	}
	return ConsoleGuardActionReject
}

// BoundConsoleGuardRetry 在路由循环没有剩余账号时把 Retry 收敛为 Reject，
// 扣住的响应体不会被丢进耗尽的循环里。
func BoundConsoleGuardRetry(action ConsoleGuardAction, hasNextRoutingAttempt bool) ConsoleGuardAction {
	if action != ConsoleGuardActionRetry || hasNextRoutingAttempt {
		return action
	}
	return ConsoleGuardActionReject
}

// ConsoleGuardCommit 是单个 attempt 的最终决定。
type ConsoleGuardCommit struct {
	Action   ConsoleGuardAction
	Audit    bool
	KeepBody bool
}

// CommitConsoleGuardHold 是扣流/换号/提交的唯一定点。
func CommitConsoleGuardHold(verdict ConsoleGuardVerdict, attemptIndex, maxAttempts int, hasNextRouting bool) ConsoleGuardCommit {
	action := BoundConsoleGuardRetry(
		DecideConsoleGuardRetry(verdict, attemptIndex, maxAttempts),
		hasNextRouting,
	)
	switch action {
	case ConsoleGuardActionRetry, ConsoleGuardActionReject:
		return ConsoleGuardCommit{Action: action, Audit: true, KeepBody: false}
	case ConsoleGuardActionDeliverLast:
		return ConsoleGuardCommit{Action: action, Audit: false, KeepBody: true}
	default:
		return ConsoleGuardCommit{Action: ConsoleGuardActionDeliver, Audit: false, KeepBody: true}
	}
}

func shouldHoldConsoleGuardStream(input Input, ownership *inferencedomain.ResponseOwnership, route modeldomain.Route, operation audit.Operation, cfg ConsoleGuardRuntime) bool {
	if !cfg.Enabled || !input.Streaming || input.ForcedEgressNodeID != 0 || ownership != nil || input.skipQualityHold {
		return false
	}
	switch operation {
	case audit.OperationChat, audit.OperationResponses, audit.OperationMessages, "":
	default:
		return false
	}
	if isResponsesCompactionRequest(input.Body) {
		return false
	}
	// 仅 Console。
	if route.Provider != accountdomain.ProviderConsole {
		return false
	}
	// 显式关闭 reasoning 的请求不检测。
	if consoleGuardRequestDisablesReasoning(input.Body) {
		return false
	}
	if modeldomain.SupportsReasoningForProvider(route.Provider, input.PublicModel) {
		return true
	}
	return modeldomain.SupportsReasoningForProvider(route.Provider, route.UpstreamModel)
}

func consoleGuardRequestDisablesReasoning(body []byte) bool {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	if consoleGuardJSONStringEquals(payload["reasoning_effort"], modeldomain.ReasoningEffortNone) {
		return true
	}
	for _, key := range []string{"reasoning", "output_config", "thinking"} {
		var nested map[string]json.RawMessage
		if json.Unmarshal(payload[key], &nested) != nil {
			continue
		}
		if consoleGuardJSONStringEquals(nested["effort"], modeldomain.ReasoningEffortNone) || consoleGuardJSONStringEquals(nested["type"], "disabled") {
			return true
		}
		var budget int64
		if raw, ok := nested["budget_tokens"]; ok && json.Unmarshal(raw, &budget) == nil && budget == 0 {
			return true
		}
	}
	return consoleGuardJSONStringEquals(payload["thinking"], "disabled")
}

func consoleGuardJSONStringEquals(raw json.RawMessage, want string) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && strings.EqualFold(strings.TrimSpace(value), want)
}

// disableConsoleGuardAccount 对降智的 Console 账号立即真停用：
// Enabled=false + 健康标记 + 失效广播 + 候选缓存剔除 + sticky 清除。
// 无冷却概念；恢复只能由管理员手动启用。
func (s *Service) disableConsoleGuardAccount(ctx context.Context, requestID string, credential accountdomain.Credential) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
	defer cancel()
	if err := s.selector.disableConsoleGuardAccount(writeCtx, credential); err != nil {
		s.logger.Error("console_guard_disable_failed", "request_id", requestID, "account_id", credential.ID, "error", err)
		return
	}
	s.logger.Info("console_guard_disabled", "request_id", requestID, "account_id", credential.ID, "account_name", credential.Name)
}

func (s *Service) recordConsoleGuardDegraded(ctx context.Context, base audit.Record, credential accountdomain.Credential, usage Usage, startedAt time.Time, trace *infraegress.Trace, provider accountdomain.Provider) {
	record := base
	record.EventID = newAuditEventID()
	accountID := credential.ID
	record.AccountID = &accountID
	record.AccountName = credential.Name
	record.StatusCode = http.StatusOK
	record.ErrorCode = ConsoleGuardErrorCode
	record.OutputTokens = usage.OutputTokens
	record.ReasoningTokens = usage.ReasoningTokens
	record.TotalTokens = usage.TotalTokens
	record.InputTokens = usage.InputTokens
	if usage.Reported {
		record.UsageSource = audit.UsageSourceUpstream
	}
	record.DurationMS = time.Since(startedAt).Milliseconds()
	record.CreatedAt = time.Now().UTC()
	applyAuditEgress(&record, trace, provider)
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
	defer cancel()
	if err := s.audits.Create(writeCtx, record); err != nil {
		s.logger.Error("console_guard_audit_failed", "event_id", record.EventID, "request_id", record.RequestID, "error", err)
	}
}
