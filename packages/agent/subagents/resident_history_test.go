package subagents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestReadResidentHistoryReturnsCompleteFinalizedItems(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "history-child")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "history-child", SessionID: "history-session", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvUserMessage{Message: provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "question"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvToolCall{ID: "call-1", Name: "bash", Args: json.RawMessage(`{"command":"pwd"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvToolResult{ID: "call-1", Result: core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "/repo"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	history, err := ReadResidentHistory(filepath.Join(root, spec.ID), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[1].ToolName != "bash" || string(history[1].ToolArgs) != `{"command":"pwd"}` || len(history[2].ToolResult) == 0 {
		t.Fatalf("history = %#v", history)
	}
	if _, err := ResidentHistoryDir(root, "../escape"); err == nil {
		t.Fatal("ResidentHistoryDir accepted traversal")
	}
}

func TestResidentHistoryMessagesRestoresToolCallsAndResults(t *testing.T) {
	assistant, err := json.Marshal(provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "call-1", Name: "bash", Arguments: json.RawMessage(`{"command":"pwd"}`)}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "/repo"}}})
	if err != nil {
		t.Fatal(err)
	}
	messages, err := ResidentHistoryMessages([]ResidentHistoryItem{
		{Type: residentRecordAssistant, Message: assistant},
		{Type: residentRecordToolCall, ToolID: "call-1", ToolName: "bash", ToolArgs: json.RawMessage(`{"command":"pwd"}`)},
		{Type: residentRecordToolResult, ToolID: "call-1", ToolResult: result},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	call, ok := messages[0].Content[0].(provider.ToolCallBlock)
	if !ok || call.ID != "call-1" {
		t.Fatalf("assistant call = %#v", messages[0])
	}
	tool, ok := messages[1].Content[0].(provider.ToolResultBlock)
	if !ok || tool.CallID != "call-1" || len(tool.Content) != 1 {
		t.Fatalf("tool message = %#v", messages[1])
	}
}

func TestReadResidentTranscriptMessagesRestoresFinalizedConversation(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "transcript")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "transcript", SessionID: "session", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvUserMessage{Message: provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "question"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	messages, err := ReadResidentTranscriptMessages(filepath.Join(root, spec.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Role != provider.RoleUser {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestReadResidentHistoryPageKeepsToolGroupsTogether(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "history-page")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "history-page", SessionID: "history-session", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	for _, group := range []struct{ id, text string }{{"old", "old answer"}, {"new", "new answer"}} {
		if err := journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: group.text}}}}); err != nil {
			t.Fatal(err)
		}
		if err := journal.RecordAgentEvent(core.EvToolCall{ID: group.id, Name: "bash", Args: json.RawMessage(`{"command":"pwd"}`)}); err != nil {
			t.Fatal(err)
		}
		if err := journal.RecordAgentEvent(core.EvToolResult{ID: group.id, Result: core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "/repo"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	page, err := ReadResidentHistoryPage(filepath.Join(root, spec.ID), "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 || page.Items[0].Type != residentRecordAssistant || page.Items[1].ToolID != "new" || page.Items[2].ToolID != "new" {
		t.Fatalf("latest page split a tool group: %#v", page.Items)
	}
	if page.OlderCursor == "" {
		t.Fatal("latest page has no cursor for the older group")
	}
	older, err := ReadResidentHistoryPage(filepath.Join(root, spec.ID), page.OlderCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(older.Items) != 3 || older.Items[0].Type != residentRecordAssistant || older.Items[1].ToolID != "old" || older.Items[2].ToolID != "old" || older.OlderCursor != "" {
		t.Fatalf("older page = %#v", older)
	}
}

func TestReadResidentHistoryPageDefersIncompleteTailAndInvalidatesReplacedJournal(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "history-cursor")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "history-cursor", SessionID: "history-session", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvUserMessage{Message: provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "saved"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvUserMessage{Message: provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "newer"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, spec.ID, residentTranscriptName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"message.assistant"}`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	page, err := ReadResidentHistoryPage(filepath.Join(root, spec.ID), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Type != residentRecordUser {
		t.Fatalf("incomplete tail leaked into page: %#v", page.Items)
	}
	if page.OlderCursor == "" {
		t.Fatal("page has no cursor before replacement")
	}

	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadResidentHistoryPage(filepath.Join(root, spec.ID), page.OlderCursor, 10); err == nil {
		t.Fatal("replaced journal cursor was accepted")
	}
}

func TestReadResidentHistoryPageCursorSurvivesJournalAppend(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "history-append")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "history-append", SessionID: "history-session", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first", "second"} {
		if err := journal.RecordAgentEvent(core.EvUserMessage{Message: provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: text}}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	page, err := ReadResidentHistoryPage(filepath.Join(root, spec.ID), "", 1)
	if err != nil || page.OlderCursor == "" {
		t.Fatalf("latest page = %#v, err=%v", page, err)
	}
	journal, err = OpenResidentJournal(root, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvUserMessage{Message: provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "third"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	older, err := ReadResidentHistoryPage(filepath.Join(root, spec.ID), page.OlderCursor, 1)
	if err != nil || len(older.Items) != 1 {
		t.Fatalf("older page = %#v, err=%v", older, err)
	}
}

func TestResidentManagerHistoryRejectsPathTraversal(t *testing.T) {
	manager := NewResidentManager(t.TempDir(), func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
		return func(context.Context, string) error { return nil }, nil
	})
	if _, err := manager.History("../escape", 10); err == nil {
		t.Fatal("History accepted traversal")
	}
	if _, err := manager.HistoryPage("../escape", "", 10); err == nil {
		t.Fatal("HistoryPage accepted traversal")
	}
}
