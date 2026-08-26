package gateway

// console_guard.go：Console 账号专用的降智扣流/换号/停用防护。
//
// 判定与决策逻辑从 fork（lij768423-svg/grok2api，commit 8955728）的
// quality_retry.go 忠实复制并重命名；差异点：
//   - 仅作用于 ProviderConsole 的流式请求
//   - withhold 后直接 Enabled=false 真停用账号（无冷却、无二次机会），由管理员手动恢复
//   - 固定 maxAttempts=5（总共 5 个账号）、holdTimeout=30s、noDataTimeout=60s、恒 fail_closed
//     （5 个账号全部 withhold 时返回 503 quality_degraded，绝不放行降智响应体）
//   - 与 qualityGuard/requestRetry 完全独立，不读取、不修改其任何状态

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/pkg/consoleguardfile"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

const (
	ConsoleGuardErrorCode           = "console_guard_degraded"
	consoleGuardMaxAttempts         = 5
	consoleGuardHoldTimeout         = 30 * time.Second
	consoleGuardNoDataTimeout       = 60 * time.Second
	lastErrorConsoleGuardDisabled   = "console_guard_degraded_disabled"
	lastErrorConsoleNoProxyDisabled = "console_guard_no_proxy_disabled"
	consoleGuardNoProxyErrorCode    = "console_guard_no_proxy"
)

var (
	errConsoleGuardEmptyStream   = errors.New("上游流式响应为空")
	errConsoleGuardNoDataTimeout = errors.New("上游 60 秒未返回任何数据")
)

const consoleGuardUpstreamClientClosedStatus = 499

func isConsoleGuardNoDataTimeout(ctx context.Context, err error) bool {
	if errors.Is(err, errConsoleGuardNoDataTimeout) {
		return true
	}
	return ctx != nil && errors.Is(context.Cause(ctx), errConsoleGuardNoDataTimeout)
}

// ConsoleGuardRuntime 是 console guard 的运行时配置。Zero Enabled 关闭防护。
type ConsoleGuardRuntime struct {
	Enabled                     bool
	HoldTimeout                 time.Duration
	SoftTPS                     float64
	HardTPS                     float64
	FirstTokenThresholdMS       int64
	GenerationWindowThresholdMS int64
	MinOutputReasoningTokens    int64
	RecordNonDegradedEvents     bool
	RecordNonDegradedEventsSet  bool
	DegradedEgressNodeFilePath  string
}

type consoleGuardProxyURLResolver interface {
	ProxyURL(context.Context, uint64) (string, error)
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
	// FirstVisibleObserved / FirstVisibleMS 保留旧 JSON 字段名，实际承载与主审计
	// 相同的首个 generated delta 时间；可见文本统计仍由 VisibleRunes/VisibleTokens 承载。
	FirstVisibleObserved bool
	FirstVisibleMS       int64
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
	if cfg.FirstTokenThresholdMS <= 0 {
		cfg.FirstTokenThresholdMS = 5000
	}
	if cfg.GenerationWindowThresholdMS <= 0 {
		cfg.GenerationWindowThresholdMS = 1250
	}
	if cfg.MinOutputReasoningTokens <= 0 {
		cfg.MinOutputReasoningTokens = 300
	}
	if !cfg.RecordNonDegradedEventsSet {
		cfg.RecordNonDegradedEvents = true
	}
	if strings.TrimSpace(cfg.DegradedEgressNodeFilePath) == "" {
		cfg.DegradedEgressNodeFilePath = consoleguardfile.DefaultPath
	}
	return cfg
}

func (s *Service) UpdateConsoleGuard(cfg ConsoleGuardRuntime) {
	normalized := normalizeConsoleGuard(cfg)
	s.consoleGuard.Store(&normalized)
}

func (s *Service) SetConsoleGuardProxyURLResolver(resolver consoleGuardProxyURLResolver) {
	s.consoleGuardProxyURLResolver = resolver
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

// ClassifyConsoleGuardHold 按 Console TPS 和慢首字 burst 阈值决定扣住的流能否转发：
// TPS 超过 hardTPS 无条件判定降智；TPS 超过 softTPS 且没有真实 thinking 也判定降智。
// 前两项未命中后，若首字慢、首字后的生成窗口短、输出与 reasoning token 同时超过阈值，
// 仍判定降智。若终止前从未观察到真实 generated delta，则 TPS 不可用，改按
// “terminal-only + 慢完成 + 高 token”独立判定。首字超时直接 withhold；首字及时出现后
// 扣住到终止事件再结算，空流交由 finishConsoleGuardPeek 继续按传输错误处理。
//
// 扣流语义（2026-08 重定）：整条流被扣住直到流结束才做最终判定，因此 completed 事件里的
// 上游 usage 一定可用。TPS 判定要求生成窗口达到最小阈值：首个可见内容后的毫秒级
// 分母会让瞬时 TPS 爆炸性虚高（正常流同样命中），必须等窗口成熟。burst 判定则保留
// “慢首字 + 短生成窗口 + 高 token”三项同时命中的独立口径。
func ClassifyConsoleGuardHold(sig ConsoleGuardSignals, cfg ConsoleGuardRuntime) ConsoleGuardVerdict {
	cfg = normalizeConsoleGuard(cfg)
	generationWindowMS := consoleGuardGenerationWindowMS(sig)
	tps := consoleGuardOutputTokensPerSecond(sig, Usage{})
	if generationWindowMS >= cfg.GenerationWindowThresholdMS && tps > cfg.HardTPS {
		return ConsoleGuardWithhold
	}
	if generationWindowMS >= cfg.GenerationWindowThresholdMS && tps > cfg.SoftTPS && !sig.HasThinking {
		return ConsoleGuardWithhold
	}
	if consoleGuardSlowFirstTokenBurst(sig, cfg) {
		return ConsoleGuardWithhold
	}
	if consoleGuardTerminalOnlyBurst(sig, cfg) {
		return ConsoleGuardWithhold
	}
	if sig.Terminal {
		if consoleGuardEffectiveOutputTokens(sig) <= 0 {
			return ConsoleGuardWait
		}
		return ConsoleGuardDeliver
	}
	if sig.HoldExpired && !sig.FirstVisibleObserved {
		return ConsoleGuardWithhold
	}
	return ConsoleGuardWait
}

func consoleGuardSlowFirstTokenBurst(signals ConsoleGuardSignals, cfg ConsoleGuardRuntime) bool {
	if !signals.FirstVisibleObserved {
		return false
	}
	totalTokens := consoleGuardEffectiveOutputTokens(signals) + signals.ReasoningTokens
	return signals.FirstVisibleMS > cfg.FirstTokenThresholdMS &&
		consoleGuardGenerationWindowMS(signals) < cfg.GenerationWindowThresholdMS &&
		totalTokens > cfg.MinOutputReasoningTokens
}

func consoleGuardTerminalOnlyBurst(signals ConsoleGuardSignals, cfg ConsoleGuardRuntime) bool {
	if !signals.Terminal || signals.FirstVisibleObserved {
		return false
	}
	totalTokens := consoleGuardEffectiveOutputTokens(signals) + signals.ReasoningTokens
	return signals.ObservationDurationMS > cfg.FirstTokenThresholdMS &&
		totalTokens > cfg.MinOutputReasoningTokens
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

func consoleGuardSkipReason(input Input, ownership *inferencedomain.ResponseOwnership, route modeldomain.Route, operation audit.Operation, cfg ConsoleGuardRuntime) string {
	if !cfg.Enabled {
		return "disabled"
	}
	if !input.Streaming {
		return "non_streaming"
	}
	if input.ForcedEgressNodeID != 0 {
		return "forced_egress"
	}
	if ownership != nil {
		return "response_ownership"
	}
	if input.skipQualityHold {
		return "gateway_skip_quality_hold"
	}
	switch operation {
	case audit.OperationChat, audit.OperationResponses, audit.OperationMessages, "":
	default:
		return "unsupported_operation"
	}
	if isResponsesCompactionRequest(input.Body) {
		return "responses_compaction"
	}
	// 仅 Console。
	if route.Provider != accountdomain.ProviderConsole {
		return "non_console_provider"
	}
	// 显式关闭 reasoning 的请求不检测。
	if consoleGuardRequestDisablesReasoning(input.Body) {
		return "reasoning_disabled"
	}
	if modeldomain.SupportsReasoningForProvider(route.Provider, input.PublicModel) || modeldomain.SupportsReasoningForProvider(route.Provider, route.UpstreamModel) {
		return ""
	}
	return "model_no_reasoning_support"
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
	if s.consoleGuardProxyURLResolver != nil {
		// ProxyURL 返回数据库中代理地址的解密原文；AppendUnique 原样写入明文文件。
		proxyURL, err := s.consoleGuardProxyURLResolver.ProxyURL(writeCtx, credential.EgressNodeID)
		if err != nil {
			s.logger.Warn("console_guard_proxy_file_resolve_failed", "request_id", requestID, "account_id", credential.ID, "egress_node_id", credential.EgressNodeID, "error", err)
		} else if err := consoleguardfile.AppendUnique(s.consoleGuardConfig().DegradedEgressNodeFilePath, proxyURL); err != nil {
			s.logger.Error("console_guard_proxy_file_write_failed", "request_id", requestID, "account_id", credential.ID, "egress_node_id", credential.EgressNodeID, "error", err)
		} else {
			s.logger.Info("console_guard_proxy_file_written", "request_id", requestID, "account_id", credential.ID, "egress_node_id", credential.EgressNodeID, "path", s.consoleGuardConfig().DegradedEgressNodeFilePath, "proxy_url_bytes", len(proxyURL))
		}
	}
	if err := s.selector.disableConsoleGuardAccount(writeCtx, credential); err != nil {
		s.logger.Error("console_guard_disable_failed", "request_id", requestID, "account_id", credential.ID, "error", err)
		return
	}
	s.logger.Info("console_guard_disabled", "request_id", requestID, "account_id", credential.ID, "account_name", credential.Name)
}

func (s *Service) disableConsoleNoProxyAccount(ctx context.Context, requestID string, credential accountdomain.Credential) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
	defer cancel()
	if err := s.selector.disableConsoleAccount(writeCtx, credential, lastErrorConsoleNoProxyDisabled); err != nil {
		s.logger.Error("console_no_proxy_disable_failed", "request_id", requestID, "account_id", credential.ID, "error", err)
		return
	}
	s.logger.Warn("console_no_proxy_disabled", "request_id", requestID, "account_id", credential.ID, "account_name", credential.Name)
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
	if !signals.FirstVisibleObserved {
		return 0
	}
	return audit.GenerationWindowMS(consoleGuardFirstTokenMS(signals), signals.ObservationDurationMS)
}

func consoleGuardOutputTokensPerSecond(signals ConsoleGuardSignals, usage Usage) float64 {
	if !signals.FirstVisibleObserved {
		return 0
	}
	outputTokens := consoleGuardEffectiveOutputTokens(signals)
	reasoningTokens := max(signals.ReasoningTokens, usage.ReasoningTokens)
	return audit.OutputTokensPerSecond(outputTokens, reasoningTokens, consoleGuardFirstTokenMS(signals), signals.ObservationDurationMS)
}

func buildConsoleGuardAttemptDetail(protocol string, signals ConsoleGuardSignals, usage Usage, cfg ConsoleGuardRuntime, verdict ConsoleGuardVerdict, action ConsoleGuardAction, attempt int, upstreamDurationMS int64) *audit.ConsoleGuardAttemptDetail {
	outputTokens := consoleGuardEffectiveOutputTokens(signals)
	reasoningTokens := max(signals.ReasoningTokens, usage.ReasoningTokens)
	generationWindowMS := consoleGuardGenerationWindowMS(signals)
	outputTokensPerSecond := consoleGuardOutputTokensPerSecond(signals, usage)
	totalOutputReasoningTokens := outputTokens + reasoningTokens
	tpsAvailable := signals.FirstVisibleObserved && generationWindowMS > 0
	tpsWindowMature := tpsAvailable && generationWindowMS >= cfg.GenerationWindowThresholdMS
	hardTPSExceeded := tpsWindowMature && outputTokensPerSecond > cfg.HardTPS
	softTPSExceededWithoutThinking := tpsWindowMature && outputTokensPerSecond > cfg.SoftTPS && !signals.HasThinking
	burstConditionsMet := consoleGuardSlowFirstTokenBurst(signals, cfg)
	slowFirstTokenBurst := !hardTPSExceeded && !softTPSExceededWithoutThinking && burstConditionsMet
	terminalOnlyBurst := consoleGuardTerminalOnlyBurst(signals, cfg)
	decisionReasons := make([]audit.ConsoleGuardEvidence, 0, 12)
	if signals.HasThinking {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "real_thinking_present", Detail: "观察到真实 reasoning 内容；仅在 TPS 可用且窗口成熟时豁免软阈值，不能生成 TPS，也不豁免 terminal-only burst"})
	} else {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "real_thinking_absent", Detail: "未观察到 reasoning delta、encrypted_content 或带文本的 thinking_delta；hasThinking=false"})
	}
	if signals.ReasoningStarted && len(signals.ThinkingEvidence) == 0 {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "reasoning_started_without_content", Detail: "只观察到 reasoning 起始标记或空 reasoning item，未观察到真实思考文本"})
	}
	if reasoningTokens > 0 && !signals.HasThinking {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "reasoning_tokens_not_evidence", Detail: fmt.Sprintf("reasoning tokens=%d 仅作统计，未作为真实思考证据", reasoningTokens)})
	}
	if !signals.FirstVisibleObserved {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "tps_unavailable_no_generated_delta", Detail: "本次账号 attempt 未观察到真实 generated delta；response.completed 中的最终 output/usage 不能反推出首字和生成窗口，TPS=N/A"})
	} else {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "tps_formula", Detail: fmt.Sprintf("Token/s=(output_tokens + reasoning_tokens)*1000/(duration_ms - first_token_ms)=(%d + %d)*1000/(%d - %d)=%.2f", outputTokens, reasoningTokens, signals.ObservationDurationMS, consoleGuardFirstTokenMS(signals), outputTokensPerSecond)})
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "tps_thresholds", Detail: fmt.Sprintf("softTPS=%.2f, hardTPS=%.2f, currentTPS=%.2f", cfg.SoftTPS, cfg.HardTPS, outputTokensPerSecond)})
		if generationWindowMS <= 0 && totalOutputReasoningTokens > 0 {
			decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "tps_generation_window_non_positive", Detail: fmt.Sprintf("TPS 分母 duration_ms - first_token_ms = %d - %d <= 0；尚无可计量生成窗口，TPS 保持 0", signals.ObservationDurationMS, consoleGuardFirstTokenMS(signals))})
		} else if !tpsWindowMature {
			decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "tps_window_not_mature", Detail: fmt.Sprintf("generationWindowMs=%d < %dms；展示原始 TPS，但不参与软/硬 TPS 降智判定", generationWindowMS, cfg.GenerationWindowThresholdMS)})
		} else if hardTPSExceeded {
			decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "hard_tps_exceeded", Detail: fmt.Sprintf("current TPS %.2f > hardTPS %.2f；无论是否有 thinking 都判定降智", outputTokensPerSecond, cfg.HardTPS)})
		} else if softTPSExceededWithoutThinking {
			decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "soft_tps_exceeded_without_thinking", Detail: fmt.Sprintf("current TPS %.2f > softTPS %.2f 且 hasThinking=false，判定降智", outputTokensPerSecond, cfg.SoftTPS)})
		} else if outputTokensPerSecond > cfg.SoftTPS && signals.HasThinking {
			decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "thinking_protected_by_soft_threshold", Detail: fmt.Sprintf("current TPS %.2f > softTPS %.2f 但存在真实 thinking，软阈值不触发降智；继续检查 burst 条件", outputTokensPerSecond, cfg.SoftTPS)})
		} else {
			decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "tps_within_soft_threshold", Detail: fmt.Sprintf("current TPS %.2f 未超过 softTPS %.2f；没有达到降智速度条件", outputTokensPerSecond, cfg.SoftTPS)})
		}
	}
	if signals.FirstVisibleObserved {
		firstTokenMS := consoleGuardFirstTokenMS(signals)
		firstTokenSlow := firstTokenMS > cfg.FirstTokenThresholdMS
		generationWindowShort := generationWindowMS < cfg.GenerationWindowThresholdMS
		tokenCountHigh := totalOutputReasoningTokens > cfg.MinOutputReasoningTokens
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "slow_first_token_burst_conditions", Detail: fmt.Sprintf("firstTokenMs=%d > %dms=%t；generationWindowMs=%d < %dms=%t；output+reasoning=%d > %d=%t", firstTokenMS, cfg.FirstTokenThresholdMS, firstTokenSlow, generationWindowMS, cfg.GenerationWindowThresholdMS, generationWindowShort, totalOutputReasoningTokens, cfg.MinOutputReasoningTokens, tokenCountHigh)})
		if hardTPSExceeded || softTPSExceededWithoutThinking {
			decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "slow_first_token_burst_not_evaluated", Detail: "TPS 判定已先命中，慢首字 burst 规则不作为本次最终判定"})
		} else if slowFirstTokenBurst {
			decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "slow_first_token_burst_exceeded", Detail: "软 TPS 和硬 TPS 均未命中，但慢首字 + 短生成窗口 + 高 output/reasoning token 条件同时满足，判定降智"})
		} else {
			decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "slow_first_token_burst_not_exceeded", Detail: "慢首字 burst 三项条件未同时满足，不因该规则判定降智"})
		}
	} else if signals.Terminal {
		attemptSlow := signals.ObservationDurationMS > cfg.FirstTokenThresholdMS
		tokenCountHigh := totalOutputReasoningTokens > cfg.MinOutputReasoningTokens
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "terminal_only_burst_conditions", Detail: fmt.Sprintf("未观察到 generated delta；attemptDurationMs=%d > %dms=%t；output+reasoning=%d > %d=%t", signals.ObservationDurationMS, cfg.FirstTokenThresholdMS, attemptSlow, totalOutputReasoningTokens, cfg.MinOutputReasoningTokens, tokenCountHigh)})
		if terminalOnlyBurst {
			decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "terminal_only_burst_exceeded", Detail: "仅在 terminal 事件收到最终 output/usage，且 attempt 完成慢、token 高；按 terminal-only burst 判定降智"})
		} else {
			decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "terminal_only_burst_not_exceeded", Detail: "terminal-only burst 的慢完成与高 token 条件未同时满足"})
		}
	}
	if outputTokens <= 0 {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "no_effective_output_tokens", Detail: "尚未观察到有效输出 token；等待终止或扫描结果，不据此判定降智"})
	}
	if signals.Terminal {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "terminal_observed", Detail: "已观察到终止事件：" + signals.TerminalEvent})
	} else if signals.HoldExpired {
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "hold_timeout_reached", Detail: fmt.Sprintf("首字等待超时=%dms 已到期", cfg.HoldTimeout.Milliseconds())})
	}
	switch verdict {
	case ConsoleGuardWithhold:
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "classified_withhold", Detail: "ClassifyConsoleGuardHold 判定为 withhold，扣住响应并进入降智处理"})
	case ConsoleGuardDeliver:
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "classified_deliver", Detail: "ClassifyConsoleGuardHold 判定为 deliver，保留本次响应"})
	default:
		decisionReasons = append(decisionReasons, audit.ConsoleGuardEvidence{Code: "classified_wait", Detail: "ClassifyConsoleGuardHold 尚未完成最终判定"})
	}
	return &audit.ConsoleGuardAttemptDetail{
		Protocol:                    protocol,
		Verdict:                     string(verdict),
		Action:                      string(action),
		Attempt:                     attempt,
		MaxAttempts:                 consoleGuardMaxAttempts,
		MinOutputTokens:             0,
		FirstTokenThresholdMS:       cfg.FirstTokenThresholdMS,
		GenerationWindowThresholdMS: cfg.GenerationWindowThresholdMS,
		MinOutputReasoningTokens:    cfg.MinOutputReasoningTokens,
		SoftTPS:                     cfg.SoftTPS,
		HardTPS:                     cfg.HardTPS,
		HoldTimeoutMS:               cfg.HoldTimeout.Milliseconds(),
		HasThinking:                 signals.HasThinking,
		ThinkingEvidence:            append([]audit.ConsoleGuardEvidence{}, signals.ThinkingEvidence...),
		ReasoningStarted:            signals.ReasoningStarted,
		ReasoningStartEvidence:      append([]audit.ConsoleGuardEvidence{}, signals.ReasoningStartEvidence...),
		VisibleRunes:                signals.VisibleRunes,
		VisibleTokens:               signals.VisibleTokens,
		OutputTokens:                outputTokens,
		ReasoningTokens:             reasoningTokens,
		UsageReported:               usage.Reported,
		UsageInputTokens:            usage.InputTokens,
		UsageOutputTokens:           usage.OutputTokens,
		UsageReasoningTokens:        usage.ReasoningTokens,
		UsageTotalTokens:            usage.TotalTokens,
		Terminal:                    signals.Terminal,
		TerminalEvent:               signals.TerminalEvent,
		HoldExpired:                 signals.HoldExpired,
		ObservationDurationMS:       signals.ObservationDurationMS,
		UpstreamDurationMS:          upstreamDurationMS,
		FirstVisibleObserved:        signals.FirstVisibleObserved,
		FirstVisibleMS:              signals.FirstVisibleMS,
		GenerationWindowMS:          generationWindowMS,
		OutputTokensPerSecond:       outputTokensPerSecond,
		AccountDisabled:             verdict == ConsoleGuardWithhold,
		DecisionReasons:             decisionReasons,
	}
}

func buildConsoleGuardDetail(protocol, skipReason string, cfg ConsoleGuardRuntime, attempts []audit.ConsoleGuardAttemptDetail, degraded bool) *audit.ConsoleGuardDetail {
	if strings.TrimSpace(skipReason) == "" && len(attempts) == 0 {
		skipReason = "request_not_forwarded"
	}
	detail := &audit.ConsoleGuardDetail{
		Degraded:   degraded,
		SkipReason: skipReason,
		Attempts:   append([]audit.ConsoleGuardAttemptDetail{}, attempts...),
	}
	if len(attempts) > 0 {
		detail.ConsoleGuardAttemptDetail = attempts[len(attempts)-1]
		return detail
	}
	detail.ConsoleGuardAttemptDetail = audit.ConsoleGuardAttemptDetail{
		Protocol:               protocol,
		Verdict:                "skipped",
		Action:                 string(ConsoleGuardActionDeliver),
		MaxAttempts:            consoleGuardMaxAttempts,
		SoftTPS:                cfg.SoftTPS,
		HardTPS:                cfg.HardTPS,
		HoldTimeoutMS:          cfg.HoldTimeout.Milliseconds(),
		ThinkingEvidence:       []audit.ConsoleGuardEvidence{},
		ReasoningStartEvidence: []audit.ConsoleGuardEvidence{},
		DecisionReasons: []audit.ConsoleGuardEvidence{{
			Code:   "guard_skipped",
			Detail: "本次 Console 请求未进入流扫描：" + skipReason,
		}},
	}
	return detail
}
