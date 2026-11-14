package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

type invokingTestTool struct {
	name      string
	execute   func(context.Context) ToolResult
	immutable bool
}

func (t *invokingTestTool) Name() string               { return t.name }
func (*invokingTestTool) Description() string          { return "nested invocation test" }
func (*invokingTestTool) Schema() json.RawMessage      { return json.RawMessage(`{"type":"object"}`) }
func (t *invokingTestTool) AllowArgumentRewrite() bool { return !t.immutable }
func (t *invokingTestTool) Execute(ctx context.Context, _ json.RawMessage, _ func(string)) (ToolResult, error) {
	return t.execute(ctx), nil
}

func TestInvokeToolReportsDenied(t *testing.T) {
	a := NewAgent(nil, "test", "", Registry{"echo": &recordingTool{}})
	a.BeforeToolExecute = func(provider.ToolCallBlock) (bool, string, json.RawMessage) { return false, "guard reason", nil }
	res, denied := a.invokeTool(context.Background(), provider.ToolCallBlock{Name: "echo"}, a.ToolsSnapshot(), invokeHooks{})
	if !denied || !res.IsError || res.Content[0].(provider.TextBlock).Text != "guard reason" {
		t.Fatalf("result=%#v denied=%v", res, denied)
	}
	assertToolTiming(t, res.Timing)
}

func TestNestedInvokerAgentTurn(t *testing.T) {
	for _, mode := range []string{"allow", "deny", "rewrite"} {
		t.Run(mode, func(t *testing.T) {
			var outcome NestedOutcome
			var childRan bool
			child := &invokingTestTool{name: "child", immutable: true, execute: func(ctx context.Context) ToolResult {
				childRan = true
				if _, ok := NestedInvokerFrom(ctx); ok {
					t.Error("nested tool received invoker")
				}
				return ToolResult{}
			}}
			parent := &invokingTestTool{name: "parent", execute: func(ctx context.Context) ToolResult {
				inv, ok := NestedInvokerFrom(ctx)
				if !ok {
					t.Fatal("parent missing invoker")
				}
				if _, err := inv.Tools().Get("child"); err != nil {
					t.Fatal(err)
				}
				outcome = inv.Invoke(ctx, "child", nil)
				return ToolResult{Content: []provider.Content{provider.TextBlock{Text: "parent done"}}}
			}}
			client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
				{content: []provider.Content{provider.ToolCallBlock{ID: "parent-id", Name: "parent", Origin: "test-origin"}}, stop: provider.StopToolUse},
				{content: []provider.Content{textContent("done", "")}, stop: provider.StopEnd},
			}}
			a := NewAgent(client, "test", "", Registry{"parent": parent, "child": child})
			var calls []provider.ToolCallBlock
			a.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
				calls = append(calls, call)
				if call.Name == "child" {
					if mode == "deny" {
						return false, "nested denied", nil
					}
					if mode == "rewrite" {
						return true, "", json.RawMessage(`{"changed":true}`)
					}
				}
				return true, "", nil
			}
			var events []AgentEvent
			if err := runPrompt(t, a, &events); err != nil {
				t.Fatal(err)
			}
			if len(calls) != 2 || calls[1].ID != "parent-id/1" || calls[1].Origin != "test-origin" {
				t.Fatalf("calls=%#v", calls)
			}
			if outcome.ID != "parent-id/1" || outcome.Denied != (mode != "allow") || outcome.Result.IsError != (mode != "allow") || childRan != (mode == "allow") {
				t.Fatalf("outcome=%#v childRan=%v", outcome, childRan)
			}
			if mode == "rewrite" && outcome.Result.Content[0].(provider.TextBlock).Text != "tool arguments cannot be rewritten" {
				t.Fatalf("rewrite result=%#v", outcome)
			}
			results := 0
			for _, ev := range events {
				if e, ok := ev.(EvToolResult); ok {
					results++
					if e.ID != "parent-id" {
						t.Errorf("nested result event: %#v", e)
					}
				}
			}
			if results != 1 {
				t.Fatalf("result events=%d", results)
			}
			blocks := 0
			for _, msg := range a.Messages() {
				for _, c := range msg.Content {
					if b, ok := c.(provider.ToolResultBlock); ok {
						blocks++
						if b.CallID != "parent-id" {
							t.Errorf("nested transcript result: %#v", b)
						}
					}
				}
			}
			if blocks != 1 {
				t.Fatalf("transcript result blocks=%d", blocks)
			}
		})
	}
}

func TestNestedInvokerContext(t *testing.T) {
	ctx := context.Background()
	if _, ok := NestedInvokerFrom(ctx); ok {
		t.Fatal("unexpected invoker")
	}
	ctx = withNestedInvoker(ctx, &nestedInvoker{})
	if _, ok := NestedInvokerFrom(ctx); !ok {
		t.Fatal("missing invoker")
	}
	if _, ok := NestedInvokerFrom(withNestedInvoker(ctx, nil)); ok {
		t.Fatal("nil did not strip invoker")
	}
}

func TestNestedInvokerConcurrent(t *testing.T) {
	const count = 16
	a := NewAgent(nil, "test", "", Registry{"echo": &resultTool{}})
	var mu sync.Mutex
	seen := make(map[string]bool)
	a.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		mu.Lock()
		defer mu.Unlock()
		if seen[call.ID] {
			t.Errorf("duplicate ID %q", call.ID)
		}
		seen[call.ID] = true
		return call.ID != "parent/1", "denied", nil
	}
	inv := &nestedInvoker{agent: a, tools: a.ToolsSnapshot(), parent: provider.ToolCallBlock{ID: "parent"}}
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(count)
	done.Add(count)
	var outcomes []NestedOutcome
	for i := 0; i < count; i++ {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			out := inv.Invoke(context.Background(), "echo", nil)
			mu.Lock()
			outcomes = append(outcomes, out)
			mu.Unlock()
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	if len(seen) != count || len(outcomes) != count {
		t.Fatalf("calls=%d outcomes=%d", len(seen), len(outcomes))
	}
	for i := 1; i <= count; i++ {
		if !seen[fmt.Sprintf("parent/%d", i)] {
			t.Errorf("missing ID parent/%d", i)
		}
	}
	denied := 0
	for _, out := range outcomes {
		if out.Denied {
			denied++
			if out.ID != "parent/1" || !out.Result.IsError {
				t.Errorf("unexpected denial: %#v", out)
			}
		}
	}
	if denied != 1 || !a.sawDeniedToolCall() {
		t.Fatalf("denials=%d tracked=%v", denied, a.sawDeniedToolCall())
	}
}

func TestNestedInvokerDoesNotCommitMetadata(t *testing.T) {
	op := PlanOperation{Action: "set", Steps: []PlanStep{{Step: "nested plan", Status: PlanInProgress}}}
	child := &invokingTestTool{name: "child", execute: func(context.Context) ToolResult {
		return ToolResult{ActivateTools: []string{"extra"}, Details: op, PlanOperation: &op}
	}}
	parent := &invokingTestTool{name: "parent", execute: func(ctx context.Context) ToolResult {
		inv, ok := NestedInvokerFrom(ctx)
		if !ok {
			t.Fatal("missing invoker")
		}
		out := inv.Invoke(ctx, "child", nil)
		if out.Result.IsError || len(out.Result.ActivateTools) != 1 || out.Result.PlanOperation == nil {
			t.Fatalf("nested metadata lost: %#v", out)
		}
		return ToolResult{}
	}}
	a := NewAgent(nil, "test", "", Registry{"parent": parent, "child": child, "extra": &resultTool{}})
	commits, observations := 0, 0
	a.CommitToolResult = func(id string, res ToolResult) error {
		commits++
		if id != "parent" || len(res.ActivateTools) != 0 || res.PlanOperation != nil {
			t.Errorf("unexpected commit %q: %#v", id, res)
		}
		return nil
	}
	a.OnToolResult = func(id string, res ToolResult) {
		observations++
		if id != "parent" || len(res.ActivateTools) != 0 || res.PlanOperation != nil {
			t.Errorf("unexpected observation %q: %#v", id, res)
		}
	}
	msg, failed := a.executeTools(context.Background(), provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "parent", Name: "parent"}}}, func(AgentEvent) {})
	if failed || commits != 1 || observations != 1 {
		t.Fatalf("failed=%v commits=%d observations=%d", failed, commits, observations)
	}
	if len(msg.AddedToolNames) != 0 || len(a.CurrentPlan()) != 0 {
		t.Fatalf("nested metadata applied: tools=%v plan=%v", msg.AddedToolNames, a.CurrentPlan())
	}
}

func TestTopLevelRewriteRefusalDoesNotTrackDenial(t *testing.T) {
	a := NewAgent(nil, "test", "", Registry{"result": &immutableResultTool{}})
	a.BeforeToolExecute = func(provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return true, "", json.RawMessage(`{"changed":true}`)
	}
	res := a.runOneTool(context.Background(), provider.ToolCallBlock{ID: "top", Name: "result"}, a.ToolsSnapshot(), func(AgentEvent) {})
	if !res.IsError || a.sawDeniedToolCall() {
		t.Fatalf("result=%#v tracked=%v", res, a.sawDeniedToolCall())
	}
}
