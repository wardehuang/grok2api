package gateway

// console_guard_scan.go 是从 fork（lij768423-svg/grok2api，commit 8955728）的
// quality_retry_scan.go 忠实复制的扫描器实现，仅做前缀重命名（quality→consoleGuard）。
// 判定口径与 fork 一致：
//   - usage.reasoning_tokens > 0 不算思考（降智账号会虚报）
//   - ": grok2api-reasoning-start" stub / 空 reasoning item 只标记 ReasoningStarted
//   - 只有真实 reasoning delta / encrypted_content / thinking 文本算 HasThinking
//   - 收到 terminal（response.completed / [DONE] / message_stop）立即结算
//
// 本文件不依赖也不修改 quality_retry*.go 的任何符号。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

const (
	consoleGuardProtocolChat      = "chat"
	consoleGuardProtocolResponses = "responses"
	consoleGuardProtocolAnthropic = "anthropic"

	consoleGuardReasoningSSEComment = ": grok2api-reasoning-start"
	consoleGuardMaxBufferBytes      = 4 << 20
)

type consoleGuardScanState struct {
	protocol               string
	pending                []byte
	hasThinking            bool
	reasoningStarted       bool
	visibleRunes           int
	reasoningTokens        int64
	outputTokens           int64
	usage                  Usage
	responseID             string
	terminal               bool
	terminalEvent          string
	thinkingEvidence       []audit.ConsoleGuardEvidence
	reasoningStartEvidence []audit.ConsoleGuardEvidence
	startedAt              time.Time
	firstVisibleObserved   bool
	firstVisibleMS         int64
}

type consoleGuardReadResult struct {
	data []byte
	err  error
}

type consoleGuardNoDataWatch struct {
	timer    *time.Timer
	cancel   context.CancelCauseFunc
	stopOnce sync.Once
}

func newConsoleGuardNoDataWatch(parent context.Context) (context.Context, *consoleGuardNoDataWatch) {
	attemptCtx, cancel := context.WithCancelCause(parent)
	watch := &consoleGuardNoDataWatch{cancel: cancel}
	watch.timer = time.AfterFunc(consoleGuardNoDataTimeout, func() {
		cancel(errConsoleGuardNoDataTimeout)
	})
	return attemptCtx, watch
}

func (w *consoleGuardNoDataWatch) markFirstByte() {
	w.stopOnce.Do(func() {
		w.timer.Stop()
	})
}

func (w *consoleGuardNoDataWatch) cancelAttempt() {
	w.stopOnce.Do(func() {
		w.timer.Stop()
	})
	w.cancel(nil)
}

type consoleGuardNoDataReadCloser struct {
	io.ReadCloser
	watch     *consoleGuardNoDataWatch
	closeOnce sync.Once
}

func (r *consoleGuardNoDataReadCloser) Read(buffer []byte) (int, error) {
	n, err := r.ReadCloser.Read(buffer)
	if n > 0 {
		r.watch.markFirstByte()
	}
	return n, err
}

func (r *consoleGuardNoDataReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.closeOnce.Do(r.watch.cancelAttempt)
	return err
}

// consoleGuardReadPump 是上游 body 的唯一读者。扣流计时器可以在上游 Read 阻塞时
// 获胜；放行后它继续作为响应体的续读通道。
type consoleGuardReadPump struct {
	source    io.ReadCloser
	results   chan consoleGuardReadResult
	done      chan struct{}
	closeOnce sync.Once
	pending   []byte
	finalErr  error
}

func newConsoleGuardReadPump(source io.ReadCloser) *consoleGuardReadPump {
	pump := &consoleGuardReadPump{
		source:  source,
		results: make(chan consoleGuardReadResult),
		done:    make(chan struct{}),
	}
	go pump.run()
	return pump
}

func (p *consoleGuardReadPump) run() {
	defer close(p.results)
	buf := make([]byte, 4096)
	for {
		n, err := p.source.Read(buf)
		if n == 0 && err == nil {
			continue
		}
		result := consoleGuardReadResult{err: err}
		if n > 0 {
			result.data = append([]byte(nil), buf[:n]...)
		}
		select {
		case p.results <- result:
		case <-p.done:
			return
		}
		if err != nil {
			return
		}
	}
}

func (p *consoleGuardReadPump) Read(dst []byte) (int, error) {
	for len(p.pending) == 0 {
		if p.finalErr != nil {
			return 0, p.finalErr
		}
		result, ok := <-p.results
		if !ok {
			p.finalErr = io.EOF
			return 0, io.EOF
		}
		p.pending = result.data
		p.finalErr = result.err
		if len(p.pending) == 0 && p.finalErr != nil {
			return 0, p.finalErr
		}
	}
	n := copy(dst, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}

func (p *consoleGuardReadPump) Close() error {
	var err error
	p.closeOnce.Do(func() {
		close(p.done)
		err = p.source.Close()
	})
	return err
}

func consoleGuardProtocolForOperation(operation audit.Operation) string {
	switch operation {
	case audit.OperationChat:
		return consoleGuardProtocolChat
	case audit.OperationMessages:
		return consoleGuardProtocolAnthropic
	default:
		return consoleGuardProtocolResponses
	}
}

func (s *consoleGuardScanState) signals() ConsoleGuardSignals {
	visible := int64((s.visibleRunes + 3) / 4)
	if s.usage.Reported {
		fromUsage := s.usage.OutputTokens - s.usage.ReasoningTokens
		if fromUsage > visible {
			visible = fromUsage
		}
	}
	output := s.outputTokens
	if s.usage.Reported && s.usage.OutputTokens > output {
		output = s.usage.OutputTokens
	}
	observationDurationMS := int64(0)
	if !s.startedAt.IsZero() {
		observationDurationMS = max(0, time.Since(s.startedAt).Milliseconds())
	}
	// Usage.reasoning_tokens 不是思考证据：降智账号在 completed 里虚报几百
	// reasoning token，但流里从未发过 reasoning_text / summary delta。
	return ConsoleGuardSignals{
		HasThinking:            s.hasThinking,
		ReasoningStarted:       s.reasoningStarted || s.hasThinking,
		VisibleTokens:          visible,
		ReasoningTokens:        max(s.reasoningTokens, s.usage.ReasoningTokens),
		OutputTokens:           output,
		Terminal:               s.terminal,
		ThinkingEvidence:       append([]audit.ConsoleGuardEvidence{}, s.thinkingEvidence...),
		ReasoningStartEvidence: append([]audit.ConsoleGuardEvidence{}, s.reasoningStartEvidence...),
		TerminalEvent:          s.terminalEvent,
		VisibleRunes:           int64(s.visibleRunes),
		ObservationDurationMS:  observationDurationMS,
		FirstVisibleObserved:   s.firstVisibleObserved,
		FirstVisibleMS:         s.firstVisibleMS,
	}
}

func noteConsoleGuardFirstToken(state *consoleGuardScanState) {
	if state == nil || state.firstVisibleObserved {
		return
	}
	state.firstVisibleObserved = true
	if !state.startedAt.IsZero() {
		state.firstVisibleMS = max(0, time.Since(state.startedAt).Milliseconds())
	}
}

func appendConsoleGuardEvidence(target *[]audit.ConsoleGuardEvidence, code, detail string) {
	for _, existing := range *target {
		if existing.Code == code {
			return
		}
	}
	*target = append(*target, audit.ConsoleGuardEvidence{Code: code, Detail: detail})
}

func setConsoleGuardTerminal(state *consoleGuardScanState, event string) {
	state.terminal = true
	if state.terminalEvent == "" {
		state.terminalEvent = event
	}
}

// ObserveConsoleGuardChunk 把一个 SSE chunk 喂给判定状态机。
func ObserveConsoleGuardChunk(state *consoleGuardScanState, chunk []byte) {
	if state == nil || len(chunk) == 0 {
		return
	}
	state.pending = append(state.pending, chunk...)
	for {
		index := bytes.IndexByte(state.pending, '\n')
		if index < 0 {
			if len(state.pending) > 1<<20 {
				state.pending = nil
			}
			return
		}
		line := bytes.TrimSpace(state.pending[:index])
		state.pending = state.pending[index+1:]
		if len(line) == 0 {
			continue
		}
		if bytes.Equal(line, []byte(consoleGuardReasoningSSEComment)) {
			// 计时 stub。降智流同样会发这个 stub，随后 reasoning_tokens=0 或虚报。
			state.reasoningStarted = true
			appendConsoleGuardEvidence(&state.reasoningStartEvidence, "sse.reasoning_start_stub", "观察到 Console reasoning 起始注释，但没有真实 reasoning 文本")
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(payload, []byte("[DONE]")) {
			setConsoleGuardTerminal(state, "sse.done")
			continue
		}
		observeConsoleGuardPayload(state, payload)
	}
}

func observeConsoleGuardPayload(state *consoleGuardScanState, payload []byte) {
	switch state.protocol {
	case consoleGuardProtocolChat:
		observeConsoleGuardChat(state, payload)
	case consoleGuardProtocolAnthropic:
		observeConsoleGuardAnthropic(state, payload)
	default:
		observeConsoleGuardResponses(state, payload)
	}
}

func observeConsoleGuardChat(state *consoleGuardScanState, payload []byte) {
	var event struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				Reasoning        string `json:"reasoning"`
				ReasoningContent string `json:"reasoning_content"`
				ThinkingContent  string `json:"thinking_content"`
				Refusal          string `json:"refusal"`
				ToolCalls        []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens            int64 `json:"prompt_tokens"`
			CompletionTokens        int64 `json:"completion_tokens"`
			TotalTokens             int64 `json:"total_tokens"`
			CompletionTokensDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	if state.responseID == "" {
		state.responseID = event.ID
	}
	if event.Usage != nil {
		state.usage.Reported = true
		state.usage.InputTokens = event.Usage.PromptTokens
		state.usage.OutputTokens = event.Usage.CompletionTokens
		state.usage.ReasoningTokens = event.Usage.CompletionTokensDetails.ReasoningTokens
		state.usage.TotalTokens = event.Usage.TotalTokens
		state.usage.ResponseModel = event.Model
		state.outputTokens = event.Usage.CompletionTokens
		state.reasoningTokens = event.Usage.CompletionTokensDetails.ReasoningTokens
	}
	for _, choice := range event.Choices {
		delta := choice.Delta
		if delta.Reasoning != "" {
			noteConsoleGuardFirstToken(state)
			state.hasThinking = true
			appendConsoleGuardEvidence(&state.thinkingEvidence, "chat.reasoning_delta", "delta.reasoning contains non-empty text")
		}
		if delta.ReasoningContent != "" {
			noteConsoleGuardFirstToken(state)
			state.hasThinking = true
			appendConsoleGuardEvidence(&state.thinkingEvidence, "chat.reasoning_content_delta", "delta.reasoning_content contains non-empty text")
		}
		if delta.ThinkingContent != "" {
			noteConsoleGuardFirstToken(state)
			state.hasThinking = true
			appendConsoleGuardEvidence(&state.thinkingEvidence, "chat.thinking_content_delta", "delta.thinking_content contains non-empty text")
		}
		if delta.Content != "" {
			noteConsoleGuardVisibleContent(state, delta.Content)
		}
		if delta.Refusal != "" {
			noteConsoleGuardFirstToken(state)
		}
		for _, call := range delta.ToolCalls {
			if call.Function.Arguments != "" {
				noteConsoleGuardFirstToken(state)
			}
		}
		if choice.FinishReason != "" {
			setConsoleGuardTerminal(state, "chat.finish_reason:"+choice.FinishReason)
		}
	}
}

type consoleGuardReasoningItem struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	EncryptedContent string `json:"encrypted_content"`
}

func noteConsoleGuardReasoningItem(state *consoleGuardScanState, item consoleGuardReasoningItem) {
	if !strings.EqualFold(strings.TrimSpace(item.Type), "reasoning") {
		return
	}
	if strings.TrimSpace(item.ID) != "" {
		noteConsoleGuardFirstToken(state)
		state.reasoningStarted = true
		appendConsoleGuardEvidence(&state.reasoningStartEvidence, "responses.reasoning_item", "reasoning output item has a non-empty ID")
	}
	if strings.TrimSpace(item.EncryptedContent) != "" {
		state.hasThinking = true
		appendConsoleGuardEvidence(&state.thinkingEvidence, "responses.encrypted_content", "reasoning item has non-empty encrypted_content")
	}
}

func observeConsoleGuardResponses(state *consoleGuardScanState, payload []byte) {
	var event struct {
		Type     string `json:"type"`
		Delta    string `json:"delta"`
		Item     consoleGuardReasoningItem
		Response *struct {
			ID     string                      `json:"id"`
			Model  string                      `json:"model"`
			Output []consoleGuardReasoningItem `json:"output"`
			Usage  *struct {
				OutputTokens        int64 `json:"output_tokens"`
				InputTokens         int64 `json:"input_tokens"`
				TotalTokens         int64 `json:"total_tokens"`
				OutputTokensDetails struct {
					ReasoningTokens int64 `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	switch event.Type {
	case "response.completed", "response.incomplete", "response.failed":
		setConsoleGuardTerminal(state, event.Type)
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if event.Delta != "" {
			noteConsoleGuardFirstToken(state)
			state.hasThinking = true
			appendConsoleGuardEvidence(&state.thinkingEvidence, "responses."+event.Type, "reasoning event delta contains non-empty text")
		}
	case "response.output_item.added", "response.output_item.done":
		noteConsoleGuardReasoningItem(state, event.Item)
	case "response.output_text.delta":
		if event.Delta != "" {
			noteConsoleGuardVisibleContent(state, event.Delta)
		}
	case "response.refusal.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		if event.Delta != "" {
			noteConsoleGuardFirstToken(state)
		}
	}
	if event.Response != nil {
		if state.responseID == "" {
			state.responseID = event.Response.ID
		}
		for _, item := range event.Response.Output {
			noteConsoleGuardReasoningItem(state, item)
		}
		if event.Response.Usage != nil {
			state.usage.Reported = true
			state.usage.InputTokens = event.Response.Usage.InputTokens
			state.usage.OutputTokens = event.Response.Usage.OutputTokens
			state.usage.ReasoningTokens = event.Response.Usage.OutputTokensDetails.ReasoningTokens
			state.usage.TotalTokens = event.Response.Usage.TotalTokens
			state.usage.ResponseModel = event.Response.Model
			state.outputTokens = event.Response.Usage.OutputTokens
			state.reasoningTokens = event.Response.Usage.OutputTokensDetails.ReasoningTokens
		}
	}
}

func observeConsoleGuardAnthropic(state *consoleGuardScanState, payload []byte) {
	var event struct {
		Type         string `json:"type"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
		} `json:"delta"`
		Usage *struct {
			OutputTokens        int64 `json:"output_tokens"`
			OutputTokensDetails struct {
				ThinkingTokens int64 `json:"thinking_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	switch event.Type {
	case "message_stop":
		setConsoleGuardTerminal(state, "anthropic.message_stop")
	case "content_block_start":
		if event.ContentBlock.Type == "thinking" {
			noteConsoleGuardFirstToken(state)
			state.reasoningStarted = true
			appendConsoleGuardEvidence(&state.reasoningStartEvidence, "anthropic.thinking_block", "content block type is thinking")
		}
	case "content_block_delta":
		if event.Delta.Type == "thinking_delta" && event.Delta.Thinking != "" {
			noteConsoleGuardFirstToken(state)
			state.hasThinking = true
			appendConsoleGuardEvidence(&state.thinkingEvidence, "anthropic.thinking_delta", "thinking_delta contains non-empty text")
		}
		if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
			noteConsoleGuardVisibleContent(state, event.Delta.Text)
		}
		if event.Delta.Type == "input_json_delta" && event.Delta.PartialJSON != "" {
			noteConsoleGuardFirstToken(state)
		}
	}
	if event.Usage != nil {
		state.usage.Reported = true
		state.usage.OutputTokens = event.Usage.OutputTokens
		state.usage.ReasoningTokens = event.Usage.OutputTokensDetails.ThinkingTokens
		state.outputTokens = event.Usage.OutputTokens
		state.reasoningTokens = event.Usage.OutputTokensDetails.ThinkingTokens
	}
}

func noteConsoleGuardVisibleContent(state *consoleGuardScanState, text string) {
	if text == "" {
		return
	}
	noteConsoleGuardFirstToken(state)
	state.visibleRunes += utf8.RuneCountInString(text)
}

func peekConsoleGuardStream(ctx context.Context, body io.ReadCloser, protocol string, cfg ConsoleGuardRuntime, requestStartedAt time.Time) (io.ReadCloser, ConsoleGuardVerdict, Usage, ConsoleGuardSignals, error) {
	cfg = normalizeConsoleGuard(cfg)
	if body == nil {
		return io.NopCloser(bytes.NewReader(nil)), ConsoleGuardWait, Usage{}, ConsoleGuardSignals{}, errConsoleGuardEmptyStream
	}
	pump := newConsoleGuardReadPump(body)
	state := consoleGuardScanState{protocol: protocol, startedAt: requestStartedAt}
	var held bytes.Buffer
	holdTimer := time.NewTimer(cfg.HoldTimeout)
	defer holdTimer.Stop()
	for {
		sig := state.signals()
		if verdict := ClassifyConsoleGuardHold(sig, cfg); verdict != ConsoleGuardWait {
			return newConsoleGuardPrefixReplay(&held, pump), verdict, state.usage, sig, nil
		}
		// 已 terminal 的空流必须立即轮换：在 response.completed / [DONE] 之后
		// 继续等 idle timeout 会把 HTTP 200 + 0 token 暴露给下游。
		if sig.Terminal {
			return finishConsoleGuardPeek(&held, pump, &state, cfg)
		}

		select {
		case <-ctx.Done():
			_ = pump.Close()
			return io.NopCloser(bytes.NewReader(held.Bytes())), ConsoleGuardWait, state.usage, sig, consoleGuardPeekAbortError(ctx, ctx.Err())
		case <-holdTimer.C:
			sig.HoldExpired = true
			if verdict := ClassifyConsoleGuardHold(sig, cfg); verdict != ConsoleGuardWait {
				return newConsoleGuardPrefixReplay(&held, pump), verdict, state.usage, sig, nil
			}
		case result, ok := <-pump.results:
			if !ok {
				return finishConsoleGuardPeek(&held, pump, &state, cfg)
			}
			if len(result.data) > 0 {
				if held.Len()+len(result.data) > consoleGuardMaxBufferBytes {
					_, _ = held.Write(result.data)
					return newConsoleGuardPrefixReplay(&held, pump), ConsoleGuardDeliver, state.usage, sig, nil
				}
				_, _ = held.Write(result.data)
				ObserveConsoleGuardChunk(&state, result.data)
			}
			if result.err == io.EOF {
				return finishConsoleGuardPeek(&held, pump, &state, cfg)
			}
			if result.err != nil {
				_ = pump.Close()
				return io.NopCloser(bytes.NewReader(held.Bytes())), ConsoleGuardWait, state.usage, sig, consoleGuardPeekAbortError(ctx, result.err)
			}
		}
	}
}

func finishConsoleGuardPeek(held *bytes.Buffer, pump *consoleGuardReadPump, state *consoleGuardScanState, cfg ConsoleGuardRuntime) (io.ReadCloser, ConsoleGuardVerdict, Usage, ConsoleGuardSignals, error) {
	if state == nil {
		return io.NopCloser(bytes.NewReader(nil)), ConsoleGuardWait, Usage{}, ConsoleGuardSignals{}, errConsoleGuardEmptyStream
	}
	if len(state.pending) > 0 {
		// 上游漏掉末尾换行时也要处理最后一条合法 SSE data 行。
		ObserveConsoleGuardChunk(state, []byte{'\n'})
	}
	setConsoleGuardTerminal(state, "stream.eof")
	signals := state.signals()
	if !signals.HasThinking && signals.ReasoningTokens <= 0 && signals.OutputTokens <= 0 && signals.VisibleTokens <= 0 {
		return newConsoleGuardPrefixReplay(held, pump), ConsoleGuardWait, state.usage, signals, errConsoleGuardEmptyStream
	}
	return newConsoleGuardPrefixReplay(held, pump), ClassifyConsoleGuardHold(signals, cfg), state.usage, signals, nil
}

func newConsoleGuardPrefixReplay(held *bytes.Buffer, rest io.ReadCloser) io.ReadCloser {
	if rest == nil {
		rest = io.NopCloser(bytes.NewReader(nil))
	}
	if held == nil || held.Len() == 0 {
		return rest
	}
	return &consoleGuardReplayReadCloser{Reader: io.MultiReader(bytes.NewReader(held.Bytes()), rest), source: rest}
}

type consoleGuardReplayReadCloser struct {
	io.Reader
	source io.ReadCloser
}

func (r *consoleGuardReplayReadCloser) Close() error { return r.source.Close() }
