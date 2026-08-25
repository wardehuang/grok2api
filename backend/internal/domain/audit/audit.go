package audit

import "time"

type Operation string

const (
	OperationResponses  Operation = "responses"
	OperationCompaction Operation = "compaction"
	OperationChat       Operation = "chat"
	OperationMessages   Operation = "messages"
	OperationImage      Operation = "image"
	OperationImageEdit  Operation = "image_edit"
	OperationVideo      Operation = "video"
	OperationTTS        Operation = "tts"
	OperationSTT        Operation = "stt"
	OperationRealtime   Operation = "realtime"
	OperationVoice      Operation = "voice"
)

type UsageSource string

const (
	UsageSourceUpstream  UsageSource = "upstream"
	UsageSourceEstimated UsageSource = "estimated"
	UsageSourceNone      UsageSource = "none"
)

type AttemptSource string

const (
	AttemptSourceUpstreamHTTP AttemptSource = "upstream_http"
	AttemptSourceTransport    AttemptSource = "gateway_transport"
	AttemptSourceCredential   AttemptSource = "credential"
)

type ErrorFrame struct {
	Type    string
	Message string
}

// ConsoleGuardEvidence 是 Console 降智判定使用的结构化证据。
// Code 供前端稳定翻译；Detail 保留本次流的具体观察结果。
type ConsoleGuardEvidence struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// ConsoleGuardAttemptDetail 保存一次 Console 流扫描的完整判定快照。
// 不保存请求正文或响应正文，只保存流扫描器的统计与判定证据。
type ConsoleGuardAttemptDetail struct {
	Protocol    string  `json:"protocol"`
	Verdict     string  `json:"verdict"`
	Action      string  `json:"action"`
	Attempt     int     `json:"attempt"`
	MaxAttempts int     `json:"maxAttempts"`
	AccountID   string  `json:"accountId,omitempty"`
	AccountName string  `json:"accountName,omitempty"`
	SoftTPS     float64 `json:"softTPS"`
	HardTPS     float64 `json:"hardTPS"`
	// MinOutputTokens 保留旧审计快照兼容；Console Guard 新判定不再使用它。
	MinOutputTokens             int64                  `json:"minOutputTokens"`
	FirstTokenThresholdMS       int64                  `json:"firstTokenThresholdMs"`
	GenerationWindowThresholdMS int64                  `json:"generationWindowThresholdMs"`
	MinOutputReasoningTokens    int64                  `json:"minOutputReasoningTokens"`
	HoldTimeoutMS               int64                  `json:"holdTimeoutMs"`
	HasThinking                 bool                   `json:"hasThinking"`
	ThinkingEvidence            []ConsoleGuardEvidence `json:"thinkingEvidence"`
	ReasoningStarted            bool                   `json:"reasoningStarted"`
	ReasoningStartEvidence      []ConsoleGuardEvidence `json:"reasoningStartEvidence"`
	VisibleRunes                int64                  `json:"visibleRunes"`
	VisibleTokens               int64                  `json:"visibleTokens"`
	OutputTokens                int64                  `json:"outputTokens"`
	ReasoningTokens             int64                  `json:"reasoningTokens"`
	UsageReported               bool                   `json:"usageReported"`
	UsageInputTokens            int64                  `json:"usageInputTokens"`
	UsageOutputTokens           int64                  `json:"usageOutputTokens"`
	UsageReasoningTokens        int64                  `json:"usageReasoningTokens"`
	UsageTotalTokens            int64                  `json:"usageTotalTokens"`
	Terminal                    bool                   `json:"terminal"`
	TerminalEvent               string                 `json:"terminalEvent"`
	HoldExpired                 bool                   `json:"holdExpired"`
	ObservationDurationMS       int64                  `json:"observationDurationMs"`
	UpstreamDurationMS          int64                  `json:"upstreamDurationMs"`
	// Legacy JSON names retained; values use the same generated-delta first-token
	// timing as the main request audit.
	FirstVisibleObserved  bool                   `json:"firstVisibleObserved"`
	FirstVisibleMS        int64                  `json:"firstVisibleMs"`
	GenerationWindowMS    int64                  `json:"generationWindowMs"`
	OutputTokensPerSecond float64                `json:"outputTokensPerSecond"`
	AccountDisabled       bool                   `json:"accountDisabled"`
	DecisionReasons       []ConsoleGuardEvidence `json:"decisionReasons"`
}

// ConsoleGuardDetail 是一条 Console 请求对应的唯一降智事件详情。
// Attempts 保存本次请求的每次扫描；顶层字段是最后一次扫描的快照，保持旧审计详情兼容。
type ConsoleGuardDetail struct {
	ConsoleGuardAttemptDetail
	Degraded   bool                        `json:"degraded"`
	SkipReason string                      `json:"skipReason,omitempty"`
	Attempts   []ConsoleGuardAttemptDetail `json:"attempts"`
}

// Attempt 保存一次失败尝试经过裁剪和脱敏的管理员诊断快照。
type Attempt struct {
	ID                    uint64
	AuditID               uint64
	Number                int
	Source                AttemptSource
	Stage                 string
	AccountID             *uint64
	AccountName           string
	Method                string
	RequestPath           string
	UpstreamURL           string
	StartedAt             time.Time
	DurationMS            int64
	UpstreamStatusCode    *int
	UpstreamStatus        string
	ResponseHeaders       map[string][]string
	ResponseBody          []byte
	ResponseBodyTruncated bool
	TransportError        string
	ErrorChain            []ErrorFrame
}

type EgressMode string

const (
	EgressModeDirect EgressMode = "direct"
	EgressModeProxy  EgressMode = "proxy"
)

// Record 表示推理请求审计；成功请求不保存正文，失败请求仅保留受限诊断快照。
type Record struct {
	ID                      uint64
	EventID                 string
	RequestID               string
	ClientKeyID             uint64
	ClientKeyName           string
	ClientIP                string
	ModelRouteID            uint64
	ModelPublicID           string
	ModelUpstreamModel      string
	Provider                string
	Operation               Operation
	UsageSource             UsageSource
	ReasoningEffort         string
	AccountID               *uint64
	AccountName             string
	EgressNodeID            *uint64
	EgressNodeName          string
	EgressScope             string
	EgressMode              EgressMode
	StatusCode              int
	Streaming               bool
	MediaInputImages        int64
	MediaOutputImages       int64
	MediaOutputSeconds      int64
	InputTokens             int64
	CachedInputTokens       int64
	OutputTokens            int64
	ReasoningTokens         int64
	TotalTokens             int64
	CostInUSDTicks          int64
	EstimatedCostInUSDTicks int64
	PricingModel            string
	PricingVersion          string
	NumSourcesUsed          int64
	NumServerSideToolsUsed  int64
	ContextInputTokens      int64
	ContextOutputTokens     int64
	FirstTokenMS            *int64
	DurationMS              int64
	ErrorCode               string
	AttemptCount            int
	Attempts                []Attempt
	ConsoleGuard            *ConsoleGuardDetail
	CreatedAt               time.Time
}

// Summary 表示指定审计范围内的聚合用量。
type Summary struct {
	Requests                int64
	SuccessfulRequests      int64
	FailedRequests          int64
	InputTokens             int64
	CachedInputTokens       int64
	OutputTokens            int64
	ReasoningTokens         int64
	TotalTokens             int64
	DurationMS              int64
	EstimatedCostInUSDTicks int64
	PricedRequests          int64
	UnpricedRequests        int64
	PricedTokens            int64
	UnpricedTokens          int64
}
