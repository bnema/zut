package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

// planCallTool stands in for the plan tool in core's tests: executeTools only
// requires a tool whose Details is a parsed PlanOperation. Ops are popped in
// call order so one message can exercise a batch of plan calls.
type planCallTool struct {
	ops []PlanOperation
}

func (*planCallTool) Name() string            { return "plan" }
func (*planCallTool) Description() string     { return "plan" }
func (*planCallTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

// AllowArgumentRewrite mirrors the real tool: guards may allow or refuse a call
// but must never rewrite its arguments.
func (*planCallTool) AllowArgumentRewrite() bool { return false }

func (t *planCallTool) Execute(context.Context, json.RawMessage, func(string)) (ToolResult, error) {
	op := t.ops[0]
	t.ops = t.ops[1:]
	return ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: "raw plan result"}},
		Details: op,
	}, nil
}

func planToolMessage(ids ...string) provider.Message {
	content := make([]provider.Content, 0, len(ids))
	for _, id := range ids {
		content = append(content, provider.ToolCallBlock{ID: id, Name: "plan", Arguments: json.RawMessage(`{}`)})
	}
	return provider.Message{Role: provider.RoleAssistant, Content: content}
}

func planResultTextOf(t *testing.T, msg provider.Message, index int) string {
	t.Helper()
	if index >= len(msg.Content) {
		t.Fatalf("tool message has %d results, want index %d", len(msg.Content), index)
	}
	block, ok := msg.Content[index].(provider.ToolResultBlock)
	if !ok {
		t.Fatalf("result %d = %T, want provider.ToolResultBlock", index, msg.Content[index])
	}
	if len(block.Content) != 1 {
		t.Fatalf("result %d content = %#v, want one text block", index, block.Content)
	}
	text, ok := block.Content[0].(provider.TextBlock)
	if !ok {
		t.Fatalf("result %d text = %T, want provider.TextBlock", index, block.Content[0])
	}
	return text.Text
}

// TestExecuteToolsAppliesPlanAfterCommit covers the happy path: preview
// materializes the list, the commit hook persists it, state advances, and the
// event carries the call ID and the materialized plan before the tool result.
func TestExecuteToolsAppliesPlanAfterCommit(t *testing.T) {
	want := []PlanStep{planStep("one", PlanPending), planStep("two", PlanInProgress)}
	agent := NewAgent(nil, "test", "", Registry{
		"plan": &planCallTool{ops: []PlanOperation{{Action: "set", Steps: []PlanStep{planStep("one", PlanPending), planStep("two", PlanInProgress)}}}},
	})
	var committed []PlanUpdate
	agent.CommitToolResult = func(_ string, result ToolResult) error {
		update, ok := result.Details.(PlanUpdate)
		if !ok {
			t.Errorf("CommitToolResult details = %T, want PlanUpdate", result.Details)
		}
		committed = append(committed, update)
		return nil
	}

	var events []AgentEvent
	msg, hadError := agent.executeTools(context.Background(), planToolMessage("call-1"), func(ev AgentEvent) {
		events = append(events, ev)
	})
	if hadError {
		t.Fatal("successful plan call marked the batch failed")
	}
	if len(committed) != 1 || !slices.Equal(committed[0].Plan, want) {
		t.Fatalf("committed = %#v, want %#v", committed, want)
	}
	if got := agent.CurrentPlan(); !slices.Equal(got, want) {
		t.Fatalf("plan = %#v, want %#v", got, want)
	}

	var order []string
	var planEvent EvPlanUpdate
	for _, ev := range events {
		switch e := ev.(type) {
		case EvPlanUpdate:
			planEvent = e
			order = append(order, "plan")
		case EvToolResult:
			order = append(order, "result")
		}
	}
	if len(order) != 2 || order[0] != "plan" || order[1] != "result" {
		t.Fatalf("event order = %#v, want plan update before tool result", order)
	}
	if planEvent.CallID != "call-1" || !slices.Equal(planEvent.Update.Plan, want) {
		t.Fatalf("plan event = %#v, want call-1 and %#v", planEvent, want)
	}
	if got := planResultTextOf(t, msg, 0); got != "Plan updated (0/2 completed; in progress: step 2)" {
		t.Fatalf("result text = %q", got)
	}
}

// TestExecuteToolsPreviewFailureChangesNothing covers a state-dependent
// validation failure: the model gets the reason, and neither state nor the
// event stream moves.
func TestExecuteToolsPreviewFailureChangesNothing(t *testing.T) {
	before := []PlanStep{planStep("one", PlanPending)}
	agent := NewAgent(nil, "test", "", Registry{
		"plan": &planCallTool{ops: []PlanOperation{{Action: "remove", Index: 5}}},
	})
	agent.SetPlan(before)

	var events []AgentEvent
	msg, hadError := agent.executeTools(context.Background(), planToolMessage("call-1"), func(ev AgentEvent) {
		if _, ok := ev.(EvPlanUpdate); ok {
			events = append(events, ev)
		}
	})
	if !hadError {
		t.Fatal("failed preview did not mark the batch failed")
	}
	if len(events) != 0 {
		t.Fatalf("events = %#v, want no plan update", events)
	}
	if got := agent.CurrentPlan(); !slices.Equal(got, before) {
		t.Fatalf("plan = %#v, want it untouched", got)
	}
	block := msg.Content[0].(provider.ToolResultBlock)
	if !block.IsError || planResultTextOf(t, msg, 0) != "plan: index 5 out of range (plan has 1 steps)" {
		t.Fatalf("result = %#v, want the validation error", block)
	}
}

// TestExecuteToolsCommitFailureChangesNothing covers a rejected persistence:
// the existing commit-error path replaces the result, and because that clears
// PlanOperation, nothing is committed and no event is emitted.
func TestExecuteToolsCommitFailureChangesNothing(t *testing.T) {
	// A pre-existing plan makes "untouched" distinguishable from "was empty".
	existing := []PlanStep{planStep("existing", PlanPending)}
	agent := NewAgent(nil, "test", "", Registry{
		"plan": &planCallTool{ops: []PlanOperation{{Action: "set", Steps: []PlanStep{planStep("one", PlanPending)}}}},
	})
	agent.SetPlan(existing)
	agent.CommitToolResult = func(string, ToolResult) error { return errors.New("disk full") }

	var events []AgentEvent
	msg, hadError := agent.executeTools(context.Background(), planToolMessage("call-1"), func(ev AgentEvent) {
		if _, ok := ev.(EvPlanUpdate); ok {
			events = append(events, ev)
		}
	})
	if !hadError {
		t.Fatal("commit failure did not mark the batch failed")
	}
	if len(events) != 0 {
		t.Fatalf("events = %#v, want no plan update", events)
	}
	if got := agent.CurrentPlan(); !slices.Equal(got, existing) {
		t.Fatalf("plan = %#v, want it untouched %#v", got, existing)
	}
	if got := planResultTextOf(t, msg, 0); got != "tool result state could not be persisted" {
		t.Fatalf("result text = %q", got)
	}
}

// TestExecuteToolsDeniedPlanCallChangesNothing covers a guard or confirmation
// denial: Execute never runs, so nothing is previewed, persisted, committed, or
// emitted.
func TestExecuteToolsDeniedPlanCallChangesNothing(t *testing.T) {
	existing := []PlanStep{planStep("existing", PlanPending)}
	agent := NewAgent(nil, "test", "", Registry{
		"plan": &planCallTool{ops: []PlanOperation{{Action: "clear"}}},
	})
	agent.SetPlan(existing)
	agent.BeforeToolExecute = func(provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return false, "denied by guard", nil
	}
	var commitCalls int
	agent.CommitToolResult = func(_ string, result ToolResult) error {
		commitCalls++
		if result.PlanOperation != nil {
			t.Fatalf("denied call reached persistence with state: %#v", result)
		}
		return nil
	}

	var events []AgentEvent
	_, hadError := agent.executeTools(context.Background(), planToolMessage("call-1"), func(ev AgentEvent) {
		if _, ok := ev.(EvPlanUpdate); ok {
			events = append(events, ev)
		}
	})
	if !hadError {
		t.Fatal("denial did not mark the batch failed")
	}
	if commitCalls != 1 {
		t.Fatalf("commit hook calls = %d, want 1 without plan state", commitCalls)
	}
	if len(events) != 0 {
		t.Fatalf("events = %#v, want no plan update", events)
	}
	if got := agent.CurrentPlan(); !slices.Equal(got, existing) {
		t.Fatalf("plan = %#v, want it untouched %#v", got, existing)
	}
}

// TestExecuteToolsRefusesPlanArgumentRewrite covers a guard that tries to rewrite
// the plan arguments: the call fails, and state stays untouched.
func TestExecuteToolsRefusesPlanArgumentRewrite(t *testing.T) {
	existing := []PlanStep{planStep("existing", PlanPending)}
	agent := NewAgent(nil, "test", "", Registry{
		"plan": &planCallTool{ops: []PlanOperation{{Action: "clear"}}},
	})
	agent.SetPlan(existing)
	agent.BeforeToolExecute = func(provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return true, "", json.RawMessage(`{"action":"clear"}`)
	}

	var events []AgentEvent
	msg, hadError := agent.executeTools(context.Background(), planToolMessage("call-1"), func(ev AgentEvent) {
		if _, ok := ev.(EvPlanUpdate); ok {
			events = append(events, ev)
		}
	})
	if !hadError {
		t.Fatal("rewrite refusal did not mark the batch failed")
	}
	if len(events) != 0 {
		t.Fatalf("events = %#v, want no plan update", events)
	}
	if got := agent.CurrentPlan(); !slices.Equal(got, existing) {
		t.Fatalf("plan = %#v, want it untouched %#v", got, existing)
	}
	if got := planResultTextOf(t, msg, 0); got != "tool arguments cannot be rewritten" {
		t.Fatalf("result text = %q", got)
	}
}

// TestExecuteToolsBatchCommitsOnlyPersistedCalls covers a batch whose second
// persistence fails: the first call is persisted, committed, and emitted; the
// second changes nothing and reports the failure.
func TestExecuteToolsBatchCommitsOnlyPersistedCalls(t *testing.T) {
	agent := NewAgent(nil, "test", "", Registry{
		"plan": &planCallTool{ops: []PlanOperation{
			{Action: "set", Steps: []PlanStep{planStep("first", PlanPending)}},
			{Action: "add", Steps: []PlanStep{planStep("second", PlanPending)}},
		}},
	})
	var commitCalls int
	agent.CommitToolResult = func(string, ToolResult) error {
		commitCalls++
		if commitCalls == 2 {
			return errors.New("disk full")
		}
		return nil
	}

	var events []AgentEvent
	msg, hadError := agent.executeTools(context.Background(), planToolMessage("call-1", "call-2"), func(ev AgentEvent) {
		if _, ok := ev.(EvPlanUpdate); ok {
			events = append(events, ev)
		}
	})
	if !hadError {
		t.Fatal("failed second commit did not mark the batch failed")
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want only the first call's update", len(events))
	}
	if planEvent, ok := events[0].(EvPlanUpdate); !ok || planEvent.CallID != "call-1" {
		t.Fatalf("event = %#v, want call-1's update", events[0])
	}
	want := []PlanStep{planStep("first", PlanPending)}
	if got := agent.CurrentPlan(); !slices.Equal(got, want) {
		t.Fatalf("plan = %#v, want only the committed call %#v", got, want)
	}
	if got := planResultTextOf(t, msg, 1); got != "tool result state could not be persisted" {
		t.Fatalf("second result text = %q", got)
	}
}

// TestExecuteToolsShowReadsWithoutPersisting covers the read action: the model
// sees the listing, and nothing is committed or persisted for it.
func TestExecuteToolsShowReadsWithoutPersisting(t *testing.T) {
	plan := []PlanStep{planStep("done", PlanCompleted), planStep("todo", PlanPending)}
	agent := NewAgent(nil, "test", "", Registry{
		"plan": &planCallTool{ops: []PlanOperation{{Action: "show"}}},
	})
	agent.SetPlan(plan)
	var commitCalls int
	agent.CommitToolResult = func(_ string, result ToolResult) error {
		commitCalls++
		if result.PlanOperation != nil || result.Details != nil {
			t.Fatalf("show result reached persistence with state: %#v", result)
		}
		return nil
	}

	var events []AgentEvent
	msg, hadError := agent.executeTools(context.Background(), planToolMessage("call-1"), func(ev AgentEvent) {
		if _, ok := ev.(EvPlanUpdate); ok {
			events = append(events, ev)
		}
	})
	if hadError {
		t.Fatal("show marked the batch failed")
	}
	if commitCalls != 1 {
		t.Fatalf("commit hook calls = %d, want 1", commitCalls)
	}
	if len(events) != 0 {
		t.Fatalf("events = %#v, want no plan update", events)
	}
	if got := agent.CurrentPlan(); !slices.Equal(got, plan) {
		t.Fatalf("plan = %#v, want it untouched", got)
	}
	if got := planResultTextOf(t, msg, 0); got != "1. [x] done\n2. [ ] todo" {
		t.Fatalf("result text = %q", got)
	}
}

// TestExecuteToolsAppliesPlanBatchInOrder covers two plan calls in one assistant
// message: each is previewed, persisted, committed, and emitted in call order.
func TestExecuteToolsAppliesPlanBatchInOrder(t *testing.T) {
	agent := NewAgent(nil, "test", "", Registry{
		"plan": &planCallTool{ops: []PlanOperation{
			{Action: "set", Steps: []PlanStep{planStep("one", PlanPending), planStep("two", PlanPending)}},
			{Action: "add", Steps: []PlanStep{planStep("three", PlanPending)}},
		}},
	})
	var committed []PlanUpdate
	agent.CommitToolResult = func(_ string, result ToolResult) error {
		committed = append(committed, result.Details.(PlanUpdate))
		return nil
	}

	var callIDs []string
	msg, hadError := agent.executeTools(context.Background(), planToolMessage("call-1", "call-2"), func(ev AgentEvent) {
		if planEvent, ok := ev.(EvPlanUpdate); ok {
			callIDs = append(callIDs, planEvent.CallID)
		}
	})
	if hadError {
		t.Fatal("batch marked failed")
	}
	if len(committed) != 2 || len(committed[0].Plan) != 2 || len(committed[1].Plan) != 3 {
		t.Fatalf("committed = %#v, want a 2-step then a 3-step plan", committed)
	}
	want := []PlanStep{planStep("one", PlanPending), planStep("two", PlanPending), planStep("three", PlanPending)}
	if got := agent.CurrentPlan(); !slices.Equal(got, want) {
		t.Fatalf("plan = %#v, want %#v", got, want)
	}
	if len(callIDs) != 2 || callIDs[0] != "call-1" || callIDs[1] != "call-2" {
		t.Fatalf("plan events = %#v, want call-1 then call-2", callIDs)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("tool results = %d, want 2", len(msg.Content))
	}
}

// TestExecuteToolsRejectsUnknownActionFromCore covers the one interface-table
// error the tool may hand through: core owns the unknown-action message.
func TestExecuteToolsRejectsUnknownActionFromCore(t *testing.T) {
	agent := NewAgent(nil, "test", "", Registry{
		"plan": &planCallTool{ops: []PlanOperation{{Action: "foo"}}},
	})
	if got := planResultTextOf(t, mustExecuteTools(t, agent, "call-1"), 0); got != `plan: unknown action "foo"` {
		t.Fatalf("result text = %q", got)
	}
}

func mustExecuteTools(t *testing.T, agent *Agent, ids ...string) provider.Message {
	t.Helper()
	msg, _ := agent.executeTools(context.Background(), planToolMessage(ids...), func(AgentEvent) {})
	return msg
}
