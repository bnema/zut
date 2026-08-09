package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

// recordingTool captures the args it was invoked with so the test
// can verify the interceptor-rewritten args reached execution.
type recordingTool struct {
	lastArgs json.RawMessage
}

type resultTool struct {
	result ToolResult
	err    error
	panic  bool
}

func (t *resultTool) Name() string            { return "result" }
func (t *resultTool) Description() string     { return "result" }
func (t *resultTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *resultTool) Execute(context.Context, json.RawMessage, func(string)) (ToolResult, error) {
	if t.panic {
		panic("tool boom")
	}
	return t.result, t.err
}

func (r *recordingTool) Name() string            { return "echo" }
func (r *recordingTool) Description() string     { return "echoes" }
func (r *recordingTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (r *recordingTool) Execute(_ context.Context, args json.RawMessage, _ func(string)) (ToolResult, error) {
	r.lastArgs = append(json.RawMessage(nil), args...)
	return ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: "ok"}},
	}, nil
}

// TestBeforeToolExecuteModifiesArgs verifies that a non-nil
// modifiedArgs returned from BeforeToolExecute is what the tool
// actually sees.
func TestBeforeToolExecuteModifiesArgs(t *testing.T) {
	rec := &recordingTool{}
	reg := Registry{"echo": rec}
	a := NewAgent(nil, "test", "", reg)

	newArgs := json.RawMessage(`{"command":"echo GUARDED: ls"}`)
	a.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return true, "", newArgs
	}

	ctx := context.Background()
	tools := a.ToolsSnapshot()
	res := a.runOneTool(ctx, provider.ToolCallBlock{
		ID:        "T1",
		Name:      "echo",
		Arguments: json.RawMessage(`{"command":"ls"}`),
	}, tools, func(AgentEvent) {})
	if res.IsError {
		t.Fatalf("unexpected error result: %v", res.Content)
	}
	assertToolTiming(t, res.Timing)
	if string(rec.lastArgs) != string(newArgs) {
		t.Errorf("tool saw %s, want %s", string(rec.lastArgs), string(newArgs))
	}
}

// TestBeforeToolExecuteInvalidJSONIgnored verifies that returning
// malformed JSON as modifiedArgs leaves the original args intact
// (safety: a buggy interceptor can't corrupt the call).
func TestBeforeToolExecuteInvalidJSONIgnored(t *testing.T) {
	rec := &recordingTool{}
	reg := Registry{"echo": rec}
	a := NewAgent(nil, "test", "", reg)

	a.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return true, "", json.RawMessage(`{not json`)
	}

	ctx := context.Background()
	tools := a.ToolsSnapshot()
	orig := json.RawMessage(`{"command":"ls"}`)
	a.runOneTool(ctx, provider.ToolCallBlock{
		ID:        "T1",
		Name:      "echo",
		Arguments: orig,
	}, tools, func(AgentEvent) {})
	if string(rec.lastArgs) != string(orig) {
		t.Errorf("tool saw %s, want original %s", string(rec.lastArgs), string(orig))
	}
}

func TestRunOneToolEmitsExecutionStartedAfterApproval(t *testing.T) {
	rec := &recordingTool{}
	a := NewAgent(nil, "test", "", Registry{"echo": rec})

	var started []EvToolExecutionStarted
	res := a.runOneTool(context.Background(), provider.ToolCallBlock{
		ID: "T1", Name: "echo", Arguments: json.RawMessage(`{"command":"echo ok"}`),
	}, a.ToolsSnapshot(), func(ev AgentEvent) {
		if e, ok := ev.(EvToolExecutionStarted); ok {
			started = append(started, e)
		}
	})
	if res.IsError {
		t.Fatalf("runOneTool error = %#v", res)
	}
	if len(started) != 1 || started[0].ID != "T1" || started[0].Name != "echo" {
		t.Fatalf("execution events = %#v", started)
	}
}

func TestRunOneToolDoesNotEmitExecutionStartedForMissingOrDeniedTools(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools Registry
		guard func(provider.ToolCallBlock) (bool, string, json.RawMessage)
	}{
		{name: "missing tool", tools: Registry{}},
		{
			name:  "denied tool",
			tools: Registry{"echo": &recordingTool{}},
			guard: func(provider.ToolCallBlock) (bool, string, json.RawMessage) { return false, "denied", nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAgent(nil, "test", "", tc.tools)
			a.BeforeToolExecute = tc.guard
			var started int
			a.runOneTool(context.Background(), provider.ToolCallBlock{ID: "T1", Name: "echo"}, a.ToolsSnapshot(), func(event AgentEvent) {
				if _, ok := event.(EvToolExecutionStarted); ok {
					started++
				}
			})
			if started != 0 {
				t.Fatalf("execution-started events = %d, want none", started)
			}
		})
	}
}

// TestBeforeToolExecuteBlockSurfacesReason verifies a refusal from
// the interceptor returns an error ToolResult with the reason text.
func TestBeforeToolExecuteBlockSurfacesReason(t *testing.T) {
	rec := &recordingTool{}
	reg := Registry{"echo": rec}
	a := NewAgent(nil, "test", "", reg)

	a.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return false, "nope", nil
	}

	ctx := context.Background()
	tools := a.ToolsSnapshot()
	res := a.runOneTool(ctx, provider.ToolCallBlock{
		ID:        "T1",
		Name:      "echo",
		Arguments: json.RawMessage(`{"command":"ls"}`),
	}, tools, func(AgentEvent) {})
	if !res.IsError {
		t.Fatal("want error result, got success")
	}
	if len(res.Content) == 0 {
		t.Fatal("no content")
	}
	tb, ok := res.Content[0].(provider.TextBlock)
	if !ok || tb.Text != "nope" {
		t.Errorf("want reason 'nope', got %v", res.Content[0])
	}
	if rec.lastArgs != nil {
		t.Error("tool ran despite block")
	}
	assertToolTiming(t, res.Timing)
}

func TestRunOneToolTimingCoversOutcomes(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name  string
		tools Registry
		ctx   context.Context
		want  string
	}{
		{name: "success", tools: Registry{"result": &resultTool{result: ToolResult{Content: []provider.Content{provider.TextBlock{Text: "ok"}}}}}, ctx: context.Background()},
		{name: "ordinary error", tools: Registry{"result": &resultTool{err: errors.New("ordinary")}}, ctx: context.Background()},
		{name: "cancellation", tools: Registry{"result": &resultTool{err: context.Canceled}}, ctx: cancelled},
		{name: "missing", tools: Registry{}, ctx: context.Background()},
		{name: "panic", tools: Registry{"result": &resultTool{panic: true}}, ctx: context.Background()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAgent(nil, "test", "", tc.tools)
			callName := "result"
			if tc.name == "missing" {
				callName = "missing"
			}
			res := a.runOneTool(tc.ctx, provider.ToolCallBlock{ID: "timed", Name: callName}, tc.tools, func(AgentEvent) {})
			assertToolTiming(t, res.Timing)
			if tc.name == "panic" {
				text, ok := res.Content[0].(provider.TextBlock)
				if !ok || text.Text != "tool execution failed" {
					t.Fatalf("panic result = %#v, want generic tool execution error", res.Content)
				}
			}
		})
	}
}

func assertToolTiming(t *testing.T, timing *provider.ToolTiming) {
	t.Helper()
	if timing == nil {
		t.Fatal("tool result has no timing")
	}
	if timing.StartedAt.IsZero() || timing.CompletedAt.IsZero() {
		t.Fatalf("tool timing has zero wall-clock bound: %#v", timing)
	}
	if timing.CompletedAt.Before(timing.StartedAt) {
		t.Fatalf("tool completed before start: %#v", timing)
	}
	if timing.Duration < 0 {
		t.Fatalf("tool duration = %s, want non-negative", timing.Duration)
	}
}
