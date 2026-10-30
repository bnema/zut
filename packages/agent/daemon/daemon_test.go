package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
)

// testSession bundles a generated MockHandle with the state its
// RunAndReturn closures read.
type testSession struct {
	h      *sessionmocks.MockHandle
	mu     sync.Mutex
	info   session.Info
	idle   atomic.Bool
	subs   []chan session.Event
	done   chan struct{}
	once   sync.Once
	closes chan string
	attach chan session.AttachState
	inputs chan session.Input
}

func newTestSession(t *testing.T, id string) *testSession {
	s := &testSession{
		h:      sessionmocks.NewMockHandle(t),
		info:   session.Info{ID: id, CWD: "/w/" + id, Attach: session.AttachStateDetached, Run: session.RunStateIdle, SessionPath: filepath.Join(t.TempDir(), id+".jsonl")},
		done:   make(chan struct{}),
		closes: make(chan string, 4),
		attach: make(chan session.AttachState, 16),
		inputs: make(chan session.Input, 4),
	}
	s.idle.Store(true)
	s.h.EXPECT().Info().RunAndReturn(func() session.Info { s.mu.Lock(); defer s.mu.Unlock(); return s.info }).Maybe()
	s.h.EXPECT().Done().Return(s.done).Maybe()
	s.h.EXPECT().Idle().RunAndReturn(s.idle.Load).Maybe()
	s.h.EXPECT().Subscribe(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, uint64) (<-chan session.Event, error) {
		ch := make(chan session.Event, 64)
		s.mu.Lock()
		s.subs = append(s.subs, ch)
		s.mu.Unlock()
		return ch, nil
	}).Maybe()
	s.h.EXPECT().SetAttach(mock.Anything, mock.Anything, mock.Anything).Run(func(_ context.Context, st session.AttachState, _ string) {
		s.mu.Lock()
		s.info.Attach = st
		s.mu.Unlock()
		s.attach <- st
	}).Maybe()
	s.h.EXPECT().Close(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, reason string) error {
		s.closes <- reason
		s.once.Do(func() { close(s.done) })
		return nil
	}).Maybe()
	s.h.EXPECT().Submit(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in session.Input) error {
		s.inputs <- in
		return nil
	}).Maybe()
	return s
}

// emit delivers ev to every subscription (daemon watcher and clients).
func (s *testSession) emit(ev session.Event) {
	s.mu.Lock()
	subs := append([]chan session.Event(nil), s.subs...)
	s.mu.Unlock()
	for _, ch := range subs {
		ch <- ev
	}
}

func (s *testSession) set(fn func(*session.Info)) { s.mu.Lock(); fn(&s.info); s.mu.Unlock() }

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for value")
	}
	var zero T
	return zero
}

type running struct {
	d      *Daemon
	home   string
	sock   string
	cancel context.CancelFunc
	errc   chan error
	once   sync.Once
	err    error
}

func shortSocket(t *testing.T) string {
	dir, err := os.MkdirTemp("", "zd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s", "daemon.sock")
}

func startDaemon(t *testing.T, f session.Factory, home string, opts Options) *running {
	t.Helper()
	return startDaemonWith(t, f, home, opts, nil)
}

// startDaemonWith runs configure on the daemon before Run.
func startDaemonWith(t *testing.T, f session.Factory, home string, opts Options, configure func(*Daemon)) *running {
	t.Helper()
	opts.Home = home
	if opts.SocketPath == "" {
		opts.SocketPath = shortSocket(t)
	}
	opts.Version = "test"
	if opts.GCInterval == 0 {
		opts.GCInterval = time.Hour
	}
	ready := make(chan struct{})
	opts.Ready = ready
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{d: New(f), home: home, sock: opts.SocketPath, cancel: cancel, errc: make(chan error, 1)}
	if configure != nil {
		configure(r.d)
	}
	go func() { r.errc <- r.d.Run(ctx, opts) }()
	select {
	case <-ready:
	case err := <-r.errc:
		cancel()
		t.Fatalf("Run: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon not ready")
	}
	t.Cleanup(func() { r.stop(t) })
	return r
}

func (r *running) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	r.wait(t)
}

func (r *running) wait(t *testing.T) {
	t.Helper()
	r.once.Do(func() {
		select {
		case r.err = <-r.errc:
		case <-time.After(10 * time.Second):
			r.err = errors.New("daemon did not stop")
		}
	})
	if r.err != nil {
		t.Fatalf("Run: %v", r.err)
	}
}

type testClient struct {
	conn  *daemonproto.Conn
	notes chan daemonproto.Notification
}

func dial(t *testing.T, sock string) *testClient {
	t.Helper()
	nc, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	c := &testClient{notes: make(chan daemonproto.Notification, 64)}
	c.conn = daemonproto.NewConn(nc, daemonproto.Options{OnNotification: func(n daemonproto.Notification) {
		select {
		case c.notes <- n:
		default:
		}
	}})
	done := make(chan struct{})
	go func() { _ = c.conn.Serve(context.Background()); close(done) }()
	t.Cleanup(func() { _ = c.conn.Close(); <-done })
	return c
}

func (c *testClient) call(t *testing.T, frame, out any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.conn.Call(ctx, frame, out)
}

func (c *testClient) note(t *testing.T, typ string) daemonproto.Notification {
	t.Helper()
	for {
		n := recv(t, c.notes)
		if n.Type == typ {
			return n
		}
	}
}

func assertNoState(t *testing.T, home string) {
	t.Helper()
	if _, err := os.Stat(statePath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state.json should not exist: %v", err)
	}
}

func TestDaemonCreateAttachSingleControllerDetachClose(t *testing.T) {
	home := t.TempDir()
	s1 := newTestSession(t, "s1")
	f := sessionmocks.NewMockFactory(t)
	var gotSpec session.CreateSpec
	f.EXPECT().Create(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, spec session.CreateSpec) (session.Handle, error) {
		gotSpec = spec
		return s1.h, nil
	}).Once()
	r := startDaemon(t, f, home, Options{})
	a, b := dial(t, r.sock), dial(t, r.sock)

	var hello daemonproto.HelloData
	if err := a.call(t, daemonproto.HelloRequest{Type: "hello", ProtocolVersion: daemonproto.ProtocolVersion}, &hello); err != nil || hello.DaemonVersion != "test" {
		t.Fatalf("hello = %+v, %v", hello, err)
	}
	if err := a.call(t, daemonproto.HelloRequest{Type: "hello", ProtocolVersion: 99}, nil); err == nil {
		t.Fatal("protocol mismatch accepted")
	}
	var info session.Info
	spec := session.CreateSpec{CWD: "/w", Env: []string{"A=1"}, Args: []string{"--model", "m"}}
	if err := a.call(t, daemonproto.CreateRequest{Type: "create", Spec: spec}, &info); err != nil || info.ID != "s1" {
		t.Fatalf("create = %+v, %v", info, err)
	}
	if gotSpec.CWD != "/w" || gotSpec.Env[0] != "A=1" {
		t.Fatalf("factory spec = %+v", gotSpec)
	}
	assertNoState(t, home)
	if err := a.call(t, daemonproto.AttachRequest{Type: "attach", Session: "s1", Controlling: true}, &info); err != nil {
		t.Fatal(err)
	}
	if st := recv(t, s1.attach); st != session.AttachStateAttached {
		t.Fatalf("attach state = %s", st)
	}
	s1.emit(session.Event{Seq: 1, Kind: session.EvTitle, Data: session.TitleEvent{Title: "hi"}})
	if n := a.note(t, "event"); n.Session != "s1" {
		t.Fatalf("event = %+v", n)
	}
	// Second controller kicks the first (D5).
	if err := b.call(t, daemonproto.AttachRequest{Type: "attach", Session: "s1", Controlling: true}, &info); err != nil {
		t.Fatal(err)
	}
	if st := recv(t, s1.attach); st != session.AttachStateAttached {
		t.Fatalf("takeover attach state = %s", st)
	}
	if n := a.note(t, "detached_by_other"); n.Session != "s1" {
		t.Fatalf("detached_by_other = %+v", n)
	}
	err := a.call(t, daemonproto.PortRequest{Type: "port", Session: "s1", Method: "submit", Params: json.RawMessage(`{"in":{"text":"x"}}`)}, nil)
	if err == nil || !strings.Contains(err.Error(), ErrNotController.Error()) {
		t.Fatalf("submit by kicked client = %v", err)
	}
	var got session.Info
	if err := a.call(t, daemonproto.PortRequest{Type: "port", Session: "s1", Method: "info"}, &got); err != nil || got.ID != "s1" {
		t.Fatalf("read by non-controller = %+v, %v", got, err)
	}
	if err := b.call(t, daemonproto.PortRequest{Type: "port", Session: "s1", Method: "submit", Params: json.RawMessage(`{"in":{"text":"x","mode":"auto","origin":"user"}}`)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.call(t, daemonproto.PortRequest{Type: "port", Session: "s1", Method: "submit", Params: json.RawMessage(`{"in":{"text":"y","origin":"inter_session","from":{"session_id":"evil"}}}`)}, nil); err != nil {
		t.Fatal(err)
	}
	if in := recv(t, s1.inputs); in.Text != "x" || in.Origin != session.InputOriginUser {
		t.Fatalf("submit input = %+v", in)
	}
	if in := recv(t, s1.inputs); in.Origin != session.InputOriginUser || in.From != nil {
		t.Fatalf("client-forged origin not overridden: %+v", in)
	}
	s1.h.EXPECT().ClearQueuedUser(mock.Anything).Return(nil).Once()
	if err := b.call(t, daemonproto.PortRequest{Type: "port", Session: "s1", Method: "clear_queued_user"}, nil); err != nil {
		t.Fatalf("clear_queued_user = %v", err)
	}
	if err := a.call(t, daemonproto.PortRequest{Type: "port", Session: "s1", Method: "clear_queued_user"}, nil); err == nil || !strings.Contains(err.Error(), ErrNotController.Error()) {
		t.Fatalf("clear_queued_user by non-controller = %v", err)
	}
	if err := a.call(t, daemonproto.CloseRequest{Type: "close", Session: "s1"}, nil); err == nil || !strings.Contains(err.Error(), ErrNotController.Error()) {
		t.Fatalf("close by non-controller = %v", err)
	}
	if err := b.call(t, daemonproto.DetachRequest{Type: "detach", Session: "s1"}, nil); err != nil {
		t.Fatal(err)
	}
	if st := recv(t, s1.attach); st != session.AttachStateDetached {
		t.Fatalf("detach state = %s", st)
	}
	var list []session.Info
	if err := a.call(t, daemonproto.ListRequest{Type: "list"}, &list); err != nil || len(list) != 1 || list[0].Attach != session.AttachStateDetached {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := a.call(t, daemonproto.CloseRequest{Type: "close", Session: "s1", Reason: "user_exit"}, nil); err != nil {
		t.Fatal(err)
	}
	if reason := recv(t, s1.closes); reason != "user_exit" {
		t.Fatalf("close reason = %q", reason)
	}
	assertNoState(t, home)
	if err := a.call(t, daemonproto.AttachRequest{Type: "attach", Session: "s1"}, nil); err == nil {
		t.Fatal("attach to closed session succeeded")
	}
}

func TestDaemonClientDisconnectDetaches(t *testing.T) {
	s1 := newTestSession(t, "s1")
	f := sessionmocks.NewMockFactory(t)
	f.EXPECT().Create(mock.Anything, mock.Anything).Return(s1.h, nil).Once()
	r := startDaemon(t, f, t.TempDir(), Options{})
	a := dial(t, r.sock)
	if err := a.call(t, daemonproto.CreateRequest{Type: "create"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.call(t, daemonproto.AttachRequest{Type: "attach", Session: "s1", Controlling: true}, nil); err != nil {
		t.Fatal(err)
	}
	recv(t, s1.attach)
	_ = a.conn.Close()
	if st := recv(t, s1.attach); st != session.AttachStateDetached {
		t.Fatalf("state after disconnect = %s", st)
	}
}

func TestDaemonClearRecreatesAndMovesController(t *testing.T) {
	home := t.TempDir()
	s1, s2 := newTestSession(t, "s1"), newTestSession(t, "s2")
	s1.set(func(i *session.Info) { i.Provider, i.Model, i.Reasoning = "p2", "m2", "high" })
	f := sessionmocks.NewMockFactory(t)
	f.EXPECT().Create(mock.Anything, mock.Anything).Return(s1.h, nil).Once()
	specs := make(chan session.CreateSpec, 1)
	f.EXPECT().Create(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, spec session.CreateSpec) (session.Handle, error) {
		specs <- spec
		return s2.h, nil
	}).Once()
	r := startDaemon(t, f, home, Options{})
	a := dial(t, r.sock)
	launch := session.CreateSpec{CWD: "/w", Env: []string{"E=1"}, Args: []string{"--session", "/x.jsonl", "--model", "m1", "--no-lsp"}}
	if err := a.call(t, daemonproto.CreateRequest{Type: "create", Spec: launch}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.call(t, daemonproto.AttachRequest{Type: "attach", Session: "s1", Controlling: true}, nil); err != nil {
		t.Fatal(err)
	}
	recv(t, s1.attach)
	var next session.Info
	if err := a.call(t, daemonproto.ClearRequest{Type: "clear", Session: "s1"}, &next); err != nil || next.ID != "s2" {
		t.Fatalf("clear = %+v, %v", next, err)
	}
	spec := recv(t, specs)
	want := "--no-lsp --provider p2 --model m2 --reasoning high"
	if got := strings.Join(spec.Args, " "); got != want || spec.CWD != "/w/s1" || spec.Env[0] != "E=1" || spec.ResumePath != "" {
		t.Fatalf("successor spec = %+v (args %q)", spec, got)
	}
	if reason := recv(t, s1.closes); reason != ReasonCleared {
		t.Fatalf("old close reason = %q", reason)
	}
	n := a.note(t, "closed")
	var closed daemonproto.ClosedNotification
	if err := json.Unmarshal(n.Raw, &closed); err != nil || closed.Successor != "s2" {
		t.Fatalf("closed = %s, %v", n.Raw, err)
	}
	if st := recv(t, s2.attach); st != session.AttachStateAttached {
		t.Fatalf("successor attach = %s", st)
	}
	assertNoState(t, home)
}

func TestClearArgsKeepsTerminator(t *testing.T) {
	got := clearArgs([]string{"-c", "-r", "abc", "--model", "x", "--", "--provider"}, session.Info{Model: "m"})
	if strings.Join(got, " ") != "--model m -- --provider" {
		t.Fatalf("clearArgs = %q", got)
	}
}

func TestDaemonStartsEmptyAndRemovesPreviousState(t *testing.T) {
	home := t.TempDir()
	transcript := filepath.Join(home, "sessions", "bucket", "x.jsonl")
	const transcriptData = "transcript sentinel\n"
	if err := os.MkdirAll(filepath.Dir(transcript), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(transcriptData), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(stateDir(home), "sessions", "old")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{
		statePath(home):                                 `{"version":1,"sessions":[{"id":"old","session_path":"missing.jsonl","env":["SYNTHETIC_KEY=test"],"running":true}]}`,
		filepath.Join(dir, "deferred.jsonl"):            `{}`,
		filepath.Join(stateDir(home), "schedules.json"): `{"keep":true}`,
	} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f := sessionmocks.NewMockFactory(t)
	r := startDaemon(t, f, home, Options{})
	if got := r.d.List(context.Background()); len(got) != 0 {
		t.Fatalf("registry = %+v", got)
	}
	assertNoState(t, home)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale directory remains: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(stateDir(home), "schedules.json")); err != nil || string(data) != `{"keep":true}` {
		t.Fatalf("schedules changed: %q, %v", data, err)
	}
	if data, err := os.ReadFile(transcript); err != nil || string(data) != transcriptData {
		t.Fatalf("transcript changed: %q, %v", data, err)
	}
	r.stop(t)
	assertNoState(t, home)
}

func TestDaemonCreateRefusesBusySessionFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	held, err := lockSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	f := sessionmocks.NewMockFactory(t)
	r := startDaemon(t, f, t.TempDir(), Options{})
	a := dial(t, r.sock)
	err = a.call(t, daemonproto.CreateRequest{Type: "create", Spec: session.CreateSpec{ResumePath: path}}, nil)
	if err == nil || !strings.Contains(err.Error(), "session file is open in another daemon") {
		t.Fatalf("create on busy file = %v", err)
	}
}

func TestDaemonLockPreventsSecondDaemonAndKeepsSocket(t *testing.T) {
	home := t.TempDir()
	r := startDaemon(t, sessionmocks.NewMockFactory(t), home, Options{})
	// Ready has completed cleanup and the first daemon still holds the lock.
	journal := filepath.Join(stateDir(home), "sessions", "live", "deferred.jsonl")
	const journalData = "live journal sentinel\n"
	if err := os.MkdirAll(filepath.Dir(journal), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte(journalData), 0600); err != nil {
		t.Fatal(err)
	}
	err := New(sessionmocks.NewMockFactory(t)).Run(context.Background(), Options{Home: home, SocketPath: r.sock})
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Run = %v", err)
	}
	if data, err := os.ReadFile(journal); err != nil || string(data) != journalData {
		t.Fatalf("live journal changed: %q, %v", data, err)
	}
	c := dial(t, r.sock)
	if err := c.call(t, daemonproto.DaemonStatusRequest{Type: "daemon_status"}, nil); err != nil {
		t.Fatalf("first daemon unreachable after second Run: %v", err)
	}
}

func TestDaemonRemovesStaleSocketAndSetsModes(t *testing.T) {
	sock := shortSocket(t)
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	r := startDaemon(t, sessionmocks.NewMockFactory(t), home, Options{SocketPath: sock})
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{filepath.Dir(sock): 0o700, sock: 0o600, pidPath(home): 0o600, stateDir(home): 0o700} {
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != want {
				t.Fatalf("%s mode = %v, want %v", path, fi.Mode().Perm(), want)
			}
		}
	}
	var status daemonproto.DaemonStatusData
	if err := dial(t, r.sock).call(t, daemonproto.DaemonStatusRequest{Type: "daemon_status"}, &status); err != nil || status.PID != os.Getpid() {
		t.Fatalf("status = %+v, %v", status, err)
	}
}

func TestDaemonStopRequest(t *testing.T) {
	r := startDaemon(t, sessionmocks.NewMockFactory(t), t.TempDir(), Options{})
	_ = dial(t, r.sock).call(t, daemonproto.DaemonStopRequest{Type: "daemon_stop", GracefulMS: 500}, nil)
	r.wait(t)
}

func TestDaemonForgetsSelfClosedSession(t *testing.T) {
	home := t.TempDir()
	s1 := newTestSession(t, "s1")
	f := sessionmocks.NewMockFactory(t)
	f.EXPECT().Create(mock.Anything, mock.Anything).Return(s1.h, nil).Once()
	r := startDaemon(t, f, home, Options{})
	a := dial(t, r.sock)
	if err := a.call(t, daemonproto.CreateRequest{Type: "create"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.call(t, daemonproto.AttachRequest{Type: "attach", Session: "s1", Controlling: true}, nil); err != nil {
		t.Fatal(err)
	}
	s1.once.Do(func() { close(s1.done) })
	a.note(t, "closed")
	assertNoState(t, home)
}

func TestGCEligibilityMatrix(t *testing.T) {
	now := time.Now()
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	cases := []struct {
		name     string
		attached bool
		idle     bool
		age      time.Duration
		input    time.Duration
		want     bool
	}{
		{"eligible", false, true, 25 * time.Hour, -1, true},
		{"attached", true, true, 25 * time.Hour, -1, false},
		{"busy_or_scheduled_or_goal_or_resident", false, false, 25 * time.Hour, -1, false},
		{"too_recent", false, true, time.Hour, -1, false},
		{"recent_user_input", false, true, 25 * time.Hour, time.Hour, false},
	}
	f := sessionmocks.NewMockFactory(t)
	sessions := map[string]*testSession{}
	for _, tc := range cases {
		s := newTestSession(t, tc.name)
		s.idle.Store(tc.idle)
		if tc.input >= 0 {
			s.set(func(i *session.Info) { i.LastUserInputAt = now.Add(tc.input) })
		}
		sessions[tc.name] = s
		f.EXPECT().Create(mock.Anything, mock.MatchedBy(func(sp session.CreateSpec) bool { return sp.CWD == tc.name })).Return(s.h, nil).Once()
	}
	clockFn := func() time.Time { return time.Unix(0, clock.Load()) }
	r := startDaemon(t, f, t.TempDir(), Options{IdleGC: 24 * time.Hour, Clock: clockFn})
	a := dial(t, r.sock)
	for _, tc := range cases {
		if err := a.call(t, daemonproto.CreateRequest{Type: "create", Spec: session.CreateSpec{CWD: tc.name}}, nil); err != nil {
			t.Fatal(err)
		}
		if tc.attached {
			if err := a.call(t, daemonproto.AttachRequest{Type: "attach", Session: session.ID(tc.name), Controlling: true}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Created "now"; ages are relative to registration.
	for _, tc := range cases {
		e := r.d.lookup(session.ID(tc.name))
		r.d.mu.Lock()
		e.detachedAt = now.Add(-tc.age)
		r.d.mu.Unlock()
	}
	closed := map[session.ID]bool{}
	for _, id := range r.d.collect() {
		closed[id] = true
	}
	for _, tc := range cases {
		if closed[session.ID(tc.name)] != tc.want {
			t.Errorf("%s: closed=%v want %v", tc.name, closed[session.ID(tc.name)], tc.want)
		}
	}
	if reason := recv(t, sessions["eligible"].closes); reason != ReasonIdleGC {
		t.Fatalf("gc reason = %q", reason)
	}
}

func TestGCDisabled(t *testing.T) {
	s := newTestSession(t, "s1")
	f := sessionmocks.NewMockFactory(t)
	f.EXPECT().Create(mock.Anything, mock.Anything).Return(s.h, nil).Once()
	r := startDaemon(t, f, t.TempDir(), Options{IdleGC: 0})
	if err := dial(t, r.sock).call(t, daemonproto.CreateRequest{Type: "create"}, nil); err != nil {
		t.Fatal(err)
	}
	r.d.mu.Lock()
	r.d.reg["s1"].detachedAt = time.Time{}
	r.d.mu.Unlock()
	if got := r.d.collect(); len(got) != 0 {
		t.Fatalf("collect with GC disabled = %v", got)
	}
}

func TestDirectorySendGuardsAndDelivery(t *testing.T) {
	sender, target := newTestSession(t, "a"), newTestSession(t, "b")
	sender.set(func(i *session.Info) { i.Title, i.CWD, i.RepoRoot = "T", "/repo/x", "/repo" })
	target.set(func(i *session.Info) { i.RepoRoot = "" })
	f := sessionmocks.NewMockFactory(t)
	f.EXPECT().Create(mock.Anything, mock.MatchedBy(func(s session.CreateSpec) bool { return s.CWD == "a" })).Return(sender.h, nil).Once()
	f.EXPECT().Create(mock.Anything, mock.MatchedBy(func(s session.CreateSpec) bool { return s.CWD == "b" })).Return(target.h, nil).Once()
	r := startDaemon(t, f, t.TempDir(), Options{})
	c := dial(t, r.sock)
	for _, id := range []string{"a", "b"} {
		if err := c.call(t, daemonproto.CreateRequest{Type: "create", Spec: session.CreateSpec{CWD: id}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if err := r.d.Send(ctx, "a", "b", "hi"); !errors.Is(err, session.ErrNotAttached) {
		t.Fatalf("detached sender = %v", err)
	}
	sender.set(func(i *session.Info) {
		i.Attach = session.AttachStateAttached
		i.CurrentOrigin = session.InputOriginInterSession
	})
	if err := r.d.Send(ctx, "a", "b", "hi"); !errors.Is(err, session.ErrLoopGuard) {
		t.Fatalf("loop guard = %v", err)
	}
	sender.set(func(i *session.Info) { i.CurrentOrigin = session.InputOriginUser })
	if err := r.d.Send(ctx, "a", "zzz", "hi"); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("unknown target = %v", err)
	}
	if err := r.d.Send(ctx, "a", "b", "hi"); err != nil {
		t.Fatalf("detached target send = %v", err)
	}
	in := recv(t, target.inputs)
	if in.Origin != session.InputOriginInterSession || in.Mode != session.SubmitModeAuto || in.From == nil || in.From.SessionID != "a" ||
		in.Text != "[message from session a (T, /repo/x)]\nhi" {
		t.Fatalf("delivered = %+v", in)
	}
	tree := r.d.Tree(ctx)
	if len(tree.Workspaces) != 2 || tree.Workspaces[0].RepoRoot != "/repo" || tree.Workspaces[1].RepoRoot != "/w/b" {
		t.Fatalf("tree = %+v", tree)
	}
}

func TestWriteFileAtomicReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d", "f.json")
	for _, v := range []string{"one", "two"} {
		if err := writeFileAtomic(path, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "two" {
		t.Fatalf("content = %q, %v", b, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover temp files: %v", entries)
	}
}

func TestOpenLog(t *testing.T) {
	home := t.TempDir()
	l, c, err := OpenLog(home)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hello")
	_ = c.Close()
	b, err := os.ReadFile(LogPath(home))
	if err != nil || !strings.Contains(string(b), "hello") {
		t.Fatalf("log = %q, %v", b, err)
	}
}

func TestDefaultSocketPathOverride(t *testing.T) {
	t.Setenv("ZUT_DAEMON_SOCKET", "/tmp/x.sock")
	if got := DefaultSocketPath("/h"); got != "/tmp/x.sock" {
		t.Fatalf("override = %q", got)
	}
	t.Setenv("ZUT_DAEMON_SOCKET", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("LOCALAPPDATA", "")
	if got := DefaultSocketPath("/h"); got != filepath.Join("/h", "run", "daemon.sock") {
		t.Fatalf("fallback = %q", got)
	}
}

func TestDaemonShutdownLeaksNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	s1 := newTestSession(t, "s1")
	f := sessionmocks.NewMockFactory(t)
	f.EXPECT().Create(mock.Anything, mock.Anything).Return(s1.h, nil).Once()
	r := startDaemon(t, f, t.TempDir(), Options{IdleGC: time.Hour})
	nc, err := net.Dial("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	conn := daemonproto.NewConn(nc, daemonproto.Options{})
	served := make(chan struct{})
	go func() { _ = conn.Serve(context.Background()); close(served) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Call(ctx, daemonproto.CreateRequest{Type: "create"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := conn.Call(ctx, daemonproto.AttachRequest{Type: "attach", Session: "s1", Controlling: true}, nil); err != nil {
		t.Fatal(err)
	}
	r.stop(t)
	<-served // the daemon closed the connection
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines %d > %d after shutdown:\n%s", runtime.NumGoroutine(), before, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDaemonSocketLockAcrossHomes(t *testing.T) {
	sock := shortSocket(t)
	r := startDaemon(t, sessionmocks.NewMockFactory(t), t.TempDir(), Options{SocketPath: sock})
	err := New(sessionmocks.NewMockFactory(t)).Run(context.Background(), Options{Home: t.TempDir(), SocketPath: sock})
	if !errors.Is(err, ErrAlreadyRunning) || !strings.Contains(err.Error(), sock) {
		t.Fatalf("second daemon on same socket = %v", err)
	}
	if err := dial(t, r.sock).call(t, daemonproto.DaemonStatusRequest{Type: "daemon_status"}, nil); err != nil {
		t.Fatalf("first daemon unreachable: %v", err)
	}
}

func TestSocketCloseKeepsForeignSocket(t *testing.T) {
	sock := shortSocket(t)
	ln, err := listenSocket(sock, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Someone replaced the path; close must not remove the replacement.
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sock, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln.close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("foreign socket removed: %v", err)
	}
	ln2, err := listenSocket(sock, nil)
	if err != nil {
		t.Fatalf("relisten after close: %v", err)
	}
	ln2.close()
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("own socket not removed: %v", err)
	}
}

func TestPrepareSocketDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode checks are Unix-only")
	}
	base := t.TempDir()
	created := filepath.Join(base, "new", "run")
	if err := prepareSocketDir(created, nil); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(created); fi.Mode().Perm() != 0o700 {
		t.Fatalf("created mode = %v", fi.Mode().Perm())
	}
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prepareSocketDir(open, nil); err == nil {
		t.Fatal("group/other-accessible foreign dir accepted")
	}
	if fi, _ := os.Stat(open); fi.Mode().Perm() != 0o755 {
		t.Fatalf("foreign dir chmodded to %v", fi.Mode().Perm())
	}
	if err := prepareSocketDir(open, []string{open}); err != nil {
		t.Fatalf("zut-owned dir: %v", err)
	}
	if fi, _ := os.Stat(open); fi.Mode().Perm() != 0o700 {
		t.Fatalf("zut-owned dir mode = %v", fi.Mode().Perm())
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(created, link); err != nil {
		t.Fatal(err)
	}
	if err := prepareSocketDir(link, nil); err == nil {
		t.Fatal("symlinked socket dir accepted")
	}
}

func TestDirectoryStuckTargetDoesNotBlockOthers(t *testing.T) {
	sender, stuck, ok := newTestSession(t, "a"), sessionmocks.NewMockHandle(t), newTestSession(t, "c")
	stuckDone := make(chan struct{})
	release := make(chan struct{})
	stuckIn := make(chan struct{}, 4)
	stuck.EXPECT().Info().Return(session.Info{ID: "b", SessionPath: filepath.Join(t.TempDir(), "b.jsonl")}).Maybe()
	stuck.EXPECT().Done().Return(stuckDone).Maybe()
	stuck.EXPECT().Subscribe(mock.Anything, mock.Anything).Return(make(chan session.Event), nil).Maybe()
	stuck.EXPECT().Close(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, string) error { close(stuckDone); return nil }).Maybe()
	stuck.EXPECT().Submit(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, _ session.Input) error {
		stuckIn <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return ctx.Err()
	}).Maybe()
	t.Cleanup(func() { close(release) })
	f := sessionmocks.NewMockFactory(t)
	f.EXPECT().Create(mock.Anything, mock.MatchedBy(func(s session.CreateSpec) bool { return s.CWD == "a" })).Return(sender.h, nil).Once()
	f.EXPECT().Create(mock.Anything, mock.MatchedBy(func(s session.CreateSpec) bool { return s.CWD == "b" })).Return(stuck, nil).Once()
	f.EXPECT().Create(mock.Anything, mock.MatchedBy(func(s session.CreateSpec) bool { return s.CWD == "c" })).Return(ok.h, nil).Once()
	r := startDaemon(t, f, t.TempDir(), Options{})
	c := dial(t, r.sock)
	for _, id := range []string{"a", "b", "c"} {
		if err := c.call(t, daemonproto.CreateRequest{Type: "create", Spec: session.CreateSpec{CWD: id}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	sender.set(func(i *session.Info) { i.Attach = session.AttachStateAttached })
	ctx := context.Background()
	if err := r.d.Send(ctx, "a", "b", "1"); err != nil {
		t.Fatal(err)
	}
	recv(t, stuckIn) // the stuck target's worker is blocked in Submit
	// Fill the stuck target's queue; overflow reports ErrBusy.
	var err error
	for i := 0; i <= deliveryQueueSize && err == nil; i++ {
		err = r.d.Send(ctx, "a", "b", "x")
	}
	if !errors.Is(err, session.ErrBusy) {
		t.Fatalf("overflow = %v", err)
	}
	if err := r.d.Send(ctx, "a", "c", "hello"); err != nil {
		t.Fatal(err)
	}
	if in := recv(t, ok.inputs); !strings.HasSuffix(in.Text, "hello") {
		t.Fatalf("other target got %+v", in)
	}
}

func TestControlNotificationSurvivesEventBurst(t *testing.T) {
	d := New(sessionmocks.NewMockFactory(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.ctx = ctx
	srv, peer := net.Pipe()
	c := &client{id: 1, d: d, outbox: make(chan any, clientOutboxSize), control: make(chan any, controlQueueSize), subs: map[session.ID]context.CancelFunc{}}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.conn = daemonproto.NewConn(srv, daemonproto.Options{})
	defer c.close()
	// The peer is not reading yet: fill the event outbox completely.
	for i := 0; i < clientOutboxSize; i++ {
		c.outbox <- daemonproto.EventNotification{Type: "event", Session: "s1", Seq: uint64(i + 1), Kind: session.EvTitle}
	}
	c.enqueue(daemonproto.ClosedNotification{Type: "closed", Session: "s1"})
	if c.ctx.Err() != nil {
		t.Fatal("client disconnected by event burst")
	}
	notes := make(chan daemonproto.Notification, clientOutboxSize+2)
	pc := daemonproto.NewConn(peer, daemonproto.Options{OnNotification: func(n daemonproto.Notification) { notes <- n }})
	served := make(chan struct{})
	go func() { _ = pc.Serve(ctx); close(served) }()
	defer func() { _ = pc.Close(); <-served }()
	wrote := make(chan struct{})
	go func() { c.writeLoop(); close(wrote) }()
	defer func() { c.close(); <-wrote }()
	if n := recv(t, notes); n.Type != "closed" {
		t.Fatalf("first notification = %s, want closed (control preferred)", n.Type)
	}
	for i := 0; i < clientOutboxSize; i++ {
		if n := recv(t, notes); n.Type != "event" {
			t.Fatalf("notification %d = %s", i, n.Type)
		}
	}
	// Only control-queue overflow disconnects.
	p1, p2 := net.Pipe()
	defer p2.Close()
	c2 := &client{id: 2, d: d, control: make(chan any, controlQueueSize), conn: daemonproto.NewConn(p1, daemonproto.Options{}), outbox: make(chan any, 1)}
	c2.ctx, c2.cancel = context.WithCancel(ctx)
	for i := 0; i <= controlQueueSize; i++ {
		c2.enqueue(daemonproto.DetachedByOtherNotification{Type: "detached_by_other", Session: "s1"})
	}
	if c2.ctx.Err() == nil {
		t.Fatal("control overflow did not disconnect")
	}
}

func TestDirectorySendToClosedTarget(t *testing.T) {
	sender, target := newTestSession(t, "a"), newTestSession(t, "b")
	f := sessionmocks.NewMockFactory(t)
	f.EXPECT().Create(mock.Anything, mock.MatchedBy(func(s session.CreateSpec) bool { return s.CWD == "a" })).Return(sender.h, nil).Once()
	f.EXPECT().Create(mock.Anything, mock.MatchedBy(func(s session.CreateSpec) bool { return s.CWD == "b" })).Return(target.h, nil).Once()
	r := startDaemon(t, f, t.TempDir(), Options{})
	c := dial(t, r.sock)
	for _, id := range []string{"a", "b"} {
		if err := c.call(t, daemonproto.CreateRequest{Type: "create", Spec: session.CreateSpec{CWD: id}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	sender.set(func(i *session.Info) { i.Attach = session.AttachStateAttached })
	dst := r.d.lookup("b")
	// Holding opMu keeps forget from unregistering the ended target, so the
	// Done check (not the registry check) must reject the delivery.
	r.d.opMu.Lock()
	target.once.Do(func() { close(target.done) })
	err := r.d.enqueueDelivery(context.Background(), dst, session.Input{Text: "x"})
	r.d.opMu.Unlock()
	if !errors.Is(err, session.ErrClosed) {
		t.Fatalf("send to closed target = %v", err)
	}
}
