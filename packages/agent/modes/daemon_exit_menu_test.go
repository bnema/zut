package modes

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	require.NotContains(t, r.i.ExitNotice(), "--resume")
	require.Contains(t, r.i.ExitNotice(), "Check zut ls")
	require.Contains(t, r.i.ExitNotice(), "zut attach s1")
	require.Empty(t, r.detach)
}

func TestDaemonExitMenuForceTimeout(t *testing.T) {
	client := newMockSessionClient(t)
	called := make(chan struct{})
	client.EXPECT().Close(mock.Anything, session.ID("s1"), portCloseReasonUserExit).RunAndReturn(func(ctx context.Context, _ session.ID, _ string) error {
		close(called)
		<-ctx.Done()
		return ctx.Err()
	}).Once()
	i := NewInteractive(InteractiveConfig{Port: sessionmocks.NewMockPort(t), Client: client})
	i.sessionID = "s1"
	i.sessionTimeout = time.Millisecond
	require.False(t, i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlC}))
	require.True(t, i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlC}))
	require.Contains(t, i.ExitNotice(), "deadline exceeded")
	require.NotContains(t, i.ExitNotice(), "--resume")
	require.Contains(t, i.ExitNotice(), "Check zut ls")
	require.Contains(t, i.ExitNotice(), "zut attach s1")
	// The forced exit returns on the deadline without waiting for Close; the
	// Close goroutine may start later, so wait for it before mock cleanup.
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("Close was never called")
	}
}

func TestDaemonQuitPrintsResumeHintAfterCleanup(t *testing.T) {
	for _, keys := range []string{"\x03q", "\x03Q", "\x03\x03", "/exit\r"} {
		t.Run(fmt.Sprintf("%q", keys), func(t *testing.T) {
			client := newMockSessionClient(t)
			client.EXPECT().Close(mock.Anything, session.ID("s1"), portCloseReasonUserExit).Return(nil).Once()
			r := startLifecycle(t, client)
			r.term.send(keys)
			r.waitExit(t)
			require.Empty(t, r.detach)
			require.Equal(t, "Resume this session: zut --resume s1", r.i.ExitNotice())
			require.True(t, strings.HasSuffix(r.term.String(), tui.SeqShowCursor+r.i.ExitNotice()+"\n"), "hint must follow frame cleanup")
		})
	}
}

func TestDaemonClosedEventPrintsResumeHint(t *testing.T) {
	i := NewInteractive(InteractiveConfig{Port: sessionmocks.NewMockPort(t)})
	i.sessionID = "s1"
	require.True(t, i.handleSessionEvent(context.Background(), session.Event{Kind: session.EvClosed, Data: session.ClosedEvent{Reason: portCloseReasonUserExit}}))
	require.Contains(t, i.ExitNotice(), "zut --resume s1")
}
