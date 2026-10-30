package modes

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func applySessionTreeLoads(d *sessionTreeDialog, events <-chan sessionTreeLoadEvent) error {
	var loadErr error
	for event := range events {
		if err := d.ApplyLoad(event); err != nil {
			loadErr = err
		}
	}
	d.ApplyLoadClosed()
	return loadErr
}

func TestInteractiveCtrlCExitMarker(t *testing.T) {
	i := NewInteractive(InteractiveConfig{})
	key := tui.Key{Kind: tui.KeyCtrlC}
	if i.handleKey(context.Background(), key) {
		t.Fatal("first Ctrl+C exited interactive mode")
	}
	if i.ExitedViaCtrlC() {
		t.Fatal("first Ctrl+C marked an exit")
	}
	if !i.handleKey(context.Background(), key) {
		t.Fatal("second Ctrl+C did not exit interactive mode")
	}
	if !i.ExitedViaCtrlC() {
		t.Fatal("Ctrl+C exit was not marked")
	}
}

func TestDoubleEscapeTrackerWindowAndReset(t *testing.T) {
	base := time.Unix(100, 0)
	var tracker doubleEscapeTracker

	tracker.Arm(base)
	if !tracker.Consume(base.Add(sessionTreeEscapeWindow)) {
		t.Fatal("tap at the 500ms boundary should count")
	}
	if tracker.Consume(base.Add(sessionTreeEscapeWindow)) {
		t.Fatal("a consumed gesture must not trigger again")
	}

	tracker.Arm(base)
	if tracker.Consume(base.Add(sessionTreeEscapeWindow + time.Nanosecond)) {
		t.Fatal("tap after the gesture window should not count")
	}
	tracker.Arm(base)
	if tracker.Consume(base.Add(-time.Nanosecond)) {
		t.Fatal("a clock moving backwards must reset the gesture")
	}

	// Zero is a valid value for a deterministic test clock; it must not be
	// confused with the tracker's unarmed sentinel.
	tracker.Arm(time.Time{})
	if !tracker.Consume(time.Time{}.Add(100 * time.Millisecond)) {
		t.Fatal("zero-valued test timestamp should still arm the gesture")
	}
}

func TestUnmodifiedEscapeRequiresBareParsedKey(t *testing.T) {
	bare := tui.Key{Kind: tui.KeyEsc}
	for _, key := range []tui.Key{
		{Kind: tui.KeyEsc, Alt: true},
		{Kind: tui.KeyEsc, Ctrl: true},
		{Kind: tui.KeyEsc, Shift: true},
		{Kind: tui.KeyEsc, Super: true},
	} {
		if isUnmodifiedEscape(key) {
			t.Fatalf("modified key %#v was treated as a bare Escape", key)
		}
	}
	if !isUnmodifiedEscape(bare) {
		t.Fatal("bare parsed Escape was not recognized")
	}
}

func TestSessionTreeSelectionRestoresCompleteUserDraft(t *testing.T) {
	image := provider.ImageBlock{MimeType: "image/png", Data: []byte("png"), ThoughtSignature: "sig"}
	msgs := []provider.Message{
		{
			Role: provider.RoleUser,
			Content: []provider.Content{
				provider.TextBlock{Text: "before"},
				image,
				provider.TextBlock{Text: "after"},
			},
		},
	}
	target := sessionTreeTarget{
		EffectiveIndex:    0,
		SelectionBoundary: 0,
		Role:              provider.RoleUser,
		UserDraft:         "fallback must not replace message text",
	}

	got, err := sessionTreeSelection(msgs, target)
	if err != nil {
		t.Fatalf("sessionTreeSelection: %v", err)
	}
	if got.upTo != 0 || !got.restoreDraft {
		t.Fatalf("selection = %#v, want a pre-user draft boundary", got)
	}
	if got.draftText != "before\nafter [clipboard image #1] " {
		t.Fatalf("draft text = %q", got.draftText)
	}
	if len(got.images) != 1 || got.images[0].Marker != "[clipboard image #1]" {
		t.Fatalf("restored images = %#v", got.images)
	}
	if string(got.images[0].Image.Data) != "png" || got.images[0].Image.MimeType != image.MimeType || got.images[0].Image.ThoughtSignature != image.ThoughtSignature {
		t.Fatalf("restored image = %#v", got.images[0].Image)
	}
}

func TestSessionTreeSelectionKeepsToolExchangeAtomic(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "ask"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "call-1", Name: "read"}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "call-1"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done"}}},
	}

	got, err := sessionTreeSelection(msgs, sessionTreeTarget{
		EffectiveIndex:    1,
		SelectionBoundary: 2,
		Role:              provider.RoleAssistant,
	})
	if err != nil {
		t.Fatalf("tool-call selection: %v", err)
	}
	if got.upTo != 3 {
		t.Fatalf("tool-call boundary = %d, want 3", got.upTo)
	}

	got, err = sessionTreeSelection(msgs, sessionTreeTarget{
		EffectiveIndex:    2,
		SelectionBoundary: 3,
		Role:              provider.RoleTool,
	})
	if err != nil {
		t.Fatalf("tool-result selection: %v", err)
	}
	if got.upTo != 3 {
		t.Fatalf("tool-result boundary = %d, want 3", got.upTo)
	}
}

func TestSessionTreeSelectionAllowsDeferredToolActivation(t *testing.T) {
	msgs := []provider.Message{
		{
			Role:           provider.RoleTool,
			AddedToolNames: []string{"lookup_weather"},
			Content:        []provider.Content{provider.ToolResultBlock{CallID: "call-1"}},
		},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "next"}}},
	}
	got, err := sessionTreeSelection(msgs, sessionTreeTarget{
		EffectiveIndex:    0,
		SelectionBoundary: 1,
		Role:              provider.RoleTool,
	})
	if err != nil {
		t.Fatalf("deferred-tool selection: %v", err)
	}
	if got.upTo != 1 {
		t.Fatalf("deferred-tool boundary = %d, want 1", got.upTo)
	}
}

// newTreePortInteractive attaches to an idle session whose file at path
// holds msgs (SessionsRoot root, CWD cwd), as tui_client.go does.
func newTreePortInteractive(t *testing.T, root, cwd string, msgs []provider.Message) (*Interactive, *sessionmocks.MockPort, *mockSessionClient, string) {
	t.Helper()
	path := newTreeTestSession(t, root, cwd, msgs)
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{
		Info:     session.Info{ID: "original", CWD: cwd, SessionPath: path, Run: session.RunStateIdle},
		Seq:      1,
		Messages: msgs,
	}, nil).Once()
	port.EXPECT().Subscribe(mock.Anything, uint64(1)).Return(make(chan session.Event), nil).Once()
	client := newMockSessionClient(t)
	i := NewInteractive(InteractiveConfig{Port: port, Client: client, SessionsRoot: root})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	i.runCtx = ctx
	require.NoError(t, i.attachPort(ctx))
	return i, port, client, path
}

func TestSessionTreeGateFailureDoesNotFlushOrOpenOverlay(t *testing.T) {
	// No session file: the gate fails closed without reaching the client.
	port := sessionmocks.NewMockPort(t)
	i := NewInteractive(InteractiveConfig{Port: port, Client: newMockSessionClient(t), SessionsRoot: t.TempDir()})
	i.portInfo = session.Info{Run: session.RunStateIdle, CWD: "/workspace"}

	i.doSessionTree()
	if i.sessionTreeDialog.Active() {
		t.Fatal("session tree overlay opened after a failed gate")
	}
}

func TestSessionTreeGateRejectsModelRefreshInFlight(t *testing.T) {
	i, _, _, _ := newTreePortInteractive(t, t.TempDir(), "/workspace/model-refresh-gate", []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "message"}}}})
	if !i.canOpenSessionTree() {
		t.Fatal("fixture gate should open while idle")
	}
	i.mu.Lock()
	i.modelRefreshing = true
	i.mu.Unlock()
	if i.canOpenSessionTree() {
		t.Fatal("tree gate opened while model refresh was in flight")
	}
}

func TestSessionTreeSelectionGateAllowsActiveTree(t *testing.T) {
	i, _, _, _ := newTreePortInteractive(t, t.TempDir(), "/workspace/selection-gate", []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "message"}}}})
	i.sessionTreeDialog.active = true
	if i.canOpenSessionTree() {
		t.Fatal("open gate accepted an already-active tree")
	}
	if !i.canCommitSessionTreeSelection() {
		t.Fatal("selection gate rejected the active tree dialog")
	}
}

func TestDoubleEscapeOpensReadableTreeThroughSharedGate(t *testing.T) {
	// No Submit expectation: the gesture never starts a provider turn.
	i, _, _, _ := newTreePortInteractive(t, t.TempDir(), "/workspace/double-escape", []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "draft"}}}})
	now := time.Unix(200, 0)
	i.clock = func() time.Time { return now }

	if done := i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("first Escape exited")
	}
	if i.sessionTreeDialog.Active() {
		t.Fatal("first Escape opened the tree")
	}
	now = now.Add(499 * time.Millisecond)
	if done := i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("second Escape exited")
	}
	if !i.sessionTreeDialog.Active() {
		t.Fatal("session tree did not open before its background load")
	}
	if err := applySessionTreeLoads(i.sessionTreeDialog, i.sessionTreeLoads); err != nil {
		t.Fatalf("background session tree load: %v", err)
	}
}

func TestSecondEscapeFallsThroughToCancellationWhenTurnBecomesBusy(t *testing.T) {
	i, port, _, _ := newTreePortInteractive(t, t.TempDir(), "/workspace/double-escape-busy", []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "draft"}}}})
	now := time.Unix(300, 0)
	i.clock = func() time.Time { return now }
	aborted := make(chan struct{})
	port.EXPECT().SlideBackQueued(mock.Anything).Return(session.Input{}, session.ErrUnknownRequest).Once()
	port.EXPECT().Abort(mock.Anything).RunAndReturn(func(context.Context) error { close(aborted); return nil }).Once()

	if done := i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("first Escape exited")
	}
	i.mu.Lock()
	i.applyInfoLocked(session.Info{ID: "original", CWD: i.portInfo.CWD, SessionPath: i.portInfo.SessionPath, Run: session.RunStateRunning})
	i.mu.Unlock()
	now = now.Add(100 * time.Millisecond)
	if done := i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("second Escape exited")
	}
	runLoopCall(t, i)
	waitSignal(t, aborted, "Abort")
	if i.sessionTreeDialog.Active() {
		t.Fatal("session tree opened after the turn became busy")
	}
	if i.doubleEscape.armed {
		t.Fatal("stale double-Escape gesture remained armed after falling through")
	}
}

func TestSecondEscapeStillConsumesFailClosedTreeGate(t *testing.T) {
	i, _, _, path := newTreePortInteractive(t, t.TempDir(), "/workspace/double-escape-gate", []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "draft"}}}})
	now := time.Unix(400, 0)
	i.clock = func() time.Time { return now }

	if done := i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("first Escape exited")
	}
	// Keep the cheap gesture eligibility true while making the asynchronous
	// family read fail after the overlay opens.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	now = now.Add(100 * time.Millisecond)
	if done := i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("second Escape exited")
	}
	if !i.sessionTreeDialog.Active() {
		t.Fatal("tree did not open before the asynchronous family validation")
	}
	if err := applySessionTreeLoads(i.sessionTreeDialog, i.sessionTreeLoads); err == nil {
		t.Fatal("unreadable family did not fail in the background")
	}
	if i.sessionTreeDialog.Active() {
		t.Fatal("unreadable family left the tree overlay active")
	}
}

func TestForkCommandsWithoutSessionPathReportError(t *testing.T) {
	for _, command := range []string{"/fork", "/session fork"} {
		i := NewInteractive(InteractiveConfig{})
		i.runSlash(context.Background(), command)
		if i.statusErr != "fork: no session is active (running with --no-session?)" {
			t.Fatalf("%s status = %q", command, i.statusErr)
		}
	}
}
