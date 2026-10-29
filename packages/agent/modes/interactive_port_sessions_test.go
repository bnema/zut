package modes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type sessionsRun struct {
	i      *Interactive
	term   *keyTerminal
	events chan session.Event
	cancel chan struct{}
}

// startSessionsPort attaches s1 with msgs. The returned cancel channel
// closes when s1's subscription ctx ends (the old session is released).
func startSessionsPort(t *testing.T, client SessionClient, msgs ...provider.Message) *sessionsRun {
	t.Helper()
	port := sessionmocks.NewMockPort(t)
	r := &sessionsRun{events: make(chan session.Event, 4), cancel: make(chan struct{})}
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", CWD: "/w", SessionPath: "/w/s1.jsonl", Run: session.RunStateIdle}, Messages: msgs}, nil).Once()
	port.EXPECT().Subscribe(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, _ uint64) (<-chan session.Event, error) {
		go func() { <-ctx.Done(); close(r.cancel) }()
		return r.events, nil
	}).Once()
	r.term = &keyTerminal{in: make(chan byte, 64)}
	r.i = NewInteractive(InteractiveConfig{Terminal: r.term, Port: port, Client: client, SessionsRoot: t.TempDir(), CWD: "/w"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.i.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		close(r.term.in)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return r
}

// targetPort is a session the TUI switches to.
func targetPort(t *testing.T, id string) *sessionmocks.MockPort {
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: id}, Seq: 5, Messages: []provider.Message{portUserMessage("hello-" + id)}}, nil).Once()
	expectPortSubscribe(port, make(chan session.Event), nil)
	return port
}

func (r *sessionsRun) requireSwitched(t *testing.T, id session.ID, port session.Port) {
	t.Helper()
	require.Eventually(t, func() bool { return r.i.SessionID() == id && r.i.SessionPort() == port }, 5*time.Second, 10*time.Millisecond)
	waitSignal(t, r.cancel, "old session release")
}

func (r *sessionsRun) waitDialogLive(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		r.i.mu.Lock()
		defer r.i.mu.Unlock()
		return len(r.i.sessionDialog.live) == n
	}, 5*time.Second, 10*time.Millisecond)
}

func liveInfos() []session.Info {
	return []session.Info{
		{ID: "s1", Title: "mine", Attach: session.AttachStateAttached},
		{ID: "s2", Title: "other", Attach: session.AttachStateDetached},
	}
}

func TestPortSessionsDialogListsLiveSessions(t *testing.T) {
	client := newMockSessionClient(t)
	client.EXPECT().List(mock.Anything).Return(liveInfos(), nil).Once()
	r := startSessionsPort(t, client)
	r.term.send("/sessions\r")
	r.waitDialogLive(t, 2)
	r.i.mu.Lock()
	out := stripANSIBytes(strings.Join(r.i.sessionDialog.Render(r.i.cfg.Theme, 120), "\n"))
	r.i.mu.Unlock()
	require.Contains(t, out, "live sessions")
	require.Contains(t, out, "this terminal")
	require.Contains(t, out, "detached")
	require.Contains(t, out, "other")
	require.Contains(t, out, "resumable sessions")
}

func TestPortSessionsPickLiveAttachesAndReleasesOld(t *testing.T) {
	client := newMockSessionClient(t)
	client.EXPECT().List(mock.Anything).Return(liveInfos(), nil).Once()
	s2 := targetPort(t, "s2")
	client.EXPECT().Attach(mock.Anything, session.ID("s2")).Return(s2, nil).Once()
	r := startSessionsPort(t, client)
	r.term.send("/sessions\r")
	r.waitDialogLive(t, 2)
	r.term.send("\x1b[B\r") // down to s2, enter
	r.requireSwitched(t, "s2", s2)
}

func TestPortSessionsPickDiskCreatesResume(t *testing.T) {
	client := newMockSessionClient(t)
	s3 := targetPort(t, "s3")
	client.EXPECT().Create(mock.Anything, session.CreateSpec{CWD: "/w", ResumePath: "/w/old.jsonl"}).Return(session.Info{ID: "s3"}, nil).Once()
	client.EXPECT().Attach(mock.Anything, session.ID("s3")).Return(s3, nil).Once()
	r := startSessionsPort(t, client)
	require.Eventually(t, func() bool { return r.i.SessionInfo().ID == "s1" }, 5*time.Second, 10*time.Millisecond)
	require.True(t, r.i.canResumeSessionSelection())
	r.i.loopCalls <- func() { r.i.applySessionSelection("/w/old.jsonl") }
	r.requireSwitched(t, "s3", s3)
}

func TestPortForkCreatesFork(t *testing.T) {
	client := newMockSessionClient(t)
	s4 := targetPort(t, "s4")
	client.EXPECT().Create(mock.Anything, session.CreateSpec{CWD: "/w", Fork: &session.ForkSpec{ParentPath: "/w/s1.jsonl", UpToMessage: 1}}).Return(session.Info{ID: "s4"}, nil).Once()
	client.EXPECT().Attach(mock.Anything, session.ID("s4")).Return(s4, nil).Once()
	r := startSessionsPort(t, client, portUserMessage("first"))
	r.term.send("/fork\r")
	require.Eventually(t, func() bool {
		r.i.mu.Lock()
		defer r.i.mu.Unlock()
		return r.i.jumpDialog.Active() && r.i.pendingFork
	}, 5*time.Second, 10*time.Millisecond)
	r.term.send("\r")
	r.requireSwitched(t, "s4", s4)
}

func TestPortSessionsSwitchErrorKeepsSessionWithNote(t *testing.T) {
	client := newMockSessionClient(t)
	client.EXPECT().List(mock.Anything).Return(liveInfos(), nil).Once()
	client.EXPECT().Attach(mock.Anything, session.ID("s2")).Return(nil, errors.New("boom")).Once()
	r := startSessionsPort(t, client)
	r.term.send("/sessions\r")
	r.waitDialogLive(t, 2)
	r.term.send("\x1b[B\r")
	require.Eventually(t, func() bool {
		r.i.mu.Lock()
		defer r.i.mu.Unlock()
		return strings.Contains(r.i.statusErr, "boom")
	}, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, session.ID("s1"), r.i.SessionID())
	select {
	case <-r.cancel:
		t.Fatal("old session released after a failed switch")
	default:
	}
}
