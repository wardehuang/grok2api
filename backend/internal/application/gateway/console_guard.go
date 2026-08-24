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
	"fmt"
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
	lastErrorConsoleGuardDisabled = "console_guard_degraded_disabled"
)

var errConsoleGuardEmptyStream = errors.New("上游流式响应为空")

// ConsoleGuardRuntime 是 console guard 的运行时配置。Zero Enabled 关闭防护。
type ConsoleGuardRuntime struct {
	Enabled     bool
	HoldTimeout time.Duration
	SoftTPS     float64
	HardTPS     float64
}

// ConsoleGuardSignals 是扣流判定输入。
type ConsoleGuardSignals struct {
	HasThinking bool
	// ReasoningStarted 表示出现了空 reasoning item 或 Chat SSE stub
	// ": grok2api-reasoning-start"。这不是思考证据：降智流同样会发 stub，
	// 随后倾倒可见 token 且 usage 里 reasoning 为 0 或虚报。
	ReasoningStarted       bool
	VisibleTokens          int64
	ReasoningTokens        int64
	OutputTokens           int64
	Terminal               bool
	HoldExpired            bool
	ThinkingEvidence       []audit.ConsoleGuardEvidence
	ReasoningStartEvidence []audit.ConsoleGuardEvidence
	TerminalEvent          string
	VisibleRunes           int64
	ObservationDurationMS  int64
	FirstVisibleObserved   bool
	FirstVisibleMS         int64
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
	if cfg.SoftTPS <= 0 {
		cfg.SoftTPS = audit.DefaultDegradeSoftTPS
	}
	if cfg.HardTPS <= 0 {
		cfg.HardTPS = audit.DefaultDegradeHardTPS
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

// ClassifyConsoleGuardHold 按 Console TPS 阈值决定扣住的流能否转发：
// TPS 超过 hardTPS 无条件判定降智；TPS 超过 softTPS 且没有真实 thinking
// 也判定降智。阈值未命中时，终止流或检测窗口到期后放行；空流交由
// finishConsoleGuardPeek 继续按传输错误处理。
func ClassifyConsoleGuardHold(sig ConsoleGuardSignals, softTPS, hardTPS float64) ConsoleGuardVerdict {
	if softTPS <= 0 {
		softTPS = audit.DefaultDegradeSoftTPS
	}
	if hardTPS <= 0 {
		hardTPS = audit.DefaultDegradeHardTPS
	}
	tps := consoleGuardOutputTokensPerSecond(sig, Usage{})
	if tps > hardTPS {
		return ConsoleGuardWithhold
	}
	if sig.HasThinking {
		return ConsoleGuardDeliver
	}
	if tps > softTPS {
		return ConsoleGuardWithhold
	}
	if sig.Terminal {
		if consoleGuardEffectiveOutputTokens(sig) <= 0 {
			return ConsoleGuardWait
		}
		return ConsoleGuardDeliver
	}
	if sig.HoldExpired {
		if consoleGuardEffectiveOutputTokens(sig) <= 0 {
			return ConsoleGuardWait
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

func (s *Service) recordConsoleGuardDegraded(ctx context.Context, base audit.Record, credential accountdomain.Credential, protocol string, usage Usage, signals ConsoleGuardSignals, cfg ConsoleGuardRuntime, commit ConsoleGuardCommit, attempt int, startedAt, responseStartedAt time.Time, trace *infraegress.Trace, provider accountdomain.Provider) {
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
	record.ConsoleGuard = buildConsoleGuardDetail(protocol, signals, usage, cfg, commit, attempt, time.Since(responseStartedAt).Milliseconds())
	applyAuditEgress(&record, trace, provider)
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
	defer cancel()
	if err := s.audits.Create(writeCtx, record); err != nil {
		s.logger.Error("console_guard_audit_failed", "event_id", record.EventID, "request_id", record.RequestID, "error", err)
	}
}

func consoleGuardEffectiveOutputTokens(signals ConsoleGuardSignals) int64 {
	return max(signals.OutputTokens, signals.VisibleTokens)
}

func consoleGuardFirstTokenMS(signals ConsoleGuardSignals) int64 {
	if !signals.FirstVisibleObserved {
		return 0
	}
	return max(0, signals.FirstVisibleMS)
}

func consoleGuardGenerationWindowMS(signals ConsoleGuardSignals) int64 {
	return audit.GenerationWindowMS(consoleGuardFirstTokenMS(signals), signals.ObservationDurationMS)
}

func consoleGuardOutputTokensPerSecond(signals ConsoleGuardSignals, usage Usage) float64 {
	outputTokens := consoleGuardEffectiveOutputTokens(signals)
	reasoningTokens := max(signals.ReasoningTokens, usage.ReasoningTokens)
	return audit.OutputTokensPerSecond(outputTokens, reasoningTokens, consoleGuardFirstTokenMS(signals), signals.ObservationDurationMS)
}

func buildConsoleGuardDetail(protocol string, signals ConsoleGuardSignals, usage Usage, cfg ConsoleGuardRuntime, commit ConsoleGuardCommit, attempt int, upstreamDurationMS int64) *audit.ConsoleGuardDetail {
	outputTokens := consoleGuardEffectiveOutputTokens(signals)
	reasoningTokens := max(signals.ReasoningTokens, usage.ReasoningTokens)
	generationWindowMS := consoleGuardGenerationWindowMS(signals)
	outputTokensPerSecond := consoleGuardOutputTokensPerSecond(signals, usage)
	decisionReasons := make([]audit.ConsoleGuardEvidence, 0, 8)
	if signals.HasThinking {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "real_thinking_present", Detail: "观察到真实 reasoning 内容；软阈值条件不会命中，但硬阈值仍独立判断"})
	} else {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "real_thinking_absent", Detail: "未观察到 reasoning delta、encrypted_content 或带文本的 thinking_delta；hasThinking=false"})
	}
	if signals.ReasoningStarted && len(signals.ThinkingEvidence) == 0 {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "reasoning_started_without_content", Detail: "只观察到 reasoning 起始标记或空 reasoning item，未观察到真实思考文本"})
	}
	if reasoningTokens > 0 && !signals.HasThinking {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "reasoning_tokens_not_evidence", Detail: fmt.Sprintf("reasoning tokens=%d 仅作统计，未作为真实思考证据")})
	}
	decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "tps_formula", Detail: fmt.Sprintf("Token/s=(output_tokens + reasoning_tokens)*1000/(duration_ms - first_token_ms)=(%d + %d)*1000/(%d - %d)=%.2f", outputTokens, reasoningTokens, signals.ObservationDurationMS, consoleGuardFirstTokenMS(signals), outputTokensPerSecond)})
	decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "tps_thresholds", Detail: fmt.Sprintf("softTPS=%.2f, hardTPS=%.2f, currentTPS=%.2f", cfg.SoftTPS, cfg.HardTPS, outputTokensPerSecond)})
	if outputTokensPerSecond > cfg.HardTPS {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "hard_tps_exceeded", Detail: fmt.Sprintf("current TPS %.2f > hardTPS %.2f；无论是否有 thinking 都判定降智", outputTokensPerSecond, cfg.HardTPS)})
	} else if outputTokensPerSecond > cfg.SoftTPS && !signals.HasThinking {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "soft_tps_exceeded_without_thinking", Detail: fmt.Sprintf("current TPS %.2f > softTPS %.2f 且 hasThinking=false，判定降智", outputTokensPerSecond, cfg.SoftTPS)})
	}
	if signals.Terminal {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "terminal_observed", Detail: "已观察到终止事件：" + signals.TerminalEvent})
	} else if signals.HoldExpired {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "hold_timeout_reached", Detail: fmt.Sprintf("hold timeout=%dms 已到期", cfg.HoldTimeout.Milliseconds())})
	}
	decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "classified_withhold", Detail: "ClassifyConsoleGuardHold 判定为 withhold，扣住响应并进入降智处理"})
	return &audit.ConsoleGuardDetail{
		Protocol:               protocol,
		Verdict:                string(ConsoleGuardWithhold),
		Action:                 string(commit.Action),
		Attempt:                attempt,
		MaxAttempts:            consoleGuardMaxAttempts,
		MinOutputTokens:        0,
		SoftTPS:                cfg.SoftTPS,
		HardTPS:                cfg.HardTPS,
		HoldTimeoutMS:          cfg.HoldTimeout.Milliseconds(),
		HasThinking:            signals.HasThinking,
		ThinkingEvidence:       append([]audit.ConsoleGuardEvidence{}, signals.ThinkingEvidence...),
		ReasoningStarted:       signals.ReasoningStarted,
		ReasoningStartEvidence: append([]audit.ConsoleGuardEvidence{}, signals.ReasoningStartEvidence...),
		VisibleRunes:           signals.VisibleRunes,
		VisibleTokens:          signals.VisibleTokens,
		OutputTokens:           outputTokens,
		ReasoningTokens:        reasoningTokens,
		UsageReported:          usage.Reported,
		UsageInputTokens:       usage.InputTokens,
		UsageOutputTokens:      usage.OutputTokens,
		UsageReasoningTokens:   usage.ReasoningTokens,
		UsageTotalTokens:       usage.TotalTokens,
		Terminal:               signals.Terminal,
		TerminalEvent:          signals.TerminalEvent,
		HoldExpired:            signals.HoldExpired,
		ObservationDurationMS:  signals.ObservationDurationMS,
		UpstreamDurationMS:     upstreamDurationMS,
		FirstVisibleObserved:   signals.FirstVisibleObserved,
		FirstVisibleMS:         signals.FirstVisibleMS,
		GenerationWindowMS:     generationWindowMS,
		OutputTokensPerSecond:  outputTokensPerSecond,
		AccountDisabled:        true,
		DecisionReasons:        decisionReasons,
	}
}
