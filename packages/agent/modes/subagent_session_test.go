package modes

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
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
	session := newResidentChildSession(journalPort(t, root), "child", tui.Dark)
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
	session := newResidentChildSession(nil, "child", tui.Dark)
	session.composer.SetValue("follow up")
	prompt, submit := session.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if !submit || prompt != "follow up" {
		t.Fatalf("submission = (%q, %v)", prompt, submit)
	}
	if got := session.composer.Value(); got != "follow up" {
		t.Fatalf("composer cleared before durable acceptance: %q", got)
	}
	session.FinishSubmission(errors.New("rejected"))
	if got := session.composer.Value(); got != "follow up" {
		t.Fatalf("composer cleared after rejection: %q", got)
	}
	session.FinishSubmission(nil)
	if got := session.composer.Value(); got != "" {
		t.Fatalf("composer after acceptance = %q", got)
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
	session := newResidentChildSession(journalPort(t, root), spec.ID, tui.Dark)
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

func TestResidentChildSessionPagesOlderHistoryThroughPort(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	msg := func(text string) subagents.ResidentHistoryItem {
		raw, err := json.Marshal(provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: text}}})
		if err != nil {
			t.Fatal(err)
		}
		return subagents.ResidentHistoryItem{Type: "message.assistant", Message: raw}
	}
	port.EXPECT().ResidentHistory(mock.Anything, "child", "", 5).Return(subagents.ResidentHistoryPage{Items: []subagents.ResidentHistoryItem{msg("newest")}, OlderCursor: "c1"}, nil).Once()
	port.EXPECT().ResidentHistory(mock.Anything, "child", "c1", 5).Return(subagents.ResidentHistoryPage{Items: []subagents.ResidentHistoryItem{msg("oldest")}}, nil).Once()
	session := newResidentChildSession(port, "child", tui.Dark)
	if err := session.LoadAll(5, nil); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(session.View().Build(80), "\n")
	if first, last := strings.Index(got, "oldest"), strings.Index(got, "newest"); first < 0 || last < first {
		t.Fatalf("paged history = %q, want oldest before newest", got)
	}
}

func TestResidentChildSessionReportsPortError(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().ResidentHistory(mock.Anything, "child", "", 200).Return(subagents.ResidentHistoryPage{}, errors.New("unknown child")).Once()
	session := newResidentChildSession(port, "child", tui.Dark)
	if !session.BeginLoad() {
		t.Fatal("load not accepted")
	}
	session.FinishLoad(session.LoadAll(200, nil))
	if lines := session.Render(80, 10); !containsLine(lines, "history: unknown child") {
		t.Fatalf("render = %q, want port error", lines)
	}
}

func TestResidentChildSessionHeaderUsesSessionSummary(t *testing.T) {
	session := newResidentChildSession(nil, "child", tui.Dark)
	session.SetSummary(residentSummaryFixture("child", string(subagents.ResidentIdle)), true)
	if lines := session.Render(80, 10); !containsLine(lines, "child  completed  reviewer  worktree") {
		t.Fatalf("header = %q", lines)
	}
}

func residentSummaryFixture(id, state string) session.ResidentSummary {
	return session.ResidentSummary{ID: id, State: state, Profile: "reviewer", Workspace: "worktree"}
}

// The session publishes an info event per resident update; an open child view
// reloads its history from that event alone (no timer, no render-time call).
func TestInteractiveResidentInfoEventRefreshesOpenChildSession(t *testing.T) {
	root := t.TempDir()
	writeResidentJournal(t, root, "child", core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "first"}}}})
	port := journalPort(t, root)
	interactive := newQueuePortInteractive(t, port, session.Snapshot{Info: session.Info{ID: "s", Residents: []session.ResidentSummary{{ID: "child", State: string(subagents.ResidentRunning)}}}}, InteractiveConfig{Theme: tui.Dark})
	interactive.openResidentChildSession("child")
	child := interactive.residentChildSession
	waitInteractive(t, interactive, func() bool { return residentLoadIdle(child) && containsLine(child.Render(100, 30), "first") })

	writeResidentJournal(t, root, "child", core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "finalized later"}}}})
	interactive.mu.Lock()
	interactive.applyInfoLocked(session.Info{ID: "s", Residents: []session.ResidentSummary{{ID: "child", State: string(subagents.ResidentIdle)}}})
	interactive.mu.Unlock()
	waitInteractive(t, interactive, func() bool { return containsLine(child.Render(100, 30), "finalized later") })
	if lines := child.Render(100, 30); !containsLine(lines, "completed") {
		t.Fatalf("header did not follow session summary: %q", lines)
	}
}

// Streaming deltas republish info with the same state and history revision;
// only a state change or a history revision bump may reload the open view.
func TestInteractiveResidentInfoEventReloadsOnlyOnHistoryChange(t *testing.T) {
	// No ResidentHistory expectation for the unchanged events: the mock fails
	// the test if a reload reaches the port.
	port := sessionmocks.NewMockPort(t)
	interactive := &Interactive{}
	child := newResidentChildSession(port, "child", tui.Dark)
	interactive.residentChildSession = child
	running := session.ResidentSummary{ID: "child", State: string(subagents.ResidentRunning), HistoryRev: 3}
	child.SetSummary(running, true)
	interactive.portInfo = session.Info{Residents: []session.ResidentSummary{running}}

	streamed := running
	streamed.UpdatedAt = time.Date(2026, time.October, 30, 12, 0, 0, 0, time.UTC)
	previous := interactive.portInfo.Residents
	interactive.portInfo = session.Info{Residents: []session.ResidentSummary{streamed}}
	if interactive.residentInfoChangedLocked(previous) {
		t.Fatal("live-only update scheduled a history reload")
	}

	// Hold the reservation so the scheduled reload coalesces instead of
	// reaching the port; the return value is the synchronous signal.
	if !child.BeginLoad() {
		t.Fatal("reservation not accepted")
	}
	appended := streamed
	appended.HistoryRev = 4
	previous = interactive.portInfo.Residents
	interactive.portInfo = session.Info{Residents: []session.ResidentSummary{appended}}
	if !interactive.residentInfoChangedLocked(previous) {
		t.Fatal("history revision bump did not schedule a reload")
	}
}

func TestResidentListChangedIgnoresLiveOnlyFields(t *testing.T) {
	base := []session.ResidentSummary{{ID: "a", State: "running", HistoryRev: 1}}
	live := []session.ResidentSummary{{ID: "a", State: "running", HistoryRev: 1, UpdatedAt: time.Unix(1, 0)}}
	if residentListChanged(base, live) {
		t.Fatal("timestamp-only change refetched the picker")
	}
	for _, next := range [][]session.ResidentSummary{
		nil,
		{{ID: "b", State: "running", HistoryRev: 1}},
		{{ID: "a", State: "idle", HistoryRev: 1}},
		{{ID: "a", State: "running", HistoryRev: 2}},
	} {
		if !residentListChanged(base, next) {
			t.Fatalf("change to %+v not detected", next)
		}
	}
}

// ReloadRecent keeps the current transcript visible until the new one is
// complete.
func TestResidentChildSessionReloadKeepsMessagesUntilReady(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	item := func(text string) subagents.ResidentHistoryItem {
		raw, err := json.Marshal(provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: text}}})
		if err != nil {
			t.Fatal(err)
		}
		return subagents.ResidentHistoryItem{Type: "message.assistant", Message: raw}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	port.EXPECT().ResidentHistory(mock.Anything, "child", "", 200).RunAndReturn(func(context.Context, string, string, int) (subagents.ResidentHistoryPage, error) {
		close(entered)
		<-release
		return subagents.ResidentHistoryPage{Items: []subagents.ResidentHistoryItem{item("old"), item("new")}}, nil
	}).Once()
	child := newResidentChildSession(port, "child", tui.Dark)
	child.view.Messages = []provider.Message{{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "old"}}}}
	done := make(chan error, 1)
	go func() { done <- child.ReloadRecent(200) }()
	<-entered
	if got := strings.Join(child.View().Build(80), "\n"); !strings.Contains(got, "old") {
		t.Fatalf("view emptied during reload: %q", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(child.View().Build(80), "\n"); !strings.Contains(got, "new") {
		t.Fatalf("reloaded view = %q", got)
	}
}

// waitInteractive consumes redraw requests until cond holds. Async history
// loads and picker fetches signal completion through invalidate.
func waitInteractive(t *testing.T, interactive *Interactive, cond func() bool) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for !cond() {
		select {
		case <-interactive.dirty:
		case <-timeout:
			t.Fatal("condition not reached before timeout")
		}
	}
}

func residentLoadIdle(child *residentChildSession) bool {
	child.mu.Lock()
	defer child.mu.Unlock()
	return !child.loading
}

func TestInteractiveInputDownOpensResidentPickerAndEnterOpensLiveChild(t *testing.T) {
	spec := subagents.ResidentChildSpec{ID: "child"}
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().ResidentList(mock.Anything, mock.Anything, mock.Anything).Return([]subagents.ResidentSnapshot{{ID: spec.ID, State: subagents.ResidentIdle}}, 1, nil)
	port.EXPECT().ResidentHistory(mock.Anything, spec.ID, "", 200).Return(subagents.ResidentHistoryPage{}, nil).Maybe()
	interactive := newQueuePortInteractive(t, port, session.Snapshot{Info: session.Info{ID: "s", Residents: []session.ResidentSummary{{ID: spec.ID, State: string(subagents.ResidentIdle)}}}}, InteractiveConfig{Theme: tui.Dark})
	interactive.ed.SetValue("draft")

	interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyDown})
	if !interactive.residentSubagentsDialog.Active() {
		t.Fatal("Down did not open the resident subagent picker")
	}
	if got := interactive.ed.Value(); got != "draft" {
		t.Fatalf("editor value = %q, want draft preserved", got)
	}
	waitInteractive(t, interactive, func() bool {
		interactive.mu.Lock()
		defer interactive.mu.Unlock()
		return interactive.residentSubagentsDialog.loaded
	})
	interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	if interactive.residentSubagentsDialog.Active() {
		t.Fatal("Enter left the resident subagent picker open")
	}
	if interactive.residentChildSession == nil || interactive.residentChildSession.childID != spec.ID {
		t.Fatalf("resident child session = %#v, want %q", interactive.residentChildSession, spec.ID)
	}
	child := interactive.residentChildSession
	waitInteractive(t, interactive, func() bool { return residentLoadIdle(child) })
}

func containsLine(lines []string, needle string) bool {
	for _, line := range lines {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

// journalPort serves ResidentHistory from on-disk resident journals, as the
// daemon does, so child-session tests exercise real history pages.
func journalPort(t *testing.T, root string) *sessionmocks.MockPort {
	t.Helper()
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().ResidentHistory(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, childID, cursor string, limit int) (subagents.ResidentHistoryPage, error) {
		return subagents.ReadResidentHistoryPage(filepath.Join(root, childID), cursor, limit)
	}).Maybe()
	return port
}

func writeResidentJournal(t *testing.T, root, childID string, events ...core.AgentEvent) {
	t.Helper()
	journal, err := subagents.OpenResidentJournal(root, childID)
	if err != nil {
		t.Fatal(err)
	}
	spec := subagents.ResidentChildSpec{ID: childID, SessionID: "session", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "task"); err != nil && !strings.Contains(err.Error(), "already") {
		t.Fatal(err)
	}
	for _, ev := range events {
		if err := journal.RecordAgentEvent(ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
}

// /subagents kill and send run ResidentAction off the input goroutine and
// report through the status line once it returns.
func TestSubagentsSlashActionsRunAsynchronously(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	interactive := newQueuePortInteractive(t, port, session.Snapshot{}, InteractiveConfig{Theme: tui.Dark})
	release := make(chan struct{})
	port.EXPECT().ResidentAction(mock.Anything, session.ResidentCommand{Op: "stop", ChildID: "child"}).RunAndReturn(func(context.Context, session.ResidentCommand) error {
		<-release
		return nil
	}).Once()
	port.EXPECT().ResidentAction(mock.Anything, session.ResidentCommand{Op: "resume", ChildID: "child", Prompt: "go on"}).Return(errors.New("busy")).Once()

	// Returns while the daemon call is still blocked.
	interactive.runResidentSubagents(context.Background(), []string{"kill", "child"})
	close(release)
	waitInteractive(t, interactive, func() bool { ok, _ := statusOf(interactive); return ok == "stopped child" })

	interactive.runResidentSubagents(context.Background(), []string{"send", "child", "go", "on"})
	waitInteractive(t, interactive, func() bool { _, errMsg := statusOf(interactive); return errMsg == "send: busy" })
}

func TestSubagentsSlashResultSubcommandRemoved(t *testing.T) {
	interactive := newQueuePortInteractive(t, sessionmocks.NewMockPort(t), session.Snapshot{}, InteractiveConfig{Theme: tui.Dark})
	interactive.runResidentSubagents(context.Background(), []string{"result", "child"})
	interactive.mu.Lock()
	defer interactive.mu.Unlock()
	if !strings.Contains(interactive.statusErr, "list / new / logs / kill / send / resume") {
		t.Fatalf("status = %q, want usage without result", interactive.statusErr)
	}
}

func statusOf(interactive *Interactive) (ok, errMsg string) {
	interactive.mu.Lock()
	defer interactive.mu.Unlock()
	return interactive.statusOK, interactive.statusErr
}
