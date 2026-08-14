package modes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

func TestResidentChildSessionReloadRecentShowsFinalizedTurn(t *testing.T) {
	root := t.TempDir()
	spec := subagents.ResidentChildSpec{ID: "child", SessionID: "session", Provider: "openai", Model: "gpt-5"}
	journal, err := subagents.OpenResidentJournal(root, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "first"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	manager := subagents.NewResidentManager(root, nil)
	session := newResidentChildSession(manager, spec.ID, tui.Dark)
	if err := session.LoadRecent(20); err != nil {
		t.Fatal(err)
	}
	journal, err = subagents.OpenResidentJournal(root, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "finalized without scrolling"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.ReloadRecent(20); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(session.View().Build(80), "\n"); !strings.Contains(got, "finalized without scrolling") {
		t.Fatalf("reloaded child session omitted finalized turn: %q", got)
	}
}

func TestResidentChildSessionCoalescesCompletionDuringHistoryLoad(t *testing.T) {
	session := newResidentChildSession(nil, "child", tui.Dark)
	if !session.BeginLoad() {
		t.Fatal("initial history load was not accepted")
	}
	if session.RequestRecentReload() {
		t.Fatal("completion refresh started alongside active history load")
	}
	if !session.FinishLoad(nil) {
		t.Fatal("completion refresh was not retained after active history load")
	}
}

func TestResidentChildSessionKeepsReloadReservationUntilFinishLoad(t *testing.T) {
	session := newResidentChildSession(nil, "child", tui.Dark)
	if !session.BeginLoad() {
		t.Fatal("initial history load was not accepted")
	}
	if session.RequestRecentReload() {
		t.Fatal("second history load started alongside active history load")
	}
	// replaceRecent is the tail of the first reader. Its reservation must
	// remain held until the controller consumes the pending reload through
	// FinishLoad, otherwise another durable event can start a concurrent read.
	session.replaceRecent(nil, "")
	if session.RequestRecentReload() {
		t.Fatal("history reload started before the first reader finished")
	}
	if !session.FinishLoad(nil) {
		t.Fatal("coalesced history reload was lost")
	}
	if !session.RequestRecentReload() {
		t.Fatal("coalesced history reload did not reserve after first reader finished")
	}
}

func TestResidentChildSessionRenderRespectsRequestedHeight(t *testing.T) {
	session := newResidentChildSession(nil, "child", tui.Dark)
	for _, height := range []int{0, 1, 2} {
		if lines := session.Render(80, height); len(lines) > height {
			t.Fatalf("height %d rendered %d lines: %#v", height, len(lines), lines)
		}
	}
}

func TestInteractiveResidentChildMouseWheelScrollsHistory(t *testing.T) {
	interactive := NewInteractive(InteractiveConfig{Theme: tui.Dark})
	session := newResidentChildSession(nil, "child", tui.Dark)
	interactive.residentChildSession = session

	interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyMouseWheelUp})
	if session.scrollOffset != 1 {
		t.Fatalf("wheel up scroll offset = %d, want 1", session.scrollOffset)
	}
	interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyMouseWheelDown})
	if session.scrollOffset != 0 {
		t.Fatalf("wheel down scroll offset = %d, want 0", session.scrollOffset)
	}
}

func TestInteractiveResidentChildMouseReportingFollowsOverlay(t *testing.T) {
	term := &alertTestTerminal{}
	interactive := NewInteractive(InteractiveConfig{Terminal: term, Theme: tui.Dark})
	interactive.runCtx = context.Background()
	interactive.setResidentChildMouseReporting(true)
	interactive.setResidentChildMouseReporting(false)
	if got := term.String(); got != tui.SeqMouseOn+tui.SeqMouseOff {
		t.Fatalf("mouse reporting sequences = %q", got)
	}
}

func TestInteractiveResidentUpdatesRefreshOpenChildSession(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{})
	stream := make(chan struct{})
	release := make(chan struct{})
	manager := subagents.NewResidentManager(root, func(_ subagents.ResidentChildSpec, journal *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(context.Context, string) error {
			close(started)
			<-stream
			if err := journal.RecordAgentEvent(core.EvTextDelta{Delta: "streamed without input"}); err != nil {
				return err
			}
			<-release
			return nil
		}, nil
	})
	t.Cleanup(func() {
		close(release)
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	interactive := NewInteractive(InteractiveConfig{ResidentManager: manager, Theme: tui.Dark})
	spec := subagents.ResidentChildSpec{ID: "child", SessionID: "session", Provider: "openai", Model: "gpt-5"}
	if _, err := manager.Spawn(context.Background(), spec, "initial task"); err != nil {
		t.Fatal(err)
	}
	<-started
	interactive.residentChildSession = newResidentChildSession(manager, "child", tui.Dark)
	for {
		select {
		case <-interactive.dirty:
		default:
			goto drained
		}
	}

drained:
	close(stream)
	select {
	case <-interactive.dirty:
	case <-time.After(time.Second):
		t.Fatal("resident stream did not request a redraw")
	}
	lines := interactive.residentChildSession.Render(80, 24)
	if !containsLine(lines, "streamed without input") {
		t.Fatalf("open resident child session did not render streamed text: %q", lines)
	}
}

func TestInteractiveResidentHistoryUpdatesRefreshOpenChildSession(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{})
	persist := make(chan struct{})
	release := make(chan struct{})
	manager := subagents.NewResidentManager(root, func(_ subagents.ResidentChildSpec, journal *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(context.Context, string) error {
			close(started)
			<-persist
			if err := journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "call-1", Name: "bash", Arguments: json.RawMessage(`{"command":"printf fresh"}`)}}}}); err != nil {
				return err
			}
			if err := journal.RecordAgentEvent(core.EvToolResult{ID: "call-1", Result: core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "fresh resident output"}}}}); err != nil {
				return err
			}
			<-release
			return nil
		}, nil
	})
	t.Cleanup(func() {
		close(release)
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	interactive := NewInteractive(InteractiveConfig{ResidentManager: manager, Theme: tui.Dark})
	spec := subagents.ResidentChildSpec{ID: "child", SessionID: "session", Provider: "openai", Model: "gpt-5"}
	if _, err := manager.Spawn(context.Background(), spec, "initial task"); err != nil {
		t.Fatal(err)
	}
	<-started
	session := newResidentChildSession(manager, spec.ID, tui.Dark)
	if err := session.LoadRecent(200); err != nil {
		t.Fatal(err)
	}
	interactive.residentChildSession = session
	close(persist)

	deadline := time.Now().Add(time.Second)
	for {
		if lines := session.Render(100, 30); containsLine(lines, "fresh resident output") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable resident history did not refresh while running: %q", session.Render(100, 30))
		}
		time.Sleep(time.Millisecond)
	}
}

func TestInteractiveResidentHistoryUpdateDoesNotBlockChildControlGoroutine(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{})
	persist := make(chan struct{})
	persisted := make(chan struct{})
	release := make(chan struct{})
	manager := subagents.NewResidentManager(root, func(_ subagents.ResidentChildSpec, journal *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(context.Context, string) error {
			close(started)
			<-persist
			if err := journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "durable update"}}}}); err != nil {
				return err
			}
			close(persisted)
			<-release
			return nil
		}, nil
	})
	t.Cleanup(func() {
		close(release)
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	interactive := NewInteractive(InteractiveConfig{ResidentManager: manager, Theme: tui.Dark})
	spec := subagents.ResidentChildSpec{ID: "child", SessionID: "session", Provider: "openai", Model: "gpt-5"}
	if _, err := manager.Spawn(context.Background(), spec, "initial task"); err != nil {
		t.Fatal(err)
	}
	<-started

	interactive.mu.Lock()
	interactive.residentChildSession = newResidentChildSession(manager, spec.ID, tui.Dark)
	defer interactive.mu.Unlock()
	close(persist)
	select {
	case <-persisted:
	case <-time.After(time.Second):
		t.Fatal("resident history update blocked the child control goroutine")
	}
}

func TestInteractiveInputDownOpensResidentPickerAndEnterOpensLiveChild(t *testing.T) {
	manager := subagents.NewResidentManager(t.TempDir(), func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(context.Context, string) error { return nil }, nil
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
	interactive := NewInteractive(InteractiveConfig{ResidentManager: manager, Theme: tui.Dark})
	interactive.ed.SetValue("draft")

	interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyDown})
	if !interactive.residentSubagentsDialog.Active() {
		t.Fatal("Down did not open the resident subagent picker")
	}
	if got := interactive.ed.Value(); got != "draft" {
		t.Fatalf("editor value = %q, want draft preserved", got)
	}
	interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	if interactive.residentSubagentsDialog.Active() {
		t.Fatal("Enter left the resident subagent picker open")
	}
	if interactive.residentChildSession == nil || interactive.residentChildSession.childID != spec.ID {
		t.Fatalf("resident child session = %#v, want %q", interactive.residentChildSession, spec.ID)
	}
}

func TestInteractiveResidentActivityDrivesIndependentAnimation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	manager := subagents.NewResidentManager(t.TempDir(), func(subagents.ResidentChildSpec, *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return func(context.Context, string) error {
			close(started)
			<-release
			return nil
		}, nil
	})
	t.Cleanup(func() {
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	interactive := NewInteractive(InteractiveConfig{ResidentManager: manager, Theme: tui.Dark})
	if interactive.residentAnimating.Load() {
		t.Fatal("new interactive reported resident animation")
	}
	if _, err := manager.Spawn(context.Background(), subagents.ResidentChildSpec{ID: "indicator", SessionID: "session", Provider: "openai", Model: "gpt-5"}, "task"); err != nil {
		t.Fatal(err)
	}
	<-started
	if !interactive.residentAnimating.Load() {
		t.Fatal("running resident did not enable independent animation")
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for interactive.residentAnimating.Load() {
		if time.Now().After(deadline) {
			t.Fatal("completed resident did not disable independent animation")
		}
		time.Sleep(time.Millisecond)
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
