package subagents

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestResidentHandoffStaysEmptyForNewResults(t *testing.T) {
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
		core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "Verification failed: service down."}}}},
	} {
		if err := journal.RecordAgentEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	turnErr := errors.New("turn failed")
	if err := journal.RecordTurnFinished(spec, spec.InitialTurnID, turnErr); err != nil {
		t.Fatal(err)
	}
	result, err := journal.Result()
	if err != nil {
		t.Fatal(err)
	}
	// Handoff is deprecated: new results stay empty, so no raw tool
	// payload can leak through the terminal projection. The completion
	// update carries the production summary instead. Archived v2
	// handoffs remain readable.
	if result.Handoff != "" {
		t.Fatalf("new result carries handoff %q, want empty", result.Handoff)
	}
	if result.Summary != "Verification failed: service down." {
		t.Fatalf("new result summary = %q", result.Summary)
	}
	update := FormatCompletionUpdate([]Completion{(ResidentCompletion{ChildID: spec.ID, TurnID: spec.InitialTurnID, Err: turnErr, Summary: result.Summary}).Completion()}, "")
	if !strings.Contains(update, "Verification failed: service down.") {
		t.Fatalf("completion update missing summary: %q", update)
	}
	for _, secret := range []string{"synthetic-argument-secret", "synthetic-output-secret", "Authorization", "curl"} {
		if strings.Contains(update, secret) {
			t.Fatalf("completion update disclosed raw tool payload %q", secret)
		}
	}
	items, err := ReadResidentHistory(journal.Dir(), 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 || items[len(items)-1].Type != residentRecordAssistant {
		t.Fatalf("history tail = %#v, want latest assistant message", items)
	}
	// The bounded history page starts at the latest prompt; the full
	// secrets remain in the authoritative transcript records.
	records, err := ReadResidentJournal(filepath.Join(journal.Dir(), residentTranscriptName))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(records)
	if err != nil || !strings.Contains(string(raw), "synthetic-argument-secret") || !strings.Contains(string(raw), "synthetic-output-secret") {
		t.Fatal("empty handoff must not destroy durable recovery history")
	}
}
