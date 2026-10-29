package modes

import (
	"context"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func startDeferredPort(t *testing.T, deferred []session.DeferredAction) (*sessionmocks.MockPort, *portRun) {
	t.Helper()
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 8)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", Run: session.RunStateRunning}, Seq: 1, Deferred: deferred}, nil).Once()
	expectPortSubscribe(port, events, nil)
	return port, startPortInteractive(t, port, nil)
}

func twoDeferred() []session.DeferredAction {
	return []session.DeferredAction{
		{ID: "d1", Kind: "confirm", Tool: "bash", Summary: "ls"},
		{ID: "d2", Kind: "question", Summary: "pick?"},
	}
}

func expectResolve(port *sessionmocks.MockPort, id, action string) chan struct{} {
	called := make(chan struct{}, 1)
	port.EXPECT().ResolveDeferred(mock.Anything, id, action).RunAndReturn(func(context.Context, string, string) error {
		called <- struct{}{}
		return nil
	}).Once()
	return called
}

func waitCalled(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("ResolveDeferred not called")
	}
}

func TestInteractivePortDeferredDialogOpensOnAttach(t *testing.T) {
	_, r := startDeferredPort(t, twoDeferred())
	waitFor(t, "deferred dialog", r.i.deferredDialog.Active)
	lines := r.i.deferredDialog.Render(r.i.cfg.Theme, 80)
	require.Contains(t, lines[0], "Deferred actions (2)")
}

func TestInteractivePortDeferredDialogDismiss(t *testing.T) {
	port, r := startDeferredPort(t, twoDeferred())
	called := expectResolve(port, "d1", "dismiss")
	waitFor(t, "deferred dialog", r.i.deferredDialog.Active)
	r.i.deferredDialog.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: 'd'})
	waitCalled(t, called)
	require.True(t, r.i.deferredDialog.Active(), "second action stays listed")
}

func TestInteractivePortDeferredDialogSend(t *testing.T) {
	port, r := startDeferredPort(t, twoDeferred())
	called := expectResolve(port, "d2", "send_to_agent")
	waitFor(t, "deferred dialog", r.i.deferredDialog.Active)
	r.i.deferredDialog.HandleKey(tui.Key{Kind: tui.KeyDown})
	r.i.deferredDialog.HandleKey(tui.Key{Kind: tui.KeyEnter})
	waitCalled(t, called)
}

func TestInteractivePortDeferredDialogEscKeepsPending(t *testing.T) {
	_, r := startDeferredPort(t, twoDeferred())
	waitFor(t, "deferred dialog", r.i.deferredDialog.Active)
	r.i.deferredDialog.HandleKey(tui.Key{Kind: tui.KeyEsc})
	require.False(t, r.i.deferredDialog.Active())
	require.Len(t, r.i.DeferredActions(), 2)
	// The mock fails the test on any unexpected ResolveDeferred call.
	loopBarrier(t, r.i)
}

func TestInteractivePortDeferredDialogEmptyStaysClosed(t *testing.T) {
	_, r := startDeferredPort(t, nil)
	waitFor(t, "attached", func() bool { return r.i.SessionID() == "s1" })
	loopBarrier(t, r.i)
	require.False(t, r.i.deferredDialog.Active())
}
