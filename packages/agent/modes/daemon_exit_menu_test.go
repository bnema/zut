package modes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestDaemonExitMenuPromptAndCancel(t *testing.T) {
	for _, busy := range []bool{false, true} {
		for _, key := range []tui.Key{{Kind: tui.KeyEsc}, {Kind: tui.KeyRune, Rune: 'x'}} {
			i := NewInteractive(InteractiveConfig{Port: sessionmocks.NewMockPort(t)})
			i.busy = busy
			require.False(t, i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlC}))
			require.True(t, i.daemonExitMenu)
			require.Equal(t, daemonExitPrompt, i.statusOK)
			require.Equal(t, busy, i.busy) // No Port.Abort call is expected by the mock.
			require.False(t, i.handleKey(context.Background(), key))
			require.False(t, i.daemonExitMenu)
			require.Empty(t, i.statusOK)
			require.True(t, i.ed.IsEmpty(), "cancel key must be consumed")
		}
	}
}

func TestDaemonExitMenuClearsEditorFirst(t *testing.T) {
	i := NewInteractive(InteractiveConfig{Port: sessionmocks.NewMockPort(t)})
	i.ed.SetValue("draft")
	require.False(t, i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlC}))
	require.False(t, i.daemonExitMenu)
	require.True(t, i.ed.IsEmpty())
	require.False(t, i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlC}))
	require.True(t, i.daemonExitMenu)
}

func TestDaemonExitMenuDetach(t *testing.T) {
	r := startLifecycle(t, newMockSessionClient(t))
	r.term.send("\x03d")
	r.requireDetached(t)
}

func TestDaemonExitMenuQuit(t *testing.T) {
	client := newMockSessionClient(t)
	client.EXPECT().Close(mock.Anything, session.ID("s1"), portCloseReasonUserExit).Return(nil).Once()
	r := startLifecycle(t, client)
	r.term.send("\x03q")
	r.waitExit(t)
	require.Empty(t, r.detach)
}

func TestDaemonExitMenuQuitError(t *testing.T) {
	client := newMockSessionClient(t)
	client.EXPECT().Close(mock.Anything, session.ID("s1"), portCloseReasonUserExit).Return(errors.New("boom")).Once()
	r := startLifecycle(t, client)
	r.term.send("\x03q")
	require.Eventually(t, func() bool { r.i.mu.Lock(); defer r.i.mu.Unlock(); return r.i.statusErr != "" }, 5*time.Second, time.Millisecond)
	select {
	case <-r.done:
		t.Fatal("Q must stay open on close failure")
	default:
	}
}

func TestDaemonExitMenuForceError(t *testing.T) {
	client := newMockSessionClient(t)
	client.EXPECT().Close(mock.Anything, session.ID("s1"), portCloseReasonUserExit).Return(errors.New("boom")).Once()
	r := startLifecycle(t, client)
	r.term.send("\x03\x03\x03")
	r.waitExit(t)
	require.Contains(t, r.i.ExitNotice(), "boom")
	require.Empty(t, r.detach)
}

func TestDaemonExitMenuForceTimeout(t *testing.T) {
	client := newMockSessionClient(t)
	client.EXPECT().Close(mock.Anything, session.ID("s1"), portCloseReasonUserExit).RunAndReturn(func(ctx context.Context, _ session.ID, _ string) error { <-ctx.Done(); return ctx.Err() }).Once()
	i := NewInteractive(InteractiveConfig{Port: sessionmocks.NewMockPort(t), Client: client})
	i.sessionID = "s1"
	i.sessionTimeout = time.Millisecond
	require.False(t, i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlC}))
	require.True(t, i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlC}))
	require.Contains(t, i.ExitNotice(), "deadline exceeded")
}
