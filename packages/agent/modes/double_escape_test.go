package modes

import (
	"context"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
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

func TestSessionTreeGateFailureDoesNotFlushOrOpenOverlay(t *testing.T) {
	i := NewInteractive(InteractiveConfig{})
	flushes := 0
	i.cfg.FlushSession = func() { flushes++ }

	i.doSessionTree()
	if flushes != 0 {
		t.Fatalf("FlushSession called %d times on a failed gate", flushes)
	}
	if i.sessionTreeDialog.Active() {
		t.Fatal("session tree overlay opened after a failed gate")
	}
}

func TestSessionTreeGateRejectsModelRefreshInFlight(t *testing.T) {
	root := t.TempDir()
	cwd := "/workspace/model-refresh-gate"
	session, err := core.NewSession(root, cwd, "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	message := provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "message"}}}
	if err := session.AppendMessage(message); err != nil {
		t.Fatal(err)
	}
	path := session.Path
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	i := NewInteractive(InteractiveConfig{
		Agent:              core.NewAgent(nil, "model", "", nil),
		CWD:                cwd,
		SessionsRoot:       root,
		CurrentSessionPath: func() string { return path },
		LoadSession:        func(string) error { return nil },
	})
	i.agent.SetMessages([]provider.Message{message})
	i.mu.Lock()
	i.modelRefreshing = true
	i.mu.Unlock()
	if i.canOpenSessionTree() {
		t.Fatal("tree gate opened while model refresh was in flight")
	}
}

func TestSessionTreeSelectionGateAllowsActiveTree(t *testing.T) {
	root := t.TempDir()
	cwd := "/workspace/selection-gate"
	session, err := core.NewSession(root, cwd, "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	message := provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "message"}}}
	if err := session.AppendMessage(message); err != nil {
		t.Fatal(err)
	}
	path := session.Path
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	ag := core.NewAgent(nil, "model", "", nil)
	ag.SetMessages([]provider.Message{message})
	i := NewInteractive(InteractiveConfig{
		Agent:        ag,
		CWD:          cwd,
		SessionsRoot: root,
		CurrentSessionPath: func() string {
			return path
		},
		LoadSession: func(string) error { return nil },
	})
	i.sessionTreeDialog.active = true
	if i.canOpenSessionTree() {
		t.Fatal("open gate accepted an already-active tree")
	}
	if !i.canCommitSessionTreeSelection() {
		t.Fatal("selection gate rejected the active tree dialog")
	}
}

func TestDoubleEscapeOpensReadableTreeThroughSharedGate(t *testing.T) {
	root := t.TempDir()
	cwd := "/workspace/double-escape"
	session, err := core.NewSession(root, cwd, "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	message := provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "draft"}}}
	if err := session.AppendMessage(message); err != nil {
		t.Fatal(err)
	}
	path := session.Path
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	ag := core.NewAgent(nil, "model", "", nil)
	ag.SetMessages([]provider.Message{message})
	now := time.Unix(200, 0)
	flushes := 0
	i := NewInteractive(InteractiveConfig{
		Agent:        ag,
		CWD:          cwd,
		SessionsRoot: root,
		CurrentSessionPath: func() string {
			return path
		},
		LoadSession: func(string) error { return nil },
		FlushSession: func() {
			flushes++
		},
	})
	i.clock = func() time.Time { return now }

	if done := i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("first Escape exited")
	}
	if i.sessionTreeDialog.Active() || flushes != 0 {
		t.Fatalf("first Escape opened/flushed: active=%v flushes=%d", i.sessionTreeDialog.Active(), flushes)
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
	if flushes != 1 {
		t.Fatalf("FlushSession called %d times, want once in the background", flushes)
	}
	if got := len(ag.Messages()); got != 1 {
		t.Fatalf("double Escape changed transcript with a provider turn: %d messages", got)
	}
}

func TestSecondEscapeFallsThroughToCancellationWhenTurnBecomesBusy(t *testing.T) {
	root := t.TempDir()
	cwd := "/workspace/double-escape-busy"
	session, err := core.NewSession(root, cwd, "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	message := provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "draft"}}}
	if err := session.AppendMessage(message); err != nil {
		t.Fatal(err)
	}
	path := session.Path
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	ag := core.NewAgent(nil, "model", "", nil)
	ag.SetMessages([]provider.Message{message})
	now := time.Unix(300, 0)
	cancelled := 0
	i := NewInteractive(InteractiveConfig{
		Agent:              ag,
		CWD:                cwd,
		SessionsRoot:       root,
		CurrentSessionPath: func() string { return path },
		LoadSession:        func(string) error { return nil },
	})
	i.clock = func() time.Time { return now }

	if done := i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("first Escape exited")
	}
	i.mu.Lock()
	i.busy = true
	i.cancelTurn = func() { cancelled++ }
	i.mu.Unlock()
	now = now.Add(100 * time.Millisecond)
	if done := i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("second Escape exited")
	}
	if cancelled != 1 {
		t.Fatalf("second Escape did not fall through to cancellation: got %d cancels", cancelled)
	}
	if i.sessionTreeDialog.Active() {
		t.Fatal("session tree opened after the turn became busy")
	}
	if i.doubleEscape.armed {
		t.Fatal("stale double-Escape gesture remained armed after falling through")
	}
}

func TestSecondEscapeStillConsumesFailClosedTreeGate(t *testing.T) {
	root := t.TempDir()
	cwd := "/workspace/double-escape-gate"
	session, err := core.NewSession(root, cwd, "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	message := provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "draft"}}}
	if err := session.AppendMessage(message); err != nil {
		t.Fatal(err)
	}
	path := session.Path
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	ag := core.NewAgent(nil, "model", "", nil)
	ag.SetMessages([]provider.Message{message})
	now := time.Unix(400, 0)
	pathCalls := 0
	flushes := 0
	i := NewInteractive(InteractiveConfig{
		Agent:        ag,
		CWD:          cwd,
		SessionsRoot: root,
		CurrentSessionPath: func() string {
			pathCalls++
			if pathCalls == 1 {
				return path
			}
			// Keep the cheap gesture eligibility true while making the
			// asynchronous family read fail after the overlay opens.
			return path + ".missing"
		},
		LoadSession: func(string) error { return nil },
		FlushSession: func() {
			flushes++
		},
	})
	i.clock = func() time.Time { return now }

	if done := i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("first Escape exited")
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
	if flushes != 1 {
		t.Fatalf("FlushSession called %d times, want one asynchronous flush", flushes)
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
