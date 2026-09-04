package subagents

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestResidentHandoffOmitsRawToolSecrets(t *testing.T) {
	journal, err := OpenResidentJournal(t.TempDir(), "handoff-secrets")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	spec := ResidentChildSpec{ID: "handoff-secrets", SessionID: "session", InitialTurnID: "turn", Provider: "openai", Model: "test"}
	if err := journal.Accept(spec, "check service"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnStarted(spec, spec.InitialTurnID); err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"command":"curl -H 'Authorization: Bearer synthetic-argument-secret' localhost"}`)
	for _, event := range []core.AgentEvent{
		core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "Service verification is incomplete."}, provider.ToolCallBlock{ID: "call", Name: "bash", Arguments: args}}}},
		core.EvToolCall{ID: "call", Name: "bash", Args: args},
		core.EvToolResult{ID: "call", Result: core.ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: "response token=synthetic-output-secret"}}}},
	} {
		if err := journal.RecordAgentEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.RecordTurnFinished(spec, spec.InitialTurnID, ErrBudgetExceeded); err != nil {
		t.Fatal(err)
	}
	result, err := journal.Result()
	if err != nil {
		t.Fatal(err)
	}
	update := FormatCompletionUpdate([]Completion{(ResidentCompletion{ChildID: spec.ID, TurnID: spec.InitialTurnID, Err: ErrBudgetExceeded, Summary: result.Handoff}).Completion()}, "")
	for _, text := range []string{result.Handoff, update} {
		for _, secret := range []string{"synthetic-argument-secret", "synthetic-output-secret", "Authorization", "curl"} {
			if strings.Contains(text, secret) {
				t.Fatalf("handoff disclosed raw tool payload %q", secret)
			}
		}
		for _, want := range []string{"Service verification is incomplete.", "tool bash (call)", "is_error=true", HistoryRef(spec.ID)} {
			if !strings.Contains(text, want) {
				t.Fatalf("handoff missing safe metadata %q: %s", want, text)
			}
		}
	}
	items, err := ReadResidentHistory(journal.Dir(), 16)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(items)
	if err != nil || !strings.Contains(string(raw), "synthetic-argument-secret") || !strings.Contains(string(raw), "synthetic-output-secret") {
		t.Fatal("omitting automatic payloads must not destroy durable recovery history")
	}
}
