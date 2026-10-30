package daemonclient

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("ZUT_TEST_DAEMON"); mode != "" {
		os.Exit(runChildDaemon(mode))
	}
	os.Exit(m.Run())
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func dial(t *testing.T, path string, opts Options) *Client {
	t.Helper()
	c, err := Dial(testCtx(t), path, opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Disconnect() })
	return c
}

func TestDialVersionMismatchWarnsAndContinues(t *testing.T) {
	_, path := newDaemon(t, "9.9.9", daemonproto.ProtocolVersion)
	var warn bytes.Buffer
	c := dial(t, path, Options{ClientVersion: "1.0.0", Warnings: &warn})
	require.Equal(t, "daemon is running zut 9.9.9; run \"zut daemon restart\"\n", warn.String())
	list, err := c.List(testCtx(t))
	require.NoError(t, err)
	require.Equal(t, []session.Info{{ID: "s1"}}, list)
}

func TestDialSameVersionIsSilent(t *testing.T) {
	_, path := newDaemon(t, "1.0.0", daemonproto.ProtocolVersion)
	var warn bytes.Buffer
	dial(t, path, Options{ClientVersion: "1.0.0", Warnings: &warn})
	require.Empty(t, warn.String())
}

func TestDialProtocolMismatchIsHardError(t *testing.T) {
	_, path := newDaemon(t, "1.0.0", daemonproto.ProtocolVersion+1)
	var warn bytes.Buffer
	c, err := Dial(testCtx(t), path, Options{ClientVersion: "1.0.0", Warnings: &warn})
	require.Nil(t, c)
	require.ErrorContains(t, err, "protocol 2")
}

func TestClientRequests(t *testing.T) {
	d, path := newDaemon(t, "v", daemonproto.ProtocolVersion)
	c := dial(t, path, Options{ClientVersion: "v"})
	ctx := testCtx(t)
	info, err := c.Create(ctx, session.CreateSpec{CWD: "/w"})
	require.NoError(t, err)
	require.Equal(t, "s2", info.ID)
	_, err = c.Clear(ctx, "s1")
	require.NoError(t, err)
	require.NoError(t, c.Detach(ctx, "s1"))
	require.NoError(t, c.Kill(ctx, "s1"))
	st, err := c.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, st.Sessions)

	p := c.Port("s1", true)
	require.NoError(t, p.Submit(ctx, session.Input{Text: "hi"}))
	in, err := p.SlideBackQueued(ctx)
	require.NoError(t, err)
	require.Equal(t, "queued", in.Text)
	require.NoError(t, p.ClearQueuedUser(ctx))
	tree, err := p.SessionsQuery(ctx)
	require.NoError(t, err)
	require.Equal(t, "/repo", tree.Workspaces[0].RepoRoot)
	require.ErrorIs(t, p.SetTitle(ctx, "x"), session.ErrBusy)
	calls := d.portCalls()
	require.Equal(t, "submit", calls[0].Method)
	var params struct {
		In session.Input `json:"in"`
	}
	require.NoError(t, json.Unmarshal(calls[0].Params, &params))
	require.Equal(t, "hi", params.In.Text)
	require.NoError(t, c.Stop(ctx, time.Second))
}

func TestClearQueuedUserOldDaemonIsUnknownRequest(t *testing.T) {
	_, path := newDaemon(t, "old", daemonproto.ProtocolVersion)
	c := dial(t, path, Options{ClientVersion: "old"})
	err := c.Port("s1", true).ClearQueuedUser(testCtx(t))
	require.ErrorIs(t, err, session.ErrUnknownRequest)
}

func recvSeqs(t *testing.T, ch <-chan session.Event, n int) []session.Event {
	t.Helper()
	out := make([]session.Event, 0, n)
	timeout := time.After(10 * time.Second)
	for len(out) < n {
		select {
		case ev, ok := <-ch:
			require.True(t, ok, "stream closed after %d events", len(out))
			out = append(out, ev)
		case <-timeout:
			t.Fatalf("timed out after %d of %d events", len(out), n)
		}
	}
	return out
}

func TestSubscribeReconnectReplaysWithoutGapsOrDuplicates(t *testing.T) {
	d, path := newDaemon(t, "v", daemonproto.ProtocolVersion)
	d.publish("1")
	d.publish("2")
	c := dial(t, path, Options{ClientVersion: "v"})
	ctx, cancel := context.WithCancel(testCtx(t))
	ch, err := c.Port("s1", true).Subscribe(ctx, 0)
	require.NoError(t, err)
	got := recvSeqs(t, ch, 2)
	d.publish("3")
	got = append(got, recvSeqs(t, ch, 1)...)

	d.dropConns()
	d.publish("4")
	d.publish("5")
	got = append(got, recvSeqs(t, ch, 2)...)
	d.publish("6")
	got = append(got, recvSeqs(t, ch, 1)...)
	for i, ev := range got {
		require.Equal(t, uint64(i+1), ev.Seq)
		require.Equal(t, session.TitleEvent{Title: string(rune('1' + i))}, ev.Data)
	}
	cancel()
	for range ch {
		t.Fatal("unexpected event after cancel")
	}
}

func TestSubscribeSeqTooOldAfterReconnectEmitsMessagesReplaced(t *testing.T) {
	d, path := newDaemon(t, "v", daemonproto.ProtocolVersion)
	d.publish("1")
	c := dial(t, path, Options{ClientVersion: "v"})
	ch, err := c.Port("s1", true).Subscribe(testCtx(t), 0)
	require.NoError(t, err)
	recvSeqs(t, ch, 1)

	d.dropConns()
	d.publish("2")
	d.publish("3")
	d.trim(3)
	ev := recvSeqs(t, ch, 1)[0]
	require.Equal(t, session.EvMessagesReplaced, ev.Kind)
	require.Equal(t, uint64(3), ev.Seq)
	d.publish("4")
	ev = recvSeqs(t, ch, 1)[0]
	require.Equal(t, uint64(4), ev.Seq)
	require.Equal(t, session.TitleEvent{Title: "4"}, ev.Data)
}

func TestSubscribeDaemonRestartSeqResetEmitsMessagesReplaced(t *testing.T) {
	d, path := newDaemon(t, "v", daemonproto.ProtocolVersion)
	for _, title := range []string{"1", "2", "3"} {
		d.publish(title)
	}
	c := dial(t, path, Options{ClientVersion: "v"})
	ch, err := c.Port("s1", true).Subscribe(testCtx(t), 0)
	require.NoError(t, err)
	recvSeqs(t, ch, 3)

	d.close() // daemon restart: sequence numbering starts over
	d2 := newDaemonAt(t, path, "v", daemonproto.ProtocolVersion)
	d2.publish("r1")
	ev := recvSeqs(t, ch, 1)[0]
	require.Equal(t, session.EvMessagesReplaced, ev.Kind)
	require.Equal(t, uint64(1), ev.Seq)
	d2.publish("r2")
	ev = recvSeqs(t, ch, 1)[0]
	require.Equal(t, uint64(2), ev.Seq)
	require.Equal(t, session.TitleEvent{Title: "r2"}, ev.Data)
}

func TestSubscribeRefusedReattachIsTerminal(t *testing.T) {
	d, path := newDaemon(t, "v", daemonproto.ProtocolVersion)
	c := dial(t, path, Options{ClientVersion: "v"})
	ch, err := c.Port("s1", true).Subscribe(testCtx(t), 0)
	require.NoError(t, err)
	d.setAttachErr(session.ErrBusy)
	d.dropConns()
	var last session.Event
	for ev := range ch {
		last = ev
	}
	require.Equal(t, session.EvClosed, last.Kind)
	require.Contains(t, last.Data.(session.ClosedEvent).Reason, session.ErrBusy.Error())
	require.Equal(t, 2, d.count("attach"), "refusal must not be retried")
}

func TestSubscribeCancelDetachesControllingPort(t *testing.T) {
	d, path := newDaemon(t, "v", daemonproto.ProtocolVersion)
	c := dial(t, path, Options{ClientVersion: "v"})
	ctx, cancel := context.WithCancel(testCtx(t))
	ch, err := c.Port("s1", true).Subscribe(ctx, 0)
	require.NoError(t, err)
	cancel()
	for range ch {
	}
	require.Eventually(t, func() bool { return d.count("detach") == 1 }, 5*time.Second, 10*time.Millisecond)

	ctx2, cancel2 := context.WithCancel(testCtx(t))
	ch, err = c.Port("s1", false).Subscribe(ctx2, 0)
	require.NoError(t, err)
	cancel2()
	for range ch {
	}
	require.NoError(t, c.Disconnect())
	require.Equal(t, 1, d.count("detach"))
}

func TestSubscribeAfterDisconnectFails(t *testing.T) {
	_, path := newDaemon(t, "v", daemonproto.ProtocolVersion)
	c := dial(t, path, Options{ClientVersion: "v"})
	require.NoError(t, c.Disconnect())
	_, err := c.Port("s1", true).Subscribe(testCtx(t), 0)
	require.ErrorIs(t, err, daemonproto.ErrClosed)
}

func TestSocketPathLengthLimit(t *testing.T) {
	long := "/" + strings.Repeat("a", 110)
	require.ErrorContains(t, checkSocketPathFor("darwin", long[:104]), "ZUT_DAEMON_SOCKET")
	require.NoError(t, checkSocketPathFor("darwin", long[:103]))
	require.ErrorContains(t, checkSocketPathFor("linux", long[:108]), "ZUT_DAEMON_SOCKET")
	require.NoError(t, checkSocketPathFor("linux", long[:107]))
	require.NoError(t, checkSocketPathFor("windows", long))
	_, err := Dial(testCtx(t), long, Options{})
	if os.PathSeparator == '/' {
		require.ErrorContains(t, err, "ZUT_DAEMON_SOCKET")
	}
}

func TestSubscribeInitialSeqTooOldIsReturned(t *testing.T) {
	d, path := newDaemon(t, "v", daemonproto.ProtocolVersion)
	d.publish("1")
	d.trim(1)
	c := dial(t, path, Options{ClientVersion: "v"})
	_, err := c.Port("s1", true).Subscribe(testCtx(t), 0)
	require.ErrorIs(t, err, session.ErrSeqTooOld)
}

func TestSubscribeSlowConsumerIsDropped(t *testing.T) {
	d, path := newDaemon(t, "v", daemonproto.ProtocolVersion)
	c := dial(t, path, Options{ClientVersion: "v"})
	ch, err := c.Port("s1", true).Subscribe(testCtx(t), 0)
	require.NoError(t, err)
	for i := 0; i < SubscriberBuffer+10; i++ {
		d.publish("x")
	}
	var last session.Event
	n := 0
	for ev := range ch {
		last = ev
		n++
	}
	require.Equal(t, SubscriberBuffer+1, n)
	require.Equal(t, session.EvAttach, last.Kind)
	require.Equal(t, session.AttachEvent{State: session.AttachStateDetached, Reason: "slow_consumer"}, last.Data)
}

func waitAttach(t *testing.T, d *miniDaemon) {
	t.Helper()
	select {
	case <-d.onAttach:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for attach")
	}
}

func TestSubscribeDaemonSlowConsumerDropReattaches(t *testing.T) {
	for _, controlling := range []bool{true, false} {
		d, path := newDaemon(t, "v", daemonproto.ProtocolVersion)
		d.onAttach = make(chan struct{}, 8)
		d.publish("1")
		c := dial(t, path, Options{ClientVersion: "v"})
		ch, err := c.Port("s1", controlling).Subscribe(testCtx(t), 0)
		require.NoError(t, err)
		waitAttach(t, d)

		// The reader is blocked (ch is not drained) while the daemon drops
		// the subscriber mid-publish and more events arrive meanwhile.
		d.publish("2")
		d.dropSlowConsumer("3")
		waitAttach(t, d)
		d.publish("4")

		got := recvSeqs(t, ch, 4)
		for i, ev := range got {
			require.Equal(t, uint64(i+1), ev.Seq)
			require.Equal(t, session.TitleEvent{Title: string(rune('1' + i))}, ev.Data)
		}
		attaches := d.attachRequests()
		require.Len(t, attaches, 2)
		require.Equal(t, uint64(2), attaches[1].AfterSeq)
		for _, a := range attaches {
			require.Equal(t, controlling, a.Controlling, "re-attach must keep the subscription's control mode")
		}
		require.Equal(t, 1, d.count("snapshot"))
	}
}

func TestSubscribeDaemonSlowConsumerDropsWithoutProgressAreBounded(t *testing.T) {
	d, path := newDaemon(t, "v", daemonproto.ProtocolVersion)
	d.onAttach = make(chan struct{}, 8)
	c := dial(t, path, Options{ClientVersion: "v"})
	ch, err := c.Port("s1", true).Subscribe(testCtx(t), 0)
	require.NoError(t, err)
	waitAttach(t, d)
	for i := 0; i < maxSlowResubscribes; i++ {
		d.dropSlowConsumer("")
		waitAttach(t, d)
	}
	d.dropSlowConsumer("")
	var evs []session.Event
	for ev := range ch {
		evs = append(evs, ev)
	}
	require.Len(t, evs, 1)
	require.Equal(t, session.AttachEvent{State: session.AttachStateDetached, Reason: "slow_consumer"}, evs[0].Data)
	require.Equal(t, 1+maxSlowResubscribes, d.count("attach"))
}

func TestEnsureDaemonAutospawns(t *testing.T) {
	path := shortSocket(t)
	home := t.TempDir()
	exe, err := os.Executable()
	require.NoError(t, err)
	env := append(os.Environ(), "ZUT_TEST_DAEMON=1", "ZUT_DAEMON_SOCKET="+path)
	getenv := func(k string) string {
		switch k {
		case "ZUT_DAEMON_SOCKET":
			return path
		case "ZUT_HOME":
			return home
		}
		return ""
	}
	var warn bytes.Buffer
	c, err := EnsureDaemon(testCtx(t), EnsureOptions{Options: Options{ClientVersion: "v", Warnings: &warn}, Getenv: getenv, Env: env, Executable: exe})
	require.NoError(t, err)
	defer c.Disconnect()
	require.Equal(t, "child-version", c.DaemonVersion())
	require.Contains(t, warn.String(), "daemon is running zut child-version")
	st, err := os.Stat(LogPath(home))
	require.NoError(t, err)
	if os.PathSeparator == '/' {
		require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	}
	// A running daemon is only dialed: a bogus executable is never spawned.
	c2, err := EnsureDaemon(testCtx(t), EnsureOptions{Options: Options{ClientVersion: "child-version"}, Getenv: getenv, Executable: "/nonexistent/zut"})
	require.NoError(t, err)
	require.NoError(t, c2.Disconnect())
	require.NoError(t, c.Stop(testCtx(t), time.Second))
}

func TestEnsureDaemonSpawnFailureReportsLogTail(t *testing.T) {
	path := shortSocket(t)
	home := t.TempDir()
	exe, err := os.Executable()
	require.NoError(t, err)
	env := append(os.Environ(), "ZUT_TEST_DAEMON=fail")
	_, err = EnsureDaemon(testCtx(t), EnsureOptions{SocketPath: path, Home: home, Env: env, Executable: exe})
	require.Error(t, err)
	require.Contains(t, err.Error(), "synthetic daemon startup failure")
	require.Contains(t, err.Error(), "did not start within")
	require.Contains(t, err.Error(), "exit status 3")
	require.True(t, strings.Contains(err.Error(), LogPath(home)))
}

func TestEnsureDaemonLoserExitsWinnerListensLate(t *testing.T) {
	path := shortSocket(t)
	home := t.TempDir()
	exe, err := os.Executable()
	require.NoError(t, err)
	env := append(os.Environ(), "ZUT_TEST_DAEMON=loser")
	type result struct {
		c   *Client
		err error
	}
	res := make(chan result, 1)
	go func() {
		c, err := EnsureDaemon(testCtx(t), EnsureOptions{Options: Options{ClientVersion: "v"}, SocketPath: path, Home: home, Env: env, Executable: exe})
		res <- result{c, err}
	}()
	// Start the winner only once the spawned loser has run and exited.
	require.Eventually(t, func() bool {
		b, _ := os.ReadFile(LogPath(home))
		return strings.Contains(string(b), "loser exiting")
	}, 4*time.Second, 10*time.Millisecond)
	newDaemonAt(t, path, "v", daemonproto.ProtocolVersion)
	r := <-res
	require.NoError(t, r.err)
	require.NoError(t, r.c.Disconnect())
}

func TestSocketPathResolution(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	cases := []struct {
		goos string
		env  func(string) string
		want string
	}{
		{"linux", env("ZUT_DAEMON_SOCKET", "/o.sock", "XDG_RUNTIME_DIR", "/run/u"), "/o.sock"},
		{"linux", env("XDG_RUNTIME_DIR", "/run/u", "ZUT_HOME", "/h"), "/run/u/zut/daemon.sock"},
		{"linux", env("ZUT_HOME", "/h"), "/h/run/daemon.sock"},
		{"linux", env("HOME", "/home/u"), "/home/u/.local/state/zut/run/daemon.sock"},
		{"windows", env("LOCALAPPDATA", `C:\L`, "ZUT_HOME", `C:\h`), `C:\L\zut\run\daemon.sock`},
		{"windows", env("ZUT_HOME", `C:\h`), `C:\h\run\daemon.sock`},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, socketPath(tc.goos, tc.env), tc.goos)
	}
}
