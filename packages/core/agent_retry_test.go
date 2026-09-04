package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bnema/zut/packages/provider"
)

type retryFakeClient struct {
	calls    int32
	firstErr error
}

func (c *retryFakeClient) Name() string { return "retry-fake" }

func (c *retryFakeClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	if req.Lifecycle != nil {
		req.Lifecycle.RequestAttempt(1, 1)
	}
	call := atomic.AddInt32(&c.calls, 1)
	out := make(chan provider.Event, 4)
	go func() {
		defer close(out)
		out <- provider.EventStart{Provider: "retry-fake", Model: req.Model}
		if call == 1 {
			err := c.firstErr
			if err == nil {
				err = fmt.Errorf("anthropic overloaded_error: Overloaded")
			}
			out <- provider.EventDone{Stop: provider.StopError, Err: err}
			return
		}
		out <- provider.EventTextDelta{Delta: "ok"}
		out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.TextBlock{Text: "ok"}},
		}}
	}()
	return out, nil
}

type silentStreamClient struct{}

func (silentStreamClient) Name() string { return "silent-stream" }

func (silentStreamClient) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
	out := make(chan provider.Event)
	go func() {
		defer close(out)
		<-ctx.Done()
	}()
	return out, nil
}

type activeStreamClient struct{}

func (activeStreamClient) Name() string { return "active-stream" }

func (activeStreamClient) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	out := make(chan provider.Event, 2)
	out <- provider.EventStart{}
	out <- provider.EventDone{Stop: provider.StopEnd}
	close(out)
	return out, nil
}

type postToolSilentClient struct {
	calls atomic.Int32
}

func (c *postToolSilentClient) Name() string { return "post-tool-silent" }

func (c *postToolSilentClient) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
	if c.calls.Add(1) == 1 {
		out := make(chan provider.Event, 1)
		out <- provider.EventDone{Stop: provider.StopToolUse, Message: provider.Message{
			Role: provider.RoleAssistant,
			Content: []provider.Content{provider.ToolCallBlock{
				ID: "call-1", Name: "result", Arguments: json.RawMessage(`{}`),
			}},
		}}
		close(out)
		return out, nil
	}
	out := make(chan provider.Event)
	go func() {
		defer close(out)
		<-ctx.Done()
	}()
	return out, nil
}

type requestOpenFailureClient struct{ calls int32 }

func (c *requestOpenFailureClient) Name() string { return "request-open-failure" }

func (c *requestOpenFailureClient) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	atomic.AddInt32(&c.calls, 1)
	return nil, errors.New("provider returned error: 503 service unavailable")
}

type readableAfterCancellationClient struct {
	cancel  context.CancelFunc
	drained chan struct{}
}

func (c readableAfterCancellationClient) Name() string { return "readable-after-cancellation" }

func (c readableAfterCancellationClient) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	out := make(chan provider.Event, 1)
	out <- provider.EventStart{}
	c.cancel()
	go func() {
		out <- provider.EventTextDelta{Delta: "discard after cancellation"}
		close(out)
		close(c.drained)
	}()
	return out, nil
}

func TestNewAgentUsesCodexStreamGuardDefaults(t *testing.T) {
	a := NewAgent(&retryFakeClient{}, "fake-model", "system", Registry{})
	if a.MaxSteps != 0 {
		t.Fatalf("max steps = %d, want unlimited", a.MaxSteps)
	}
	if a.MaxRetries != 5 {
		t.Fatalf("stream retries = %d, want 5", a.MaxRetries)
	}
	if a.StreamIdleTimeout != 5*time.Minute {
		t.Fatalf("stream idle timeout = %s, want 5m", a.StreamIdleTimeout)
	}
}

func TestAgentStopsReadingStreamAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	drained := make(chan struct{})
	a := NewAgent(readableAfterCancellationClient{cancel: cancel, drained: drained}, "fake-model", "system", Registry{})
	a.MaxRetries = 0
	if err := a.Prompt(ctx, "hello", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Prompt error = %v, want context.Canceled", err)
	}
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("provider producer remained blocked after turn cancellation")
	}
}

func TestAgentStopsSilentStreamAtIdleDeadline(t *testing.T) {
	a := NewAgent(silentStreamClient{}, "fake-model", "system", Registry{})
	a.MaxRetries = 0
	a.StreamIdleTimeout = time.Minute
	idle := make(chan time.Time, 1)
	a.streamIdleTimer = func(time.Duration) (<-chan time.Time, func(), func()) {
		idle <- time.Now()
		return idle, func() {}, func() {}
	}
	if err := a.Prompt(context.Background(), "hello", nil, nil); !errors.Is(err, ErrStreamIdleTimeout) {
		t.Fatalf("Prompt error = %v, want ErrStreamIdleTimeout", err)
	}
}

func TestAgentResetsIdleDeadlineForEveryStreamEvent(t *testing.T) {
	a := NewAgent(activeStreamClient{}, "fake-model", "system", Registry{})
	a.MaxRetries = 0
	a.StreamIdleTimeout = time.Minute
	var resets atomic.Int32
	a.streamIdleTimer = func(time.Duration) (<-chan time.Time, func(), func()) {
		return make(chan time.Time), func() {}, func() { resets.Add(1) }
	}
	if err := a.Prompt(context.Background(), "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := resets.Load(); got != 2 {
		t.Fatalf("idle timer resets = %d, want one for each stream event", got)
	}
}

func TestAgentDoesNotRetrySilentStream(t *testing.T) {
	client := &retryFakeClient{firstErr: ErrStreamIdleTimeout}
	a := NewAgent(client, "fake-model", "system", Registry{})
	a.RetryBaseDelay = time.Millisecond
	if err := a.Prompt(context.Background(), "hello", nil, nil); !errors.Is(err, ErrStreamIdleTimeout) {
		t.Fatalf("Prompt error = %v, want ErrStreamIdleTimeout", err)
	}
	if got := atomic.LoadInt32(&client.calls); got != 1 {
		t.Fatalf("Stream calls = %d; want no reconnect after idle deadline", got)
	}
}

func TestAgentFailsSilentContinuationAfterToolResult(t *testing.T) {
	client := &postToolSilentClient{}
	a := NewAgent(client, "fake-model", "system", Registry{
		"result": &resultTool{result: ToolResult{Content: []provider.Content{provider.TextBlock{Text: "ok"}}}},
	})
	a.RetryBaseDelay = time.Millisecond
	var timers atomic.Int32
	a.streamIdleTimer = func(time.Duration) (<-chan time.Time, func(), func()) {
		idle := make(chan time.Time, 1)
		if timers.Add(1) > 1 {
			idle <- time.Now()
		}
		return idle, func() {}, func() {}
	}
	var durable []string
	err := a.Prompt(context.Background(), "review", nil, func(event AgentEvent) {
		switch event.(type) {
		case EvUserMessage, EvAssistantMessage, EvToolCall, EvToolResult:
			durable = append(durable, event.Type())
		}
	})
	if !errors.Is(err, ErrStreamIdleTimeout) {
		t.Fatalf("Prompt error = %v, want ErrStreamIdleTimeout", err)
	}
	if got := client.calls.Load(); got != 2 {
		t.Fatalf("provider calls = %d, want tool request plus one bounded continuation", got)
	}
	if got, want := durable[len(durable)-1], "tool_result"; got != want {
		t.Fatalf("last durable event = %q, want %q; events = %v", got, want, durable)
	}
}

func TestAgentRetriesOverloadedStreamError(t *testing.T) {
	client := &retryFakeClient{}
	a := NewAgent(client, "fake-model", "system", Registry{})
	a.RetryBaseDelay = time.Millisecond

	var turnErrs []string
	err := a.Prompt(context.Background(), "hello", nil, func(ev AgentEvent) {
		if e, ok := ev.(EvTurnEnd); ok && e.Err != nil {
			turnErrs = append(turnErrs, e.Err.Error())
		}
	})
	if err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := atomic.LoadInt32(&client.calls); got != 2 {
		t.Fatalf("Stream calls = %d; want 2", got)
	}
	if len(turnErrs) != 1 || !strings.Contains(turnErrs[0], "overloaded_error") {
		t.Fatalf("turn errors = %v; want one overloaded error before retry", turnErrs)
	}
	msgs := a.Messages()
	if len(msgs) != 2 {
		t.Fatalf("message count = %d; want user + final assistant", len(msgs))
	}
	if got := extractText(msgs[1]); got != "ok" {
		t.Fatalf("final assistant text = %q; want ok", got)
	}
}

func TestAgentDoesNotReplayProviderRequestRetriesAtStreamLayer(t *testing.T) {
	client := &requestOpenFailureClient{}
	a := NewAgent(client, "fake-model", "system", Registry{})
	a.RetryBaseDelay = time.Millisecond

	err := a.Prompt(context.Background(), "hello", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("Prompt error = %v, want provider failure", err)
	}
	if got := atomic.LoadInt32(&client.calls); got != 1 {
		t.Fatalf("Stream calls = %d, want one provider-owned request sequence", got)
	}
}

func TestAgentRetriesUnexpectedEOFStreamError(t *testing.T) {
	client := &retryFakeClient{firstErr: fmt.Errorf("read SSE: %w", io.ErrUnexpectedEOF)}
	a := NewAgent(client, "fake-model", "system", Registry{})
	a.RetryBaseDelay = time.Millisecond

	if err := a.Prompt(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := atomic.LoadInt32(&client.calls); got != 2 {
		t.Fatalf("Stream calls = %d; want 2", got)
	}
}

func TestAgentRetriesBareEOFStreamError(t *testing.T) {
	client := &retryFakeClient{firstErr: fmt.Errorf("read SSE: %w", io.EOF)}
	a := NewAgent(client, "fake-model", "system", Registry{})
	a.RetryBaseDelay = time.Millisecond
	var records []RetryLifecycleRecord
	a.OnRetryLifecycle = func(record RetryLifecycleRecord) {
		records = append(records, record)
	}

	if err := a.Prompt(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := atomic.LoadInt32(&client.calls); got != 2 {
		t.Fatalf("Stream calls = %d; want 2", got)
	}
	if len(records) != 2 || records[0].Reason != RetryReasonNetwork || records[1].Reason != RetryReasonNetwork {
		t.Fatalf("retry lifecycle = %#v; want network failure and retry", records)
	}
}

func TestAgentEmitsRetryLifecycleEvents(t *testing.T) {
	client := &retryFakeClient{}
	a := NewAgent(client, "fake-model", "system", Registry{})
	a.RetryBaseDelay = time.Millisecond

	var lifecycle []string
	if err := a.Prompt(context.Background(), "hello", nil, func(ev AgentEvent) {
		switch e := ev.(type) {
		case EvRequestStarted:
			lifecycle = append(lifecycle, fmt.Sprintf("request:%s:%d/%d", e.Scope, e.Attempt, e.MaxAttempts))
		case EvRetryScheduled:
			lifecycle = append(lifecycle, fmt.Sprintf("retry:%s:%d/%d", e.Scope, e.Attempt, e.MaxAttempts))
		}
	}); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}

	want := []string{
		"request:agent:1/6",
		"request:provider:1/1",
		"retry:agent:2/6",
		"request:agent:2/6",
		"request:provider:1/1",
	}
	if strings.Join(lifecycle, ",") != strings.Join(want, ",") {
		t.Fatalf("lifecycle = %v, want %v", lifecycle, want)
	}
}

func TestAgentReportsSanitizedRetryLifecycle(t *testing.T) {
	client := &retryFakeClient{}
	a := NewAgent(client, "fake-model", "system", Registry{})
	a.RetryBaseDelay = time.Millisecond

	var records []RetryLifecycleRecord
	a.OnRetryLifecycle = func(record RetryLifecycleRecord) {
		records = append(records, record)
	}
	if err := a.Prompt(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}

	want := []RetryLifecycleRecord{
		{
			Event: RetryLifecycleRequestFailed, Scope: RetryScopeAgent,
			Attempt: 1, MaxAttempts: 6, Reason: RetryReasonOverload,
		},
		{
			Event: RetryLifecycleRetryScheduled, Scope: RetryScopeAgent,
			Attempt: 2, MaxAttempts: 6, Reason: RetryReasonOverload, DelayMS: 1,
		},
	}
	if !reflect.DeepEqual(records, want) {
		t.Fatalf("retry lifecycle = %#v, want %#v", records, want)
	}
}

func TestRequestLifecycleSinkForwardsProviderFailureReason(t *testing.T) {
	var records []RetryLifecycleRecord
	sink := &requestLifecycleSink{
		sink:    func(AgentEvent) {},
		observe: func(record RetryLifecycleRecord) { records = append(records, record) },
	}
	sink.RequestFailed(1, 3, provider.RequestFailureOverload, false)
	sink.RetryScheduled(2, 3, 250*time.Millisecond)

	want := []RetryLifecycleRecord{
		{
			Event: RetryLifecycleRequestFailed, Scope: RetryScopeProvider,
			Attempt: 1, MaxAttempts: 3, Reason: RetryReasonOverload,
		},
		{
			Event: RetryLifecycleRetryScheduled, Scope: RetryScopeProvider,
			Attempt: 2, MaxAttempts: 3, Reason: RetryReasonOverload, DelayMS: 250,
		},
	}
	if !reflect.DeepEqual(records, want) {
		t.Fatalf("provider retry lifecycle = %#v, want %#v", records, want)
	}
}

func TestAgentReportsTerminalRetryFailure(t *testing.T) {
	client := &retryFakeClient{}
	a := NewAgent(client, "fake-model", "system", Registry{})
	a.MaxRetries = 0

	var records []RetryLifecycleRecord
	a.OnRetryLifecycle = func(record RetryLifecycleRecord) {
		records = append(records, record)
	}
	if err := a.Prompt(context.Background(), "hello", nil, nil); err == nil {
		t.Fatal("Prompt succeeded; want terminal provider error")
	}
	if len(records) != 1 {
		t.Fatalf("retry lifecycle = %#v, want one terminal record", records)
	}
	record := records[0]
	if record.Event != RetryLifecycleRequestFailed || record.Scope != RetryScopeAgent ||
		record.Attempt != 1 || record.MaxAttempts != 1 || record.Reason != RetryReasonOverload || !record.Terminal {
		t.Fatalf("terminal retry record = %#v", record)
	}
}

// codexRetryFakeClient reproduces a transient OpenAI Codex backend
// failure on the first call and succeeds afterwards.
type codexRetryFakeClient struct {
	calls    int32
	firstErr string
}

func (c *codexRetryFakeClient) Name() string { return "openai-codex" }

func (c *codexRetryFakeClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	call := atomic.AddInt32(&c.calls, 1)
	out := make(chan provider.Event, 4)
	go func() {
		defer close(out)
		out <- provider.EventStart{Provider: "openai-codex", Model: req.Model}
		if call == 1 {
			out <- provider.EventDone{Stop: provider.StopError, Err: fmt.Errorf("%s", c.firstErr)}
			return
		}
		out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.TextBlock{Text: "ok"}},
		}}
	}()
	return out, nil
}

func TestAgentRetriesCodexProcessingError(t *testing.T) {
	cases := []struct {
		name string
		err  string
	}{
		{
			name: "processing error",
			err:  "codex error: An error occurred while processing your request. You can retry your request, or contact us through our help center at help.openai.com if the error persists. Please include the request ID 60c8ebbd-20bd-42e4-b756-6e844041cfc0 in your message.",
		},
		{
			name: "servers overloaded",
			err:  "codex error: Our servers are currently overloaded. Please try again later.",
		},
		{
			name: "try again later only",
			err:  "codex error: Something went wrong. Please try again later.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &codexRetryFakeClient{firstErr: tc.err}
			a := NewAgent(client, "gpt-5.6-sol", "system", Registry{})
			a.RetryBaseDelay = time.Millisecond

			if err := a.Prompt(context.Background(), "hello", nil, nil); err != nil {
				t.Fatalf("Prompt returned %v; want retry to succeed", err)
			}
			if got := atomic.LoadInt32(&client.calls); got != 2 {
				t.Fatalf("Stream calls = %d; want 2", got)
			}
			msgs := a.Messages()
			if len(msgs) != 2 || extractText(msgs[1]) != "ok" {
				t.Fatalf("messages = %v; want user + ok assistant", msgs)
			}
		})
	}
}

// TestCanRetryErrorCapacityMessages pins the classification of Codex
// capacity wording and makes sure quota/usage limits stay terminal even
// when they carry "try again later" style advice.
func TestCanRetryErrorCapacityMessages(t *testing.T) {
	a := NewAgent(nil, "gpt-5.6-sol", "system", Registry{})
	cases := []struct {
		msg  string
		want bool
	}{
		{"codex error: Our servers are currently overloaded. Please try again later.", true},
		{"codex error: Our servers are busy right now.", true},
		{"codex error: Please try again later.", true},
		{"codex error: You have hit your monthly usage limit. Try again later.", false},
		{"codex error: quota exceeded, try again later", false},
		{"codex error: unsupported parameter: reasoning", false},
	}
	for _, tc := range cases {
		if got := a.canRetryError(errors.New(tc.msg), 0); got != tc.want {
			t.Errorf("canRetryError(%q) = %v; want %v", tc.msg, got, tc.want)
		}
	}
}

type partialRetryFakeClient struct {
	calls int32
}

func (c *partialRetryFakeClient) Name() string { return "partial-retry-fake" }

func (c *partialRetryFakeClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	call := atomic.AddInt32(&c.calls, 1)
	out := make(chan provider.Event, 4)
	go func() {
		defer close(out)
		out <- provider.EventStart{Provider: "partial-retry-fake", Model: req.Model}
		if call == 1 {
			out <- provider.EventTextDelta{Delta: "partial"}
			out <- provider.EventDone{Stop: provider.StopError, Err: fmt.Errorf("provider returned error: 503"), Message: provider.Message{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "partial"}},
			}}
			return
		}
		out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.TextBlock{Text: "recovered"}},
		}}
	}()
	return out, nil
}

func TestAgentDropsPartialAssistantBeforeRetry(t *testing.T) {
	client := &partialRetryFakeClient{}
	a := NewAgent(client, "fake-model", "system", Registry{})
	a.RetryBaseDelay = time.Millisecond

	if err := a.Prompt(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	msgs := a.Messages()
	if len(msgs) != 2 {
		t.Fatalf("message count = %d; want user + recovered assistant", len(msgs))
	}
	if got := extractText(msgs[1]); got != "recovered" {
		t.Fatalf("final assistant text = %q; want recovered", got)
	}
}

// captureClient records the last Request it received so tests can
// assert what the agent put on the wire.
type captureClient struct {
	lastReq provider.Request
}

func (c *captureClient) Name() string { return "capture" }

func (c *captureClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.lastReq = req
	out := make(chan provider.Event, 3)
	go func() {
		defer close(out)
		out <- provider.EventStart{Provider: "capture", Model: req.Model}
		out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.TextBlock{Text: "ok"}},
		}}
	}()
	return out, nil
}

func TestAgentPersistsTurnContextAsDeveloperHistory(t *testing.T) {
	client := &captureClient{}
	a := NewAgent(client, "fake-model", "system", Registry{})
	a.BeforeTurnContext = func(context.Context, int) (bool, string, string) {
		return true, "", "current phase: parse files"
	}
	if err := a.Prompt(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if strings.Contains(client.lastReq.System, "current phase: parse files") {
		t.Fatalf("stable request system prompt = %q; dynamic context must not rewrite it", client.lastReq.System)
	}
	if len(client.lastReq.Messages) != 2 || client.lastReq.Messages[0].Role != provider.RoleDeveloper || !strings.Contains(extractText(client.lastReq.Messages[0]), "current phase: parse files") {
		t.Fatalf("request context history = %#v, want developer context before user task", client.lastReq.Messages)
	}
	if len(a.Messages()) != 3 || a.Messages()[0].Role != provider.RoleDeveloper {
		t.Fatalf("persisted transcript = %#v, want developer context", a.Messages())
	}
}

func TestBoundedTurnContextPreservesUTF8AndLimit(t *testing.T) {
	contextText := strings.Repeat("界", maxTurnContextBytes)
	got := boundedTurnContext(contextText)
	if len(got) > maxTurnContextBytes {
		t.Fatalf("bounded context bytes = %d, want <= %d", len(got), maxTurnContextBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("bounded context is not valid UTF-8")
	}
	if !strings.HasSuffix(got, turnContextTruncatedMarker) {
		tail := got
		if len(tail) > 64 {
			tail = tail[len(tail)-64:]
		}
		t.Fatalf("bounded context missing truncation marker: %q", tail)
	}
}

func TestAgentPropagatesMaxTokens(t *testing.T) {
	client := &captureClient{}
	a := NewAgent(client, "fake-model", "system", Registry{})
	a.MaxTokens = 64000

	if err := a.Prompt(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if client.lastReq.MaxTokens != 64000 {
		t.Fatalf("request MaxTokens = %d; want 64000 (Agent.MaxTokens not propagated)", client.lastReq.MaxTokens)
	}
}

func TestAgentPropagatesTemperature(t *testing.T) {
	client := &captureClient{}
	a := NewAgent(client, "fake-model", "system", Registry{})
	temp := float32(0)
	a.Temperature = &temp

	if err := a.Prompt(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if client.lastReq.Temperature == nil || *client.lastReq.Temperature != temp {
		t.Fatalf("request Temperature = %v; want %v", client.lastReq.Temperature, temp)
	}
}

type cancelledRetryClient struct {
	cancel context.CancelFunc
}

func (c *cancelledRetryClient) Name() string { return "cancelled-retry" }

func (c *cancelledRetryClient) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	out := make(chan provider.Event, 1)
	c.cancel()
	out <- provider.EventDone{Stop: provider.StopError, Err: errors.New("provider overloaded")}
	close(out)
	return out, nil
}

func TestAgentDoesNotReportRetryAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &cancelledRetryClient{cancel: cancel}
	agent := NewAgent(client, "fake-model", "system", Registry{})
	agent.MaxRetries = 1

	var retries []EvRetryScheduled
	var lifecycle []RetryLifecycleRecord
	agent.OnRetryLifecycle = func(record RetryLifecycleRecord) {
		lifecycle = append(lifecycle, record)
	}
	err := agent.Prompt(ctx, "hello", nil, func(event AgentEvent) {
		if retry, ok := event.(EvRetryScheduled); ok {
			retries = append(retries, retry)
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Prompt error = %v, want context.Canceled", err)
	}
	if len(retries) != 0 {
		t.Fatalf("retry events = %#v, want none after cancellation", retries)
	}
	if len(lifecycle) != 1 || lifecycle[0].Event != RetryLifecycleRequestFailed || !lifecycle[0].Terminal {
		t.Fatalf("retry lifecycle = %#v, want one terminal failure after cancellation", lifecycle)
	}
}
