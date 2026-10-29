package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/daemonclient"
	"github.com/bnema/zut/packages/agent/internal/flock"
	"github.com/bnema/zut/packages/agent/scheduler"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
	"github.com/stretchr/testify/require"
)

// daemonTestSocket keeps sun_path short (macOS 104-byte limit).
func daemonTestSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "zd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

type testDaemon struct {
	server *daemonServer
	socket string
	done   chan error
	once   sync.Once
}

// startFactoryDaemon runs the real daemon + session factory in-process.
func startFactoryDaemon(t *testing.T, home, socket string) *testDaemon {
	t.Helper()
	ready := make(chan struct{})
	s, err := newDaemonServer(daemonServerOptions{Home: home, SocketPath: socket, Version: "test", Ready: ready})
	require.NoError(t, err)
	td := &testDaemon{server: s, socket: socket, done: make(chan error, 1)}
	go func() { td.done <- s.run(context.Background()) }()
	select {
	case <-ready:
	case err := <-td.done:
		t.Fatalf("daemon exited: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon not ready")
	}
	t.Cleanup(td.stop)
	return td
}

func (td *testDaemon) stop() {
	td.once.Do(func() {
		td.server.daemon.Stop()
		select {
		case <-td.done:
		case <-time.After(20 * time.Second):
			panic("daemon did not stop")
		}
	})
}

func dialTestDaemon(t *testing.T, socket string) *daemonclient.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := daemonclient.Dial(ctx, socket, daemonclient.Options{ClientVersion: "test", ClientKind: "tui"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Disconnect() })
	return c
}

func factoryTestCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// waitAssistantText reads events until the turn ends and returns the text.
func waitAssistantText(t *testing.T, ctx context.Context, events <-chan session.Event) string {
	t.Helper()
	var text strings.Builder
	for {
		select {
		case ev, ok := <-events:
			require.True(t, ok, "event stream ended before turn_end")
			switch a := ev.Agent.(type) {
			case core.EvTextDelta:
				text.WriteString(a.Delta)
			case *core.EvTextDelta:
				text.WriteString(a.Delta)
			case core.EvTurnEnd, *core.EvTurnEnd:
				return text.String()
			}
		case <-ctx.Done():
			t.Fatal("no turn_end:", ctx.Err())
		}
	}
}

func submitAndWait(t *testing.T, ctx context.Context, c *daemonclient.Client, id session.ID) string {
	t.Helper()
	port := c.Port(id, true)
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := port.Subscribe(subCtx, 0)
	require.NoError(t, err)
	require.NoError(t, port.Submit(ctx, session.Input{Text: "hello", Origin: session.InputOriginUser}))
	return waitAssistantText(t, ctx, events)
}

func TestDaemonFactoryTwoSessionsDifferentCWD(t *testing.T) {
	home := sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	td := startFactoryDaemon(t, home, daemonTestSocket(t))
	c := dialTestDaemon(t, td.socket)
	ctx := factoryTestCtx(t)

	cwdA, cwdB := t.TempDir(), t.TempDir()
	env := []string{"PATH=" + os.Getenv("PATH")}
	a, err := c.Create(ctx, session.CreateSpec{CWD: cwdA, Env: env, Args: sessionAssemblyArgs(server)})
	require.NoError(t, err)
	b, err := c.Create(ctx, session.CreateSpec{CWD: cwdB, Env: env, Args: sessionAssemblyArgs(server)})
	require.NoError(t, err)
	require.NotEqual(t, a.ID, b.ID)
	require.Equal(t, cwdA, a.CWD)
	require.Equal(t, cwdB, b.CWD)

	require.Equal(t, "done", submitAndWait(t, ctx, c, session.ID(a.ID)))
	require.Equal(t, "done", submitAndWait(t, ctx, c, session.ID(b.ID)))

	list, err := c.List(ctx)
	require.NoError(t, err)
	cwds := map[string]string{}
	for _, info := range list {
		cwds[info.ID] = info.CWD
	}
	require.Equal(t, map[string]string{a.ID: cwdA, b.ID: cwdB}, cwds)
	require.NotNil(t, td.server.factory.lookup(session.ID(a.ID)).Controller)
}

func TestDaemonFactoryCloseRunsCleanup(t *testing.T) {
	home := sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	td := startFactoryDaemon(t, home, daemonTestSocket(t))
	c := dialTestDaemon(t, td.socket)
	ctx := factoryTestCtx(t)

	info, err := c.Create(ctx, session.CreateSpec{CWD: t.TempDir(), Env: []string{}, Args: sessionAssemblyArgs(server)})
	require.NoError(t, err)
	id := session.ID(info.ID)
	require.Equal(t, "done", submitAndWait(t, ctx, c, id))
	h := td.server.factory.lookup(id)
	require.NotNil(t, h)
	path := h.Info().SessionPath
	require.NotEmpty(t, path)

	_, err = c.Attach(ctx, id, 0, true)
	require.NoError(t, err)
	require.NoError(t, c.Close(ctx, id, "user_exit"))
	select {
	case <-h.Done():
	case <-ctx.Done():
		t.Fatal("handle not done after close")
	}
	require.Nil(t, td.server.factory.lookup(id))
	// Cleanup closed the transcript and the daemon released its file lock.
	lock, err := flock.TryLock(path + ".lock")
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	list, err := c.List(ctx)
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestDaemonFactoryNoGoroutineLeak(t *testing.T) {
	home := sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	// Warm package-level lazies with one daemon lifecycle first.
	warm := startFactoryDaemon(t, home, daemonTestSocket(t))
	wc := dialTestDaemon(t, warm.socket)
	ctx := factoryTestCtx(t)
	_, err := wc.Create(ctx, session.CreateSpec{CWD: t.TempDir(), Env: []string{}, Args: sessionAssemblyArgs(server)})
	require.NoError(t, err)
	_ = wc.Disconnect()
	warm.stop()
	before := len(packageGoroutines())

	td := startFactoryDaemon(t, home, daemonTestSocket(t))
	c := dialTestDaemon(t, td.socket)
	info, err := c.Create(ctx, session.CreateSpec{CWD: t.TempDir(), Env: []string{}, Args: sessionAssemblyArgs(server)})
	require.NoError(t, err)
	require.Equal(t, "done", submitAndWait(t, ctx, c, session.ID(info.ID)))
	_, err = c.Attach(ctx, session.ID(info.ID), 0, true)
	require.NoError(t, err)
	require.NoError(t, c.Close(ctx, session.ID(info.ID), "user_exit"))
	_ = c.Disconnect()
	td.stop()

	deadline := time.Now().Add(5 * time.Second)
	for {
		after := packageGoroutines()
		if len(after) <= before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines in zut packages = %d, want <= %d\n%s", len(after), before, strings.Join(after, "\n\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDaemonFactoryRestoreIdleInterrupted(t *testing.T) {
	home := sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	socket := daemonTestSocket(t)
	td := startFactoryDaemon(t, home, socket)
	c := dialTestDaemon(t, socket)
	ctx := factoryTestCtx(t)
	cwd := t.TempDir()
	info, err := c.Create(ctx, session.CreateSpec{CWD: cwd, Env: []string{}, Args: sessionAssemblyArgs(server)})
	require.NoError(t, err)
	require.Equal(t, "done", submitAndWait(t, ctx, c, session.ID(info.ID)))
	_ = c.Disconnect()
	td.stop()

	// Simulate a daemon that died mid-turn: state.json records it running.
	statePath := filepath.Join(home, "daemon", "state.json")
	raw, err := os.ReadFile(statePath)
	require.NoError(t, err)
	var st map[string]any
	require.NoError(t, json.Unmarshal(raw, &st))
	sessions := st["sessions"].([]any)
	require.Len(t, sessions, 1)
	sessions[0].(map[string]any)["running"] = true
	raw, err = json.Marshal(st)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statePath, raw, 0o600))

	startFactoryDaemon(t, home, socket)
	c2 := dialTestDaemon(t, socket)
	list, err := c2.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	got := list[0]
	require.Equal(t, info.ID, got.ID)
	require.Equal(t, cwd, got.CWD)
	require.Equal(t, info.SessionPath, got.SessionPath)
	require.Equal(t, session.RunStateIdle, got.Run)
	require.Equal(t, session.AttachStateDetached, got.Attach)
	require.Equal(t, "interrupted", got.Activity.Kind)
	snap, err := c2.Snapshot(ctx, session.ID(got.ID))
	require.NoError(t, err)
	require.NotEmpty(t, snap.Notes)
	// The restored session runs turns on top of its resumed transcript
	// (the controller builds the agent lazily on the first turn).
	require.Equal(t, "done", submitAndWait(t, ctx, c2, session.ID(got.ID)))
	snap, err = c2.Snapshot(ctx, session.ID(got.ID))
	require.NoError(t, err)
	users := 0
	for _, m := range snap.Messages {
		if m.Role == "user" {
			users++
		}
	}
	require.Equal(t, 2, users, "resumed user turn plus the new one")
}

func TestDaemonFactorySchedulesScopedAndDroppedOnClose(t *testing.T) {
	home := sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	td := startFactoryDaemon(t, home, daemonTestSocket(t))
	c := dialTestDaemon(t, td.socket)
	ctx := factoryTestCtx(t)
	info, err := c.Create(ctx, session.CreateSpec{CWD: t.TempDir(), Env: []string{}, Args: sessionAssemblyArgs(server)})
	require.NoError(t, err)
	id := session.ID(info.ID)
	f := td.server.factory
	host := sessionSchedule{engine: f.engine, id: info.ID}
	task, err := host.Add(schedulerInput("0 0 1 1 *", "later"))
	require.NoError(t, err)
	require.Equal(t, info.ID, task.SessionID)
	other := sessionSchedule{engine: f.engine, id: "someone-else"}
	require.Empty(t, other.List())
	require.False(t, other.Cancel(task.ID))
	require.Len(t, host.List(), 1)

	_, err = c.Attach(ctx, id, 0, true)
	require.NoError(t, err)
	require.NoError(t, c.Close(ctx, id, "user_exit"))
	require.Eventually(t, func() bool { return len(f.engine.List()) == 0 }, 5*time.Second, 10*time.Millisecond)
}

// TestDaemonRunCLIStatusStop builds zut and drives `zut daemon run`,
// `status` and `stop` as separate processes.
func TestDaemonRunCLIStatusStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal and subprocess semantics differ on Windows")
	}
	if testing.Short() {
		t.Skip("builds the zut binary")
	}
	binary := buildOrchestrationCLI(t)
	home := t.TempDir()
	socket := daemonTestSocket(t)
	env := append(os.Environ(), "ZUT_HOME="+home, "HOME="+t.TempDir(), "ZUT_DAEMON_SOCKET="+socket)

	run := exec.Command(binary, "daemon", "run")
	run.Env = env
	outPath := filepath.Join(t.TempDir(), "run.out")
	outFile, err := os.Create(outPath)
	require.NoError(t, err)
	defer outFile.Close()
	run.Stdout, run.Stderr = outFile, outFile
	runOutput := func() string { b, _ := os.ReadFile(outPath); return string(b) }
	require.NoError(t, run.Start())
	exited := make(chan error, 1)
	waited := make(chan struct{})
	go func() { exited <- run.Wait(); close(waited) }()
	t.Cleanup(func() {
		_ = run.Process.Kill()
		<-waited
	})

	zut := func(args ...string) string {
		cmd := exec.Command(binary, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%v: %s", args, out)
		return string(out)
	}
	require.Eventually(t, func() bool {
		return strings.Contains(zut("daemon", "status"), "zut daemon: running")
	}, 10*time.Second, 50*time.Millisecond, "daemon did not report running")
	status := zut("daemon", "status")
	require.Contains(t, status, "sessions: 0")
	require.Contains(t, status, "pid: ")

	require.Contains(t, zut("daemon", "stop"), "zut daemon: stopped")
	select {
	case err := <-exited:
		require.NoError(t, err, "daemon run output: %s", runOutput())
	case <-time.After(15 * time.Second):
		t.Fatal("zut daemon run did not exit after stop")
	}
	require.Contains(t, zut("daemon", "status"), "zut daemon: not running")
	require.FileExists(t, filepath.Join(home, "logs", "daemon.log"))
	require.Contains(t, runOutput(), "daemon started")
	fi, err := os.Stat(filepath.Join(home, "logs", "daemon.log"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

func schedulerInput(cron, msg string) scheduler.NewTaskInput {
	return scheduler.NewTaskInput{Cron: cron, Message: msg, Location: time.UTC}
}
