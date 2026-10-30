package subagents

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestResidentJournalKeepsToolCallWithInvalidArgs(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "bad-args")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "bad-args", SessionID: "s", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvToolCall{ID: "call-1", Name: "bash", Args: json.RawMessage("{\"command\":\"a\tb\"}")}); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvToolResult{ID: "call-1", Result: core.ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: "invalid args"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileResidentJournal(filepath.Join(root, spec.ID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}
