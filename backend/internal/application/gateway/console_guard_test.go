package gateway

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

// 测试移植自 fork quality_retry_test.go 的判定/扫描用例，适配 console guard
// 的固定参数（hold 30s、TPS 双阈值、fail_closed、命中即停用）。

func consoleGuardTestRoute(provider accountdomain.Provider) modeldomain.Route {
	return modeldomain.Route{Provider: provider, UpstreamModel: "grok-4.6", PublicID: "grok-4.6"}
}

const (
	consoleGuardTestSoftTPS = 500.0
	consoleGuardTestHardTPS = 1000.0
)

var consoleGuardTestCfg = ConsoleGuardRuntime{Enabled: true, SoftTPS: consoleGuardTestSoftTPS, HardTPS: consoleGuardTestHardTPS, FirstTokenThresholdMS: 5000, GenerationWindowThresholdMS: 1250, MinOutputReasoningTokens: 300, HoldTimeout: time.Second}

func TestClassifyConsoleGuardHold(t *testing.T) {
	t.Parallel()
	if got := ClassifyConsoleGuardHold(ConsoleGuardSignals{HasThinking: true}, consoleGuardTestCfg); got != ConsoleGuardWait {
		t.Fatalf("thinking without terminal must keep scanning, got %s", got)
	}
	hardTPSWithThinking := ConsoleGuardSignals{HasThinking: true, FirstVisibleObserved: true, FirstVisibleMS: 1, ObservationDurationMS: 2, OutputTokens: 2}
	if got := ClassifyConsoleGuardHold(hardTPSWithThinking, consoleGuardTestCfg); got != ConsoleGuardWithhold {
		t.Fatalf("hard TPS must withhold even with thinking, got %s", got)
	}
	if ClassifyConsoleGuardHold(ConsoleGuardSignals{ReasoningTokens: 60, OutputTokens: 90, ObservationDurationMS: 50}, consoleGuardTestCfg) != ConsoleGuardWithhold {
		t.Fatal("usage-only reasoning with no streamed thinking must withhold")
	}
	if got := ClassifyConsoleGuardHold(ConsoleGuardSignals{Terminal: true, VisibleTokens: 4, ObservationDurationMS: 50}, consoleGuardTestCfg); got != ConsoleGuardDeliver {
		t.Fatalf("short reply must deliver, got %s", got)
	}
	if got := ClassifyConsoleGuardHold(ConsoleGuardSignals{Terminal: true, VisibleTokens: 40, ObservationDurationMS: 50}, consoleGuardTestCfg); got != ConsoleGuardWithhold {
		t.Fatalf("finished no-think sample must withhold, got %s", got)
	}
	if got := ClassifyConsoleGuardHold(ConsoleGuardSignals{Terminal: true}, consoleGuardTestCfg); got != ConsoleGuardWait {
		t.Fatalf("empty terminal must wait for empty-stream classification, got %s", got)
	}
	stub := ConsoleGuardSignals{ReasoningStarted: true, VisibleTokens: 40, ObservationDurationMS: 50}
	if got := ClassifyConsoleGuardHold(stub, consoleGuardTestCfg); got != ConsoleGuardWait {
		t.Fatalf("stub without terminal must keep waiting for usage, got %s", got)
	}
	if got := ClassifyConsoleGuardHold(stub.withTerminal(), consoleGuardTestCfg); got != ConsoleGuardWithhold {
		t.Fatalf("stub + terminal + enough output must withhold, got %s", got)
	}
	burst := ConsoleGuardSignals{Terminal: true, FirstVisibleObserved: true, FirstVisibleMS: 5001, ObservationDurationMS: 6250, OutputTokens: 301}
	if got := ClassifyConsoleGuardHold(burst, consoleGuardTestCfg); got != ConsoleGuardWithhold {
		t.Fatalf("slow first token burst must withhold, got %s", got)
	}
}

func (s ConsoleGuardSignals) withTerminal() ConsoleGuardSignals {
	s.Terminal = true
	return s
}

func TestDecideAndCommitConsoleGuardHold(t *testing.T) {
	t.Parallel()
	const maxAttempts = consoleGuardMaxAttempts
	for index := 0; index < maxAttempts-1; index++ {
		if got := DecideConsoleGuardRetry(ConsoleGuardWithhold, index, maxAttempts); got != ConsoleGuardActionRetry {
			t.Fatalf("attempt %d action = %s, want retry", index, got)
		}
	}
	// 最后一个 attempt 也扣住：恒 fail_closed 拒绝。
	if got := DecideConsoleGuardRetry(ConsoleGuardWithhold, maxAttempts-1, maxAttempts); got != ConsoleGuardActionReject {
		t.Fatalf("last withhold action = %s, want reject", got)
	}
	if got := BoundConsoleGuardRetry(ConsoleGuardActionRetry, false); got != ConsoleGuardActionReject {
		t.Fatalf("exhausted routing must reject, got %s", got)
	}
	if got := BoundConsoleGuardRetry(ConsoleGuardActionRetry, true); got != ConsoleGuardActionRetry {
		t.Fatalf("available routing must retry, got %s", got)
	}
	rejectCommit := CommitConsoleGuardHold(ConsoleGuardWithhold, maxAttempts-1, maxAttempts, true)
	if !rejectCommit.Audit || rejectCommit.KeepBody {
		t.Fatalf("reject commit = %#v", rejectCommit)
	}
	deliverCommit := CommitConsoleGuardHold(ConsoleGuardDeliver, 0, maxAttempts, true)
	if deliverCommit.Audit || !deliverCommit.KeepBody || deliverCommit.Action != ConsoleGuardActionDeliver {
		t.Fatalf("deliver commit = %#v", deliverCommit)
	}
}

func TestObserveConsoleGuardChunkUsageOnlyIsNotThinking(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("word ", 40)
	state := consoleGuardScanState{protocol: consoleGuardProtocolChat}
	ObserveConsoleGuardChunk(&state, []byte(sse(
		": grok2api-reasoning-start",
		`data: {"choices":[{"delta":{"content":"`+content+`"}}]}`,
		`data: {"usage":{"completion_tokens":45,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"data: [DONE]",
	)))
	sig := state.signals()
	if sig.HasThinking {
		t.Fatalf("chat stub + reasoning_tokens=0 must not count as thinking: %#v", sig)
	}
	if !sig.ReasoningStarted || !sig.Terminal {
		t.Fatalf("chat stub signals = %#v", sig)
	}
	if ClassifyConsoleGuardHold(sig, consoleGuardTestCfg) != ConsoleGuardWithhold {
		t.Fatalf("degraded dump must withhold: %#v", sig)
	}

	fake := consoleGuardScanState{protocol: consoleGuardProtocolResponses}
	ObserveConsoleGuardChunk(&fake, []byte(sse(
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":90,"output_tokens_details":{"reasoning_tokens":60}}}}`,
	)))
	fakeSig := fake.signals()
	if fakeSig.HasThinking {
		t.Fatalf("usage.reasoning_tokens alone must not prove thinking: %#v", fakeSig)
	}
	if ClassifyConsoleGuardHold(fakeSig, consoleGuardTestCfg) != ConsoleGuardWithhold {
		t.Fatalf("fake reasoning tokens with no deltas must withhold: %#v", fakeSig)
	}
}

func TestObserveConsoleGuardChunkRealThinkingDelivers(t *testing.T) {
	t.Parallel()
	real := consoleGuardScanState{protocol: consoleGuardProtocolResponses}
	ObserveConsoleGuardChunk(&real, []byte(sse(
		`data: {"type":"response.reasoning_summary_text.delta","delta":"plan the fix"}`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":90,"output_tokens_details":{"reasoning_tokens":60}}}}`,
	)))
	sig := real.signals()
	if !sig.HasThinking {
		t.Fatalf("streamed summary must count as thinking: %#v", sig)
	}
	if ClassifyConsoleGuardHold(sig, consoleGuardTestCfg) != ConsoleGuardDeliver {
		t.Fatalf("real thinking should deliver")
	}

	encrypted := consoleGuardScanState{protocol: consoleGuardProtocolResponses}
	ObserveConsoleGuardChunk(&encrypted, []byte(sse(
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"gAAAA-cipher"}}`,
		`data: {"type":"response.output_text.delta","delta":"hello hello hello hello hello hello hello hello"}`,
	)))
	encSig := encrypted.signals()
	if !encSig.FirstVisibleObserved {
		t.Fatalf("generated reasoning item must establish the main-audit first token: %#v", encSig)
	}
	if !encSig.HasThinking {
		t.Fatalf("encrypted reasoning item must count as thinking: %#v", encSig)
	}
	if ClassifyConsoleGuardHold(encSig, consoleGuardTestCfg) != ConsoleGuardDeliver {
		t.Fatalf("encrypted thinking should deliver")
	}
}

func TestObserveConsoleGuardChunkEmptyReasoningStubWaitsForUsage(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("word ", 40)
	responses := consoleGuardScanState{protocol: consoleGuardProtocolResponses}
	ObserveConsoleGuardChunk(&responses, []byte(sse(
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_text.delta","delta":"`+content+`"}`,
	)))
	respSig := responses.signals()
	if respSig.HasThinking {
		t.Fatalf("empty reasoning item must not count as thinking: %#v", respSig)
	}
	if ClassifyConsoleGuardHold(respSig, consoleGuardTestCfg) != ConsoleGuardWait {
		t.Fatalf("midstream empty stub must wait for usage, got %s (%#v)", ClassifyConsoleGuardHold(respSig, consoleGuardTestCfg), respSig)
	}
}

func TestPeekConsoleGuardStreamThinkingDeliversRemainder(t *testing.T) {
	t.Parallel()
	body := io.NopCloser(strings.NewReader(sse(
		`data: {"choices":[{"delta":{"thinking_content":"think"}}]}`,
		`data: {"choices":[{"delta":{"content":"answer after think"}}]}`,
		"data: [DONE]",
	)))
	replay, verdict, _, _, err := peekConsoleGuardStream(context.Background(), body, consoleGuardProtocolChat, consoleGuardTestCfg, time.Now().Add(-50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if verdict != ConsoleGuardDeliver {
		t.Fatalf("verdict=%s", verdict)
	}
	got, _ := io.ReadAll(replay)
	if !strings.Contains(string(got), "answer after think") || !strings.Contains(string(got), "thinking_content") {
		t.Fatalf("replay lost frames: %s", got)
	}
}

func TestPeekConsoleGuardStreamWithholdsNoThinkEnough(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("abcd", 40) // 160 runes → ~40 tokens
	body := io.NopCloser(strings.NewReader(sse(
		`data: {"choices":[{"delta":{"content":"`+content+`"}}]}`,
		`data: {"usage":{"completion_tokens":40,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		"data: [DONE]",
	)))
	replay, verdict, usage, _, err := peekConsoleGuardStream(context.Background(), body, consoleGuardProtocolChat, consoleGuardTestCfg, time.Now().Add(-50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	if verdict != ConsoleGuardWithhold {
		t.Fatalf("verdict=%s usage=%#v", verdict, usage)
	}
	if usage.ReasoningTokens != 0 || usage.OutputTokens < 8 {
		t.Fatalf("usage=%#v", usage)
	}
}

func TestPeekConsoleGuardStreamEmptyCompletedRetriesWithoutIdle(t *testing.T) {
	t.Parallel()
	started := time.Now()
	replay, verdict, _, _, err := peekConsoleGuardStream(
		context.Background(),
		io.NopCloser(strings.NewReader(sse(
			`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"output_tokens":0}}}`,
		))),
		consoleGuardProtocolResponses,
		consoleGuardTestCfg,
		time.Now().Add(-50*time.Millisecond),
	)
	if replay != nil {
		defer replay.Close()
	}
	if !errors.Is(err, errConsoleGuardEmptyStream) {
		t.Fatalf("peek error = %v, want empty stream", err)
	}
	if verdict != ConsoleGuardWait {
		t.Fatalf("verdict = %s, want wait so the attempt loop retries as transport", verdict)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("empty completed stream waited %s, want immediate retry", time.Since(started))
	}
}

func TestPeekConsoleGuardStreamHoldTimeoutEmptyDoesNotFailOpen(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	reader, writer := io.Pipe()
	defer writer.Close()
	done := make(chan struct{})
	var verdict ConsoleGuardVerdict
	var peekErr error
	go func() {
		defer close(done)
		_, verdict, _, _, peekErr = peekConsoleGuardStream(ctx, reader, consoleGuardProtocolChat, ConsoleGuardRuntime{
			Enabled:     true,
			SoftTPS:     consoleGuardTestSoftTPS,
			HardTPS:     consoleGuardTestHardTPS,
			HoldTimeout: 20 * time.Millisecond,
		}, time.Now().Add(-50*time.Millisecond))
	}()
	select {
	case <-done:
		t.Fatal("empty hold timeout must keep reading, not fail-open")
	case <-time.After(50 * time.Millisecond):
	}
	cancel(neterrorpkg.ErrUpstreamStreamIdleTimeout)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("peekConsoleGuardStream did not return after idle cancel")
	}
	if !neterrorpkg.IsUpstreamStreamIdleTimeout(peekErr) {
		t.Fatalf("peekErr = %v, want idle timeout", peekErr)
	}
	if verdict != ConsoleGuardWait {
		t.Fatalf("verdict=%s, want wait so the loop does not fail-open", verdict)
	}
}

func TestShouldHoldConsoleGuardStreamGates(t *testing.T) {
	t.Parallel()
	route := consoleGuardTestRoute(accountdomain.ProviderConsole)
	input := Input{Streaming: true, PublicModel: "grok-4.6"}
	if consoleGuardSkipReason(input, nil, route, audit.OperationChat, consoleGuardTestCfg) != "" {
		t.Fatal("expected hold on console chat")
	}
	buildRoute := consoleGuardTestRoute(accountdomain.ProviderBuild)
	if consoleGuardSkipReason(input, nil, buildRoute, audit.OperationChat, consoleGuardTestCfg) == "" {
		t.Fatal("build must not be guarded by console guard")
	}
	webRoute := consoleGuardTestRoute(accountdomain.ProviderWeb)
	if consoleGuardSkipReason(input, nil, webRoute, audit.OperationChat, consoleGuardTestCfg) == "" {
		t.Fatal("web must not be guarded by console guard")
	}
	off := consoleGuardTestCfg
	off.Enabled = false
	if consoleGuardSkipReason(input, nil, route, audit.OperationChat, off) == "" {
		t.Fatal("disabled must not hold")
	}
	forced := input
	forced.ForcedEgressNodeID = 9
	if consoleGuardSkipReason(forced, nil, route, audit.OperationChat, consoleGuardTestCfg) == "" {
		t.Fatal("forced egress must not hold")
	}
	owned := inferencedomain.ResponseOwnership{ResponseID: "r1", AccountID: 1}
	if consoleGuardSkipReason(input, &owned, route, audit.OperationChat, consoleGuardTestCfg) == "" {
		t.Fatal("pinned response must not hold")
	}
	if consoleGuardSkipReason(input, nil, route, audit.OperationImage, consoleGuardTestCfg) == "" {
		t.Fatal("image must not hold")
	}
	classified := input
	classified.skipQualityHold = true
	if consoleGuardSkipReason(classified, nil, route, audit.OperationResponses, consoleGuardTestCfg) == "" {
		t.Fatal("gateway-classified compaction must not hold")
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "chat reasoning none", body: `{"reasoning_effort":"none"}`},
		{name: "responses reasoning none", body: `{"reasoning":{"effort":"none"}}`},
		{name: "messages thinking disabled", body: `{"thinking":{"type":"disabled"}}`},
		{name: "messages zero thinking budget", body: `{"thinking":{"type":"enabled","budget_tokens":0}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := input
			request.Body = []byte(test.body)
			if consoleGuardSkipReason(request, nil, route, audit.OperationChat, consoleGuardTestCfg) == "" {
				t.Fatal("explicitly disabled reasoning must not be held")
			}
		})
	}
}

func TestConsoleGuardPeekAbortErrorPrefersIdleCause(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(neterrorpkg.ErrUpstreamStreamIdleTimeout)
	got := consoleGuardPeekAbortError(ctx, context.Canceled)
	if !neterrorpkg.IsUpstreamStreamIdleTimeout(got) {
		t.Fatalf("abort error = %v, want idle timeout", got)
	}
	if isClientRequestCancel(ctx, got) {
		t.Fatal("idle timeout must not look like a client cancel")
	}
}
