package modes

import (
	"context"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newPortTitleInteractive attaches a daemon session whose snapshot carries
// title. The terminal title is pure presentation of the daemon's title; the
// daemon owns generation (session.Controller.maybeGenerateTitle).
func newPortTitleInteractive(t *testing.T, term *alertTestTerminal, title string, enabled *bool) (*Interactive, context.Context) {
	t.Helper()
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "title", Run: session.RunStateIdle, Title: title}, Seq: 1}, nil).Once()
	port.EXPECT().Subscribe(mock.Anything, uint64(0x1)).Return(make(chan session.Event), nil).Once()
	i := NewInteractive(InteractiveConfig{Port: port, Terminal: term, TerminalTitleEnabled: enabled})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Run marks the UI started before attaching the port.
	i.markInteractiveStarted()
	require.NoError(t, i.attachPort(ctx))
	return i, ctx
}

func TestPortTitleEventSetsTerminalTitle(t *testing.T) {
	term := &alertTestTerminal{}
	i, ctx := newPortTitleInteractive(t, term, "", nil)
	require.Empty(t, term.String(), "untitled session keeps the default terminal title")
	i.handleSessionEvent(ctx, session.Event{Seq: 2, Kind: session.EvTitle, Data: session.TitleEvent{Title: "Fix login retries"}})
	require.Equal(t, "\x1b]0;zut: Fix login retries\x07", term.String())
}

func TestPortClearedSessionResetsTerminalTitle(t *testing.T) {
	term := &alertTestTerminal{}
	i, _ := newPortTitleInteractive(t, term, "Fix login retries", nil)
	// /clear reattaches the daemon's fresh, untitled successor.
	i.mu.Lock()
	i.hydrateLocked(session.Snapshot{Info: session.Info{ID: "fresh", Run: session.RunStateIdle}, Seq: 1})
	i.mu.Unlock()
	require.True(t, strings.HasSuffix(term.String(), "\x1b]0;zut\x07"), "terminal title output after /clear = %q, want neutral title last", term.String())
	i.mu.Lock()
	defer i.mu.Unlock()
	require.Empty(t, i.sessionTitle)
}

func TestDisabledTerminalTitlesSkipHiddenRequest(t *testing.T) {
	term := &alertTestTerminal{}
	disabled := false
	i, ctx := newPortTitleInteractive(t, term, "", &disabled)
	i.handleSessionEvent(ctx, session.Event{Seq: 2, Kind: session.EvTitle, Data: session.TitleEvent{Title: "should not be shown"}})
	require.Empty(t, term.String())
}

func TestDisablingTerminalTitlesRestoresNeutralTitle(t *testing.T) {
	term := &alertTestTerminal{}
	enabled := true
	i, _ := newPortTitleInteractive(t, term, "Existing work", &enabled)
	i.applySettingToggle("terminal_title_enabled", false)

	got := term.String()
	require.Contains(t, got, "\x1b]0;zut: Existing work\x07")
	require.Contains(t, got[strings.Index(got, "Existing work"):], "\x1b]0;zut\x07", "neutral title must follow the existing one")
}

func TestLoadedSessionTitleIsRestoredWithoutGeneration(t *testing.T) {
	term := &alertTestTerminal{}
	newPortTitleInteractive(t, term, "Existing work", nil)
	require.Equal(t, "\x1b]0;zut: Existing work\x07", term.String())
}
