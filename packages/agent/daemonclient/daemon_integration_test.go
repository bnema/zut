package daemonclient

import (
	"bytes"
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/daemon"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// scriptedSession backs a generated MockHandle with an event log: Subscribe
// replays after afterSeq then streams live; Submit publishes a title event.
type scriptedSession struct {
	h *sessionmocks.MockHandle

	mu     sync.Mutex
	info   session.Info
	log    []session.Event
	subs   map[chan session.Event]bool
	done   chan struct{}
	once   sync.Once
	closed chan string
}

func newScriptedSession(t *testing.T, id string) *scriptedSession {
	s := &scriptedSession{
		h:      sessionmocks.NewMockHandle(t),
		info:   session.Info{ID: id, CWD: "/w", Attach: session.AttachStateDetached, Run: session.RunStateIdle},
		subs:   map[chan session.Event]bool{},
		done:   make(chan struct{}),
		closed: make(chan string, 1),
	}
	s.h.EXPECT().Info().RunAndReturn(func() session.Info { s.mu.Lock(); defer s.mu.Unlock(); return s.info }).Maybe()
	s.h.EXPECT().Done().Return(s.done).Maybe()
	s.h.EXPECT().Idle().Return(true).Maybe()
	s.h.EXPECT().SetAttach(mock.Anything, mock.Anything, mock.Anything).Run(func(_ context.Context, st session.AttachState, _ string) {
		s.mu.Lock()
		s.info.Attach = st
		s.mu.Unlock()
	}).Maybe()
	s.h.EXPECT().Subscribe(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, after uint64) (<-chan session.Event, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if after > uint64(len(s.log)) {
			return nil, session.ErrSeqTooOld
		}
		ch := make(chan session.Event, 256)
		for _, ev := range s.log[after:] {
			ch <- ev
		}
		s.subs[ch] = true
		context.AfterFunc(ctx, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.subs[ch] {
				delete(s.subs, ch)
				close(ch)
			}
		})
		return ch, nil
	}).Maybe()
	s.h.EXPECT().Submit(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in session.Input) error {
		s.publish(in.Text)
		return nil
	}).Maybe()
	s.h.EXPECT().Close(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, reason string) error {
		s.once.Do(func() {
			s.mu.Lock()
			for ch := range s.subs {
				delete(s.subs, ch)
				close(ch)
			}
			s.mu.Unlock()
			close(s.done)
			s.closed <- reason
		})
		return nil
	}).Maybe()
	return s
}

func (s *scriptedSession) publish(title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev := session.Event{Seq: uint64(len(s.log) + 1), Session: session.ID(s.info.ID), Kind: session.EvTitle, Data: session.TitleEvent{Title: title}}
	s.log = append(s.log, ev)
	for ch := range s.subs {
		ch <- ev
	}
}

func titles(evs []session.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.Data.(session.TitleEvent).Title
	}
	return out
}

func TestClientAgainstRealDaemon(t *testing.T) {
	path := shortSocket(t)
	s1 := newScriptedSession(t, "s1")
	f := sessionmocks.NewMockFactory(t)
	f.EXPECT().Create(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, spec session.CreateSpec) (session.Handle, error) {
		require.Equal(t, "/w", spec.CWD)
		return s1.h, nil
	}).Once()

	d := daemon.New(f)
	ready := make(chan struct{})
	runErr := make(chan error, 1)
	go func() {
		runErr <- d.Run(context.Background(), daemon.Options{SocketPath: path, Home: t.TempDir(), Version: "daemon-v", GCInterval: time.Hour, Ready: ready})
	}()
	t.Cleanup(func() {
		d.Stop()
		select {
		case <-runErr:
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	select {
	case <-ready:
	case err := <-runErr:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon not ready")
	}
	ctx := testCtx(t)

	var warn bytes.Buffer
	c := dial(t, path, Options{ClientVersion: "client-v", ClientKind: "tui", Warnings: &warn})
	require.Equal(t, "daemon is running zut daemon-v; run \"zut daemon restart\"\n", warn.String())

	info, err := c.Create(ctx, session.CreateSpec{CWD: "/w", Env: []string{}, Args: []string{}})
	require.NoError(t, err)
	require.Equal(t, "s1", info.ID)

	port := c.Port("s1", true)
	subCtx, cancelSub := context.WithCancel(ctx)
	ch, err := port.Subscribe(subCtx, 0)
	require.NoError(t, err)
	require.Equal(t, session.AttachStateAttached, port.Info().Attach)
	require.NoError(t, port.Submit(ctx, session.Input{Text: "1"}))
	require.NoError(t, port.Submit(ctx, session.Input{Text: "2"}))
	got := recvSeqs(t, ch, 2)
	require.Equal(t, []string{"1", "2"}, titles(got))

	require.NoError(t, c.Detach(ctx, "s1"))
	// The daemon releases control (SetAttach) before answering detach.
	require.Equal(t, session.AttachStateDetached, s1.h.Info().Attach)
	cancelSub()
	for range ch {
	}
	// Work continues while detached.
	s1.publish("3")
	s1.publish("4")

	ch, err = port.Subscribe(ctx, got[len(got)-1].Seq)
	require.NoError(t, err)
	got = append(got, recvSeqs(t, ch, 2)...)
	require.NoError(t, port.Submit(ctx, session.Input{Text: "5"}))
	got = append(got, recvSeqs(t, ch, 1)...)
	for i, ev := range got {
		require.Equal(t, uint64(i+1), ev.Seq)
	}
	require.Equal(t, []string{"1", "2", "3", "4", "5"}, titles(got))

	// Close requires control.
	other := dial(t, path, Options{ClientVersion: "daemon-v"})
	require.ErrorContains(t, other.Close(ctx, "s1", "user_exit"), daemon.ErrNotController.Error())
	require.NoError(t, c.Close(ctx, "s1", "user_exit"))
	select {
	case reason := <-s1.closed:
		require.Equal(t, "user_exit", reason)
	case <-time.After(5 * time.Second):
		t.Fatal("session not closed")
	}
	list, err := c.List(ctx)
	require.NoError(t, err)
	require.Empty(t, list)

	st, err := c.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, os.Getpid(), st.PID)
	require.Equal(t, "daemon-v", st.Version)
	require.NoError(t, c.Stop(ctx, time.Second))
	select {
	case err := <-runErr:
		require.NoError(t, err)
		runErr <- nil
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not stop after daemon_stop")
	}
}
