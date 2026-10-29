package modes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const lifecycleDetachNotice = "detached from s1 — reattach: zut attach s1"

type lifecycleRun struct {
	i      *Interactive
	term   *keyTerminal
	done   chan error
	detach chan struct{}
}

func startLifecycle(t *testing.T, client SessionClient) *lifecycleRun {
	t.Helper()
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 4)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", Run: session.RunStateIdle}}, nil).Once()
	expectPortSubscribe(port, events, nil)
	term := &keyTerminal{in: make(chan byte, 64)}
	r := &lifecycleRun{term: term, done: make(chan error, 1), detach: make(chan struct{}, 4)}
	r.i = NewInteractive(InteractiveConfig{Terminal: term, Port: port, Client: client, Detach: func() { r.detach <- struct{}{} }})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { r.done <- r.i.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		close(term.in)
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return r
}

func (r *lifecycleRun) waitExit(t *testing.T) {
	t.Helper()
	select {
	case err := <-r.done:
		require.NoError(t, err)
		r.done <- nil // let Cleanup observe the return
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

func (r *lifecycleRun) requireDetached(t *testing.T) {
	t.Helper()
	r.waitExit(t)
	select {
	case <-r.detach:
	default:
		t.Fatal("Detach was not called")
	}
	require.Equal(t, lifecycleDetachNotice, r.i.ExitNotice())
}

func TestInteractivePortLifecycleDetachCommand(t *testing.T) {
	r := startLifecycle(t, newMockSessionClient(t))
	r.term.send("/detach\r")
	r.requireDetached(t)
}

func TestInteractivePortLifecycleDetachCtrlDEmpty(t *testing.T) {
	r := startLifecycle(t, newMockSessionClient(t))
	r.term.send("\x04")
	r.requireDetached(t)
}

func TestInteractivePortLifecycleDetachDoubleCtrlC(t *testing.T) {
	r := startLifecycle(t, newMockSessionClient(t))
	r.term.send("\x03\x03")
	r.requireDetached(t)
}

func TestInteractivePortLifecycleExitClosesSession(t *testing.T) {
	client := newMockSessionClient(t)
	client.EXPECT().Close(mock.Anything, session.ID("s1"), portCloseReasonUserExit).Return(nil).Once()
	r := startLifecycle(t, client)
	r.term.send("/exit\r")
	r.waitExit(t)
	require.Empty(t, r.detach)
}

func TestInteractivePortLifecycleExitCloseErrorStaysOpen(t *testing.T) {
	client := newMockSessionClient(t)
	called := make(chan struct{})
	client.EXPECT().Close(mock.Anything, session.ID("s1"), portCloseReasonUserExit).RunAndReturn(func(context.Context, session.ID, string) error {
		close(called)
		return errors.New("boom")
	}).Once()
	r := startLifecycle(t, client)
	r.term.send("/exit\r")
	waitSignal(t, called, "Close")
	require.Eventually(t, func() bool {
		r.i.mu.Lock()
		defer r.i.mu.Unlock()
		return strings.Contains(r.i.statusErr, "boom")
	}, 5*time.Second, 10*time.Millisecond)
	select {
	case <-r.done:
		t.Fatal("Run returned after a failed Close")
	default:
	}
}

func TestInteractivePortLifecycleClearCallsClient(t *testing.T) {
	client := newMockSessionClient(t)
	called := make(chan struct{})
	client.EXPECT().Clear(mock.Anything, session.ID("s1")).RunAndReturn(func(context.Context, session.ID) (session.Info, error) {
		close(called)
		return session.Info{ID: "s2"}, nil
	}).Once()
	r := startLifecycle(t, client)
	r.term.send("/clear\r")
	waitSignal(t, called, "Clear")
}
