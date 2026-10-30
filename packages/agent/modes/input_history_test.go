package modes

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newHistoryPortInteractive attaches cfg to a mocked session port whose
// snapshot carries msgs, the transcript input history browses.
func newHistoryPortInteractive(t *testing.T, msgs []provider.Message, cfg InteractiveConfig) *Interactive {
	t.Helper()
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{
		Info:     session.Info{ID: "history", Run: session.RunStateIdle},
		Seq:      1,
		Messages: msgs,
	}, nil).Once()
	port.EXPECT().Subscribe(mock.Anything, uint64(1)).Return(make(chan session.Event), nil).Once()
	cfg.Port = port
	i := NewInteractive(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, i.attachPort(ctx))
	return i
}

func TestInputHistoryUsesUpDown(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "first"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "reply"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "second"}}},
	}
	i := newHistoryPortInteractive(t, msgs, InteractiveConfig{})

	if !i.handleInputHistoryKey(tui.Key{Kind: tui.KeyUp}) {
		t.Fatal("expected Up to enter history")
	}
	if got := i.ed.Value(); got != "second" {
		t.Fatalf("Up loaded %q, want newest history item", got)
	}

	if !i.handleInputHistoryKey(tui.Key{Kind: tui.KeyUp}) {
		t.Fatal("expected repeated Up to stay in history")
	}
	if got := i.ed.Value(); got != "first" {
		t.Fatalf("second Up loaded %q, want older history item", got)
	}

	if !i.handleInputHistoryKey(tui.Key{Kind: tui.KeyDown}) {
		t.Fatal("expected Down to move forward in history")
	}
	if got := i.ed.Value(); got != "second" {
		t.Fatalf("Down loaded %q, want newer history item", got)
	}

	if !i.handleInputHistoryKey(tui.Key{Kind: tui.KeyDown}) {
		t.Fatal("expected Down at newest item to clear editor")
	}
	if got := i.ed.Value(); got != "" {
		t.Fatalf("Down past newest loaded %q, want empty editor", got)
	}
}

func TestInputHistorySkipsShellEscapeContext(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "prompt"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "$ pwd\n\n/tmp\n\n[exit 0]"}}, Meta: map[string]string{shellEscapeMetaKey: "true"}},
	}
	i := newHistoryPortInteractive(t, msgs, InteractiveConfig{})

	history := i.inputHistory()
	if len(history) != 1 || history[0] != "prompt" {
		t.Fatalf("inputHistory() = %q, want [prompt]", history)
	}
}

func TestInputHistorySkipsCompactionContext(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "prompt"}}},
		{Role: provider.RoleUser, Meta: map[string]string{"compaction": "true"}, Content: []provider.Content{provider.TextBlock{Text: "## Context Summary (compacted)"}}},
		{Role: provider.RoleUser, Meta: map[string]string{autoCompactContinueMetaKey: "true"}, Content: []provider.Content{provider.TextBlock{Text: "resume internally"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "new prompt"}}},
	}
	i := newHistoryPortInteractive(t, msgs, InteractiveConfig{})

	history := i.inputHistory()
	if len(history) != 2 || history[0] != "prompt" || history[1] != "new prompt" {
		t.Fatalf("inputHistory() = %q, want [prompt new prompt]", history)
	}
}

func TestJumpTargetsSkipInternalUserContext(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "prompt"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "call-1", Name: "read"}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "call-1"}}},
		{Role: provider.RoleUser, Meta: map[string]string{"compaction": "true"}, Content: []provider.Content{provider.TextBlock{Text: "## Context Summary (compacted)"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "new prompt"}}},
	}

	targets := buildJumpTargets(msgs)
	if len(targets) != 2 {
		t.Fatalf("jump target count = %d, want 2", len(targets))
	}
	if targets[0].TurnNo != 1 || targets[0].ToolCount != 1 || targets[1].TurnNo != 2 {
		t.Fatalf("jump targets = %+v, want real turns and tool count", targets)
	}
}

func TestInputHistoryNoLongerUsesLeftRight(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "prompt"}}},
	}
	i := newHistoryPortInteractive(t, msgs, InteractiveConfig{})

	if i.handleInputHistoryKey(tui.Key{Kind: tui.KeyLeft}) {
		t.Fatal("Left should not browse input history")
	}
	if i.handleInputHistoryKey(tui.Key{Kind: tui.KeyRight}) {
		t.Fatal("Right should not browse input history")
	}
	if got := i.ed.Value(); got != "" {
		t.Fatalf("left/right changed editor to %q", got)
	}
}

func TestInputHistoryDoesNotStealNonEmptyEditor(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "prompt"}}},
	}
	i := newHistoryPortInteractive(t, msgs, InteractiveConfig{})
	i.ed.SetValue("draft")

	if i.handleInputHistoryKey(tui.Key{Kind: tui.KeyUp}) {
		t.Fatal("Up should not browse history while editing a draft")
	}
	if got := i.ed.Value(); got != "draft" {
		t.Fatalf("editor changed to %q, want draft", got)
	}
}
