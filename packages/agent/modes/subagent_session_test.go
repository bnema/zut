package modes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func TestResidentChildSessionBuildsSharedTranscriptView(t *testing.T) {
	root := t.TempDir()
	journal, err := subagents.OpenResidentJournal(root, "child")
	if err != nil {
		t.Fatal(err)
	}
	spec := subagents.ResidentChildSpec{ID: "child", SessionID: "session", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "call", Name: "bash", Arguments: json.RawMessage(`{"command":"pwd"}`)}}}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvToolResult{ID: "call", Result: core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "/repo"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	manager := subagents.NewResidentManager(root, func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(context.Context, string) error { return nil }, nil
	})
	session := newResidentChildSession(manager, "child", tui.Dark)
	if err := session.LoadRecent(20); err != nil {
		t.Fatal(err)
	}
	view := session.View()
	if view == nil || !view.ExpandAll || len(view.Messages) != 2 {
		t.Fatalf("view = %#v", view)
	}
	lines := view.Build(80)
	if len(lines) == 0 {
		t.Fatal("shared view rendered no child history")
	}
}

func TestResidentChildSessionKeepsComposerUntilFollowUpIsAccepted(t *testing.T) {
	root := t.TempDir()
	runs := make(chan string, 2)
	manager := subagents.NewResidentManager(root, func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(_ context.Context, prompt string) error {
			runs <- prompt
			return nil
		}, nil
	})
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	spec := subagents.ResidentChildSpec{ID: "child", SessionID: "session", Provider: "openai", Model: "gpt-5"}
	if _, err := manager.Spawn(context.Background(), spec, "initial task"); err != nil {
		t.Fatal(err)
	}
	if got := <-runs; got != "initial task" {
		t.Fatalf("initial prompt = %q", got)
	}
	session := newResidentChildSession(manager, "child", tui.Dark)
	session.composer.SetValue("follow up")
	prompt, submit := session.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if !submit || prompt != "follow up" {
		t.Fatalf("submission = (%q, %v)", prompt, submit)
	}
	if got := session.composer.Value(); got != "follow up" {
		t.Fatalf("composer cleared before durable acceptance: %q", got)
	}
	if err := manager.Resume(context.Background(), "child", prompt); err != nil {
		t.Fatal(err)
	}
	session.FinishSubmission(nil)
	if got := session.composer.Value(); got != "" {
		t.Fatalf("composer after acceptance = %q", got)
	}
	if got := <-runs; got != "follow up" {
		t.Fatalf("follow-up prompt = %q", got)
	}
}

func TestResidentChildSessionPreservesScrolledViewportAndMarksUnread(t *testing.T) {
	session := newResidentChildSession(nil, "child", tui.Dark)
	session.view.Messages = []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "first"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "first response"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "second"}}},
	}
	_ = session.Render(60, 12)
	session.Scroll(2)
	session.view.Messages = append(session.view.Messages, provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "new response"}}})
	lines := session.Render(60, 12)
	if !containsLine(lines, "new updates below") {
		t.Fatalf("scrolled child session did not mark unread updates: %q", lines)
	}
	session.FollowTail()
	lines = session.Render(60, 12)
	if containsLine(lines, "new updates below") {
		t.Fatalf("following tail retained unread marker: %q", lines)
	}
}

func containsLine(lines []string, needle string) bool {
	for _, line := range lines {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}
