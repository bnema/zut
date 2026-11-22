package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

// advisorTranscript is the full rendering the reviewer sends on a fresh
// advisor conversation.
func advisorTranscript(system string, tools []provider.Tool, msgs []provider.Message, pendingCallID string) string {
	return SerializeAdvisorContext(system, tools, msgs) + "\n=== transcript ===\n" + SerializeAdvisorMessages(msgs, pendingCallID)
}

func TestSerializeAdvisorTranscript(t *testing.T) {
	tools := []provider.Tool{
		{Name: "read", Description: "Read a file", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "hidden", Description: "deferred", Deferred: true},
		{Name: "late", Description: "activated later", Deferred: true},
	}
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "fix the bug"}, provider.ImageBlock{MimeType: "image/png", Data: []byte("abc")}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.ReasoningBlock{Summary: "secret thoughts"},
			provider.ToolCallBlock{ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path":"a.go"}`)},
		}},
		{Role: provider.RoleTool, AddedToolNames: []string{"late"}, Content: []provider.Content{
			provider.ToolResultBlock{CallID: "c1", IsError: true, Content: []provider.Content{provider.TextBlock{Text: "no such file"}}},
		}},
		{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.TextBlock{Text: "asking advisor"},
			provider.ToolCallBlock{ID: "c2", Name: "read", Arguments: json.RawMessage(`{"path":"b.go"}`)},
			provider.ToolCallBlock{ID: "adv", Name: "advisor", Arguments: json.RawMessage(`{}`)},
		}},
	}
	got := advisorTranscript("SYSTEM PROMPT", tools, msgs, "adv")

	for _, want := range []string{
		"SYSTEM PROMPT",
		"## read\nRead a file\nschema: {\"type\":\"object\"}",
		"## late",
		"fix the bug",
		"[image: image/png, 3 bytes]",
		`[tool_call id=c1 read {"path":"a.go"}]` + "\n",
		"[tool_result id=c1 error]\nno such file",
		"asking advisor",
		`[tool_call id=c2 read {"path":"b.go"}] (result pending)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"secret thoughts", "id=adv", "## hidden"} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected %q in:\n%s", bad, got)
		}
	}
}

func TestSerializeAdvisorTranscriptSkipsEmptyAssistant(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "hi"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "adv", Name: "advisor"}}},
	}
	got := advisorTranscript("", nil, msgs, "adv")
	if strings.Contains(got, "--- assistant ---") {
		t.Fatalf("empty assistant header rendered:\n%s", got)
	}
}

func TestCostTrackerAddExternalKeepsLastTurn(t *testing.T) {
	var c CostTracker
	c.Add(provider.Usage{InputTokens: 100, CostUSD: 1})
	total := c.AddExternal(provider.Usage{InputTokens: 50, CostUSD: 0.5})
	if total.InputTokens != 150 || total.CostUSD != 1.5 {
		t.Fatalf("total=%#v", total)
	}
	if c.LastTurn.InputTokens != 100 || c.LastTurn.CostUSD != 1 {
		t.Fatalf("last turn changed: %#v", c.LastTurn)
	}
	var empty CostTracker
	if got := empty.AddExternal(provider.Usage{CostUSD: 2}); got.CostUSD != 2 {
		t.Fatalf("empty total=%#v", got)
	}
}

func TestToolUsageFoldsIntoTotalOnly(t *testing.T) {
	var sawMessages, sawSystem bool
	var gotCallID string
	var a *Agent
	tool := &invokingTestTool{name: "advisor", execute: func(ctx context.Context) ToolResult {
		// Snapshot accessors must be safe from inside Execute.
		msgs := a.Messages()
		system, reg := a.PromptConfig()
		sawMessages = len(msgs) >= 2
		sawSystem = system == "SYS" && reg["advisor"] != nil
		gotCallID, _ = ToolCallIDFrom(ctx)
		return ToolResult{
			Content: []provider.Content{provider.TextBlock{Text: "advice"}},
			Usage:   &provider.Usage{InputTokens: 40, OutputTokens: 10, CostUSD: 0.25},
		}
	}}
	client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
		{content: []provider.Content{provider.ToolCallBlock{ID: "adv-1", Name: "advisor"}}, stop: provider.StopToolUse},
		{content: []provider.Content{textContent("done", "")}, stop: provider.StopEnd},
	}}
	a = NewAgent(client, "test", "SYS", Registry{"advisor": tool})
	a.SeedLastTurnUsage(provider.Usage{InputTokens: 1000})
	a.SeedCost(provider.Usage{InputTokens: 1000, CostUSD: 1})
	var onUsage []provider.Usage
	var lastBefore, lastAtFold provider.Usage
	a.OnUsage = func(cum provider.Usage) {
		onUsage = append(onUsage, cum)
		if len(onUsage) == 1 {
			lastBefore = a.LastTurnUsage()
		}
		if len(onUsage) == 2 {
			lastAtFold = a.LastTurnUsage()
		}
	}

	var events []AgentEvent
	if err := runPrompt(t, a, &events); err != nil {
		t.Fatal(err)
	}
	if !sawMessages || !sawSystem || gotCallID != "adv-1" {
		t.Fatalf("snapshot messages=%v system=%v callID=%q", sawMessages, sawSystem, gotCallID)
	}
	if got := a.Cost(); got.CostUSD != 1.25 {
		t.Fatalf("cost=%#v", got)
	}
	var external []EvUsage
	var lastTurn provider.Usage
	var turnInput int
	for _, ev := range events {
		switch e := ev.(type) {
		case EvUsage:
			if e.External {
				external = append(external, e)
			} else {
				lastTurn = e.Usage
				turnInput += e.Usage.InputTokens
			}
		case EvToolResult:
			if e.Result.Usage != nil {
				t.Fatalf("usage leaked into tool result event")
			}
		}
	}
	if len(external) != 1 || external[0].Usage.CostUSD != 0.25 || external[0].Cumulative.CostUSD != 1.25 {
		t.Fatalf("external usage events=%#v", external)
	}
	if got := a.Cost().InputTokens; got != 1000+turnInput+40 {
		t.Fatalf("input total=%d turns=%d", got, turnInput)
	}
	if got := a.LastTurnUsage(); got != lastTurn {
		t.Fatalf("last turn=%#v want model turn %#v", got, lastTurn)
	}
	// onUsage[0] is turn 1, onUsage[1] the external fold, onUsage[2] turn 2.
	if len(onUsage) != 3 || onUsage[1].CostUSD != 1.25 || lastAtFold != lastBefore {
		t.Fatalf("OnUsage=%#v lastBefore=%#v lastAtFold=%#v", onUsage, lastBefore, lastAtFold)
	}
	if onUsage[len(onUsage)-1].CostUSD != 1.25 {
		t.Fatalf("OnUsage=%#v", onUsage)
	}
}

func TestNestedToolUsageFoldsOnce(t *testing.T) {
	child := &invokingTestTool{name: "child", execute: func(context.Context) ToolResult {
		return ToolResult{Usage: &provider.Usage{CostUSD: 0.5}}
	}}
	var nested NestedOutcome
	parent := &invokingTestTool{name: "parent", execute: func(ctx context.Context) ToolResult {
		inv, _ := NestedInvokerFrom(ctx)
		nested = inv.Invoke(ctx, "child", nil)
		return ToolResult{}
	}}
	client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
		{content: []provider.Content{provider.ToolCallBlock{ID: "p", Name: "parent"}}, stop: provider.StopToolUse},
		{content: []provider.Content{textContent("done", "")}, stop: provider.StopEnd},
	}}
	a := NewAgent(client, "test", "", Registry{"parent": parent, "child": child})
	var events []AgentEvent
	if err := runPrompt(t, a, &events); err != nil {
		t.Fatal(err)
	}
	if nested.Result.Usage != nil {
		t.Fatal("nested usage not cleared")
	}
	if got := a.Cost().CostUSD; got != 0.5 {
		t.Fatalf("cost=%v", got)
	}
}
