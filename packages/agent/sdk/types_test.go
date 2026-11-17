package sdk

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestToEventPreservesCacheDiagnostics(t *testing.T) {
	for _, diagnostics := range []core.EvCacheDiagnostics{
		{Eligible: true, Mode: "automatic", Transport: "http_sse", Continuation: "full"},
		{Eligible: false, Mode: "below_minimum", Transport: "websocket", Continuation: "incremental"},
	} {
		event := toEvent(diagnostics)
		if event.Type != "cache_diagnostics" || event.CacheDiagnostics == nil {
			t.Fatalf("event = %#v", event)
		}
		got := event.CacheDiagnostics
		if got.Eligible != diagnostics.Eligible || got.Mode != diagnostics.Mode || got.Transport != diagnostics.Transport || got.Continuation != diagnostics.Continuation {
			t.Fatalf("cache diagnostics = %#v, want %#v", got, diagnostics)
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(encoded) {
			t.Fatalf("encoded diagnostics are invalid JSON: %s", encoded)
		}
		if !diagnostics.Eligible && !strings.Contains(string(encoded), `"eligible":false`) {
			t.Fatalf("encoded ineligible diagnostics = %s, want eligible=false", encoded)
		}
	}
}

func TestToEventPreservesPlanUpdate(t *testing.T) {
	explanation := "Discovery complete"
	event := toEvent(core.EvPlanUpdate{CallID: "call-7", Update: core.PlanUpdate{
		Explanation: &explanation,
		Plan: []core.PlanStep{
			{Step: "Implement", Status: core.PlanInProgress},
			{Step: "Verify", Status: core.PlanPending},
		},
	}})
	if event.Type != "plan_update" || event.ID != "call-7" || event.Explanation == nil || *event.Explanation != explanation {
		t.Fatalf("plan event = %#v", event)
	}
	if event.Plan == nil || len(*event.Plan) != 2 || (*event.Plan)[0].Status != "in_progress" {
		t.Fatalf("plan payload = %#v", event.Plan)
	}

	empty := toEvent(core.EvPlanUpdate{Update: core.PlanUpdate{Plan: []core.PlanStep{}}})
	encoded, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"plan":[]`) {
		t.Fatalf("empty plan JSON = %s, want explicit empty list", encoded)
	}
	if strings.Contains(string(encoded), `"id"`) {
		t.Fatalf("call id should be omitted when empty: %s", encoded)
	}
}

func TestToEventPreservesActivityAndToolStreamFacts(t *testing.T) {
	retry := toEvent(core.EvRetryScheduled{
		Scope: core.RetryScopeProvider, Attempt: 2, MaxAttempts: 3, Delay: 250 * time.Millisecond, Reason: "idle timeout (1/1)",
	})
	if retry.Reason != "idle timeout (1/1)" || retry.Message != nil {
		t.Fatalf("retry reason = %q, message = %#v", retry.Reason, retry.Message)
	}
	if retry.Type != "retry_scheduled" || retry.Scope != "provider" || retry.Attempt != 2 || retry.MaxAttempts != 3 || retry.DelayMS == nil || *retry.DelayMS != 250 {
		t.Fatalf("retry event = %#v", retry)
	}

	zeroDelay := toEvent(core.EvRetryScheduled{Scope: core.RetryScopeProvider, Attempt: 2, MaxAttempts: 3})
	encoded, err := json.Marshal(zeroDelay)
	if err != nil {
		t.Fatalf("marshal zero-delay retry: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("decode zero-delay retry: %v", err)
	}
	if got, ok := wire["delay_ms"]; !ok || got != float64(0) {
		t.Fatalf("zero-delay retry JSON delay_ms = %#v, want 0", got)
	}

	toolStart := toEvent(core.EvToolUseStart{ID: "call-1", Name: "read"})
	if toolStart.Type != "tool_use_start" || toolStart.ID != "call-1" || toolStart.Name != "read" {
		t.Fatalf("tool start = %#v", toolStart)
	}
	toolArgs := toEvent(core.EvToolUseArgs{ID: "call-1", Delta: `{"path":"a"`})
	if toolArgs.Type != "tool_use_args" || toolArgs.ID != "call-1" || toolArgs.Delta == "" {
		t.Fatalf("tool args = %#v", toolArgs)
	}
	toolEnd := toEvent(core.EvToolExecutionStarted{ID: "call-1", Name: "read"})
	if toolEnd.Type != "tool_execution_started" || toolEnd.ID != "call-1" || toolEnd.Name != "read" {
		t.Fatalf("tool execution = %#v", toolEnd)
	}
}

func TestToEventNestedTools(t *testing.T) {
	started := toEvent(core.EvNestedToolStarted{ID: "p/1", ParentID: "p", Name: "read", Args: json.RawMessage(`{"path":"a"}`)})
	if started.Type != "nested_tool_started" || started.ID != "p/1" || started.ParentID != "p" || started.Name != "read" || string(started.Args) != `{"path":"a"}` {
		t.Fatalf("started=%#v", started)
	}
	result := toEvent(core.EvNestedToolResult{ID: "p/1", ParentID: "p", Name: "read", IsError: true, Denied: true, Summary: "denied"})
	if result.Type != "nested_tool_result" || result.ID != "p/1" || result.ParentID != "p" || result.Name != "read" || !result.IsError || !result.Denied || result.Summary != "denied" {
		t.Fatalf("result=%#v", result)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["parent_id"] != "p" || fields["denied"] != true || fields["summary"] != "denied" {
		t.Fatalf("wire=%s", raw)
	}
	raw, err = json.Marshal(toEvent(core.EvToolExecutionStarted{ID: "p", Name: "parent"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"parent_id", "denied", "summary"} {
		if strings.Contains(string(raw), `"`+field+`"`) {
			t.Fatalf("optional %s included: %s", field, raw)
		}
	}
}

func TestToEventExternalUsage(t *testing.T) {
	if toEvent(core.EvUsage{}).External {
		t.Fatal("model usage marked external")
	}
	if !toEvent(core.EvUsage{External: true}).External {
		t.Fatal("external flag lost")
	}
}

func TestToEventServerTool(t *testing.T) {
	ev := toEvent(core.EvServerTool{ID: "srvtoolu_test1", Name: "advisor", Phase: provider.ServerToolPhaseResult, Text: "advice", ErrorCode: "x", Redacted: true})
	if ev.Type != "server_tool" || ev.ID != "srvtoolu_test1" || ev.Name != "advisor" || ev.Phase != "result" || ev.Text != "advice" || ev.ErrorCode != "x" || !ev.Redacted {
		t.Fatalf("event = %+v", ev)
	}
	msg := toEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{
		provider.ServerToolBlock{Provider: "anthropic", Kind: provider.ServerToolKindAdvisorResult, ID: "srvtoolu_test1", Name: "advisor", Redacted: true, Raw: json.RawMessage(`{"encrypted_content":"ZW5jcnlwdGVk"}`)},
	}}})
	if msg.Message == nil || len(msg.Message.Content) != 1 {
		t.Fatalf("message = %+v", msg.Message)
	}
	b := msg.Message.Content[0]
	if b.Type != "server_tool" || b.Kind != provider.ServerToolKindAdvisorResult || b.ID != "srvtoolu_test1" || !b.Redacted {
		t.Fatalf("block = %+v", b)
	}
	if raw, _ := json.Marshal(msg); strings.Contains(string(raw), "ZW5jcnlwdGVk") {
		t.Fatalf("raw leaked: %s", raw)
	}
	// Older consumers see no new fields on unrelated events.
	if raw, _ := json.Marshal(toEvent(core.EvToolUseEnd{ID: "c"})); strings.Contains(string(raw), "phase") || strings.Contains(string(raw), "redacted") {
		t.Fatalf("unexpected fields: %s", raw)
	}
}
