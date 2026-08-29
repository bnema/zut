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

type immutableResultTool struct{ resultTool }

func (*immutableResultTool) AllowArgumentRewrite() bool { return false }

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

func TestToolResultPersistenceFailureBecomesToolError(t *testing.T) {
	agent := NewAgent(nil, "test", "", Registry{
		"result": &resultTool{result: ToolResult{
			Content: []provider.Content{provider.TextBlock{Text: "done"}},
			Details: "durable update",
		}},
	})
	agent.CommitToolResult = func(string, ToolResult) error { return errors.New("disk full") }
	message := provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.Content{provider.ToolCallBlock{
			ID:        "call-1",
			Name:      "result",
			Arguments: json.RawMessage(`{}`),
		}},
	}
	var event EvToolResult
	toolMessage, hadError := agent.executeTools(context.Background(), message, func(ev AgentEvent) {
		if result, ok := ev.(EvToolResult); ok {
			event = result
		}
	})

	if !hadError {
		t.Fatal("persistence failure did not mark the tool batch failed")
	}
	if !event.Result.IsError || event.Result.Details != nil {
		t.Fatalf("tool event result = %#v", event.Result)
	}
	block, ok := toolMessage.Content[0].(provider.ToolResultBlock)
	if !ok || !block.IsError {
		t.Fatalf("tool transcript block = %#v", toolMessage.Content)
	}
	if got := block.Content[0].(provider.TextBlock).Text; got != "tool result state could not be persisted" {
		t.Fatalf("tool error text = %q", got)
	}
}

func TestExecuteToolsEmitsPlanUpdateBeforeResult(t *testing.T) {
	update := PlanUpdate{Plan: []PlanStep{{Step: "Implement", Status: PlanInProgress}}}
	agent := NewAgent(nil, "test", "", Registry{
		"result": &resultTool{result: ToolResult{
			Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}},
			Details: update,
		}},
	})
	message := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{
		provider.ToolCallBlock{ID: "call-1", Name: "result", Arguments: json.RawMessage(`{}`)},
	}}
	var events []AgentEvent
	agent.executeTools(context.Background(), message, func(event AgentEvent) {
		switch event.(type) {
		case EvPlanUpdate, EvToolResult:
			events = append(events, event)
		}
	})

	if len(events) != 2 {
		t.Fatalf("events = %#v, want plan update and tool result", events)
	}
	planEvent, ok := events[0].(EvPlanUpdate)
	if !ok || len(planEvent.Update.Plan) != 1 || planEvent.Update.Plan[0].Step != "Implement" {
		t.Fatalf("first event = %#v, want plan update", events[0])
	}
	if _, ok := events[1].(EvToolResult); !ok {
		t.Fatalf("second event = %T, want EvToolResult", events[1])
	}
}

func TestToolResultCommitErrorUsesSafeMessage(t *testing.T) {
	agent := NewAgent(nil, "test", "", Registry{
		"result": &resultTool{result: ToolResult{Content: []provider.Content{provider.TextBlock{Text: "done"}}}},
	})
	agent.CommitToolResult = func(string, ToolResult) error {
		return &ToolResultCommitError{Message: "goal transition rejected: the current goal is still active", Err: errors.New("database path /private/session is unavailable")}
	}
	message := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "call-1", Name: "result", Arguments: json.RawMessage(`{}`)}}}
	toolMessage, hadError := agent.executeTools(context.Background(), message, func(AgentEvent) {})
	if !hadError {
		t.Fatal("commit error did not mark the tool batch failed")
	}
	block := toolMessage.Content[0].(provider.ToolResultBlock)
	if got := block.Content[0].(provider.TextBlock).Text; got != "goal transition rejected: the current goal is still active" {
		t.Fatalf("tool error text = %q", got)
	}
}

func TestBeforeToolExecuteRejectsRewriteForImmutableTool(t *testing.T) {
	tool := &immutableResultTool{}
	agent := NewAgent(nil, "test", "", Registry{"result": tool})
	agent.BeforeToolExecute = func(provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return true, "", json.RawMessage(`{"rewritten":true}`)
	}

	result := agent.runOneTool(context.Background(), provider.ToolCallBlock{
		ID: "T1", Name: "result", Arguments: json.RawMessage(`{"original":true}`),
	}, agent.ToolsSnapshot(), func(AgentEvent) {})
	if !result.IsError {
		t.Fatalf("result = %#v, want rewrite error", result)
	}
	if got := result.Content[0].(provider.TextBlock).Text; got != "tool arguments cannot be rewritten" {
		t.Fatalf("rewrite error = %q", got)
	}
}

// TestBeforeToolExecuteModifiesArgs verifies that a non-nil modifiedArgs
// returned from BeforeToolExecute is what the tool actually sees.
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
