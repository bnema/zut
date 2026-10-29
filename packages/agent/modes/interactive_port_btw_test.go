package modes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func btwForkSpec(upTo int) session.CreateSpec {
	return session.CreateSpec{CWD: "/w", Fork: &session.ForkSpec{ParentPath: "/w/s1.jsonl", UpToMessage: upTo}, Headless: true}
}

func btwRendered(r *sessionsRun) string {
	return stripANSIBytes(strings.Join(r.i.btwDialog.Render(r.i.cfg.Theme, 120), "\n"))
}

func TestPortBtwRunsOnForkedSession(t *testing.T) {
	client := newMockSessionClient(t)
	fork := sessionmocks.NewMockPort(t)
	forkEvents := make(chan session.Event, 8)
	released := make(chan struct{})
	submitted := make(chan session.Input, 1)
	client.EXPECT().Create(mock.Anything, btwForkSpec(2)).Return(session.Info{ID: "b1"}, nil).Once()
	client.EXPECT().Attach(mock.Anything, session.ID("b1")).Return(fork, nil).Once()
	fork.EXPECT().Subscribe(mock.Anything, mock.Anything).Return((<-chan session.Event)(forkEvents), nil).Once()
	fork.EXPECT().Submit(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in session.Input) error {
		submitted <- in
		return nil
	}).Once()
	client.EXPECT().Close(mock.Anything, session.ID("b1"), btwRemoteCloseReason).RunAndReturn(func(context.Context, session.ID, string) error {
		close(released)
		return nil
	}).Once()

	r := startSessionsPort(t, client, portUserMessage("first"), portUserMessage("second"))
	require.Eventually(t, func() bool { return r.i.SessionInfo().ID == "s1" }, 5*time.Second, 10*time.Millisecond)
	r.term.send("/btw what is up\r")

	select {
	case in := <-submitted:
		require.Equal(t, "what is up", in.Text)
	case <-time.After(5 * time.Second):
		t.Fatal("Submit was not called on the forked session")
	}
	forkEvents <- session.Event{Seq: 1, Kind: session.EvAgent, Agent: core.EvTextDelta{Delta: "side answer"}}
	forkEvents <- session.Event{Seq: 2, Kind: session.EvAgent, Agent: core.EvDone{}}
	require.Eventually(t, func() bool {
		return strings.Contains(btwRendered(r), "side answer") && !r.i.btwDialog.Loading()
	}, 5*time.Second, 10*time.Millisecond)

	r.term.send("\x1b")
	waitSignal(t, released, "Close(forked session)")
	require.False(t, r.i.btwDialog.Active())
	require.Equal(t, session.ID("s1"), r.i.SessionID())
}

func TestPortBtwCreateErrorShowsNote(t *testing.T) {
	client := newMockSessionClient(t)
	client.EXPECT().Create(mock.Anything, btwForkSpec(1)).Return(session.Info{}, errors.New("daemon says no")).Once()

	r := startSessionsPort(t, client, portUserMessage("first"))
	require.Eventually(t, func() bool { return r.i.SessionInfo().ID == "s1" }, 5*time.Second, 10*time.Millisecond)
	r.term.send("/btw\r")
	require.Eventually(t, func() bool {
		return strings.Contains(btwRendered(r), "daemon says no")
	}, 5*time.Second, 10*time.Millisecond)
	require.True(t, r.i.btwDialog.Active())
}
