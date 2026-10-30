package mcp

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pooledManager builds and starts a Manager on pool for the given servers.
func pooledManager(t *testing.T, pool *Pool, servers map[string]ServerConfig, cwd string) (*Manager, []error, error) {
	t.Helper()
	m := NewManager(Config{Servers: servers}, cwd, ClientInfo{Name: "zut-test", Version: "0"}, nil)
	m.SetPool(pool)
	t.Cleanup(func() { _ = m.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	failures, reqErr := m.Start(ctx)
	return m, failures, reqErr
}

func mustPooled(t *testing.T, pool *Pool, servers map[string]ServerConfig, cwd string) *Manager {
	t.Helper()
	m, failures, reqErr := pooledManager(t, pool, servers, cwd)
	if len(failures) != 0 || reqErr != nil {
		t.Fatalf("start: %v / %v", failures, reqErr)
	}
	return m
}

func serverPID(t *testing.T, m *Manager) int {
	t.Helper()
	pid, err := strconv.Atoi(resultText(execTool(t, findTool(t, m, "mcp__fake__pid"), `{}`)))
	if err != nil {
		t.Fatalf("pid: %v", err)
	}
	return pid
}

func newTestPool(t *testing.T, idle time.Duration) *Pool {
	t.Helper()
	p := NewPool(idle, nil)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestPoolSharesProcessBetweenManagers(t *testing.T) {
	cfg, _ := fakeStdioConfig(t, nil)
	cwd := t.TempDir()
	pool := newTestPool(t, time.Minute)
	a := mustPooled(t, pool, map[string]ServerConfig{"fake": cfg}, cwd)
	b := mustPooled(t, pool, map[string]ServerConfig{"fake": cfg}, cwd)
	if pa, pb := serverPID(t, a), serverPID(t, b); pa != pb {
		t.Fatalf("pids differ: %d vs %d", pa, pb)
	}
}

func TestPoolNotUsedWhenUnsharedOrNoPool(t *testing.T) {
	cwd := t.TempDir()
	off := false
	cfg, _ := fakeStdioConfig(t, nil)
	unshared := cfg
	unshared.Shared = &off
	pool := newTestPool(t, time.Minute)
	a := mustPooled(t, pool, map[string]ServerConfig{"fake": unshared}, cwd)
	b := mustPooled(t, pool, map[string]ServerConfig{"fake": unshared}, cwd)
	if serverPID(t, a) == serverPID(t, b) {
		t.Fatal("shared:false must not share a process")
	}
	c := mustPooled(t, nil, map[string]ServerConfig{"fake": cfg}, cwd)
	d := mustPooled(t, nil, map[string]ServerConfig{"fake": cfg}, cwd)
	if serverPID(t, c) == serverPID(t, d) {
		t.Fatal("no pool must not share a process")
	}
}

func TestPoolPerManagerToolFiltering(t *testing.T) {
	cfg, _ := fakeStdioConfig(t, nil)
	cwd := t.TempDir()
	pool := newTestPool(t, time.Minute)
	filtered := cfg
	filtered.DisabledTools = []string{"echo"}
	a := mustPooled(t, pool, map[string]ServerConfig{"fake": cfg}, cwd)
	b := mustPooled(t, pool, map[string]ServerConfig{"fake": filtered}, cwd)
	if serverPID(t, a) != serverPID(t, b) {
		t.Fatal("filters must not split the process")
	}
	if !hasTool(a, "mcp__fake__echo") {
		t.Error("A lost echo")
	}
	if hasTool(b, "mcp__fake__echo") {
		t.Error("B must not see disabled echo")
	}
	for _, st := range b.Status() {
		if st.Tools != len(b.ServerTools("fake")) {
			t.Errorf("status tools = %d, want %d", st.Tools, len(b.ServerTools("fake")))
		}
	}
}

func TestPoolClosingOneManagerKeepsOther(t *testing.T) {
	cfg, _ := fakeStdioConfig(t, nil)
	cwd := t.TempDir()
	pool := newTestPool(t, 0)
	a := mustPooled(t, pool, map[string]ServerConfig{"fake": cfg}, cwd)
	b := mustPooled(t, pool, map[string]ServerConfig{"fake": cfg}, cwd)
	pid := serverPID(t, a)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if got := serverPID(t, b); got != pid {
		t.Fatalf("B pid = %d, want shared %d", got, pid)
	}
	if canCheckProcess && !processAlive(pid) {
		t.Fatal("shared process died with A")
	}
}

func TestPoolIdleZeroStopsAfterLastRelease(t *testing.T) {
	if !canCheckProcess {
		t.Skip("process liveness check unavailable")
	}
	cfg, _ := fakeStdioConfig(t, nil)
	cwd := t.TempDir()
	pool := newTestPool(t, 0)
	a := mustPooled(t, pool, map[string]ServerConfig{"fake": cfg}, cwd)
	b := mustPooled(t, pool, map[string]ServerConfig{"fake": cfg}, cwd)
	pid := serverPID(t, a)
	_ = a.Close()
	_ = b.Close()
	eventually(t, "shared process stopped", func() bool { return !processAlive(pid) })
}

func TestPoolReuseDuringIdle(t *testing.T) {
	cfg, _ := fakeStdioConfig(t, nil)
	cwd := t.TempDir()
	pool := newTestPool(t, time.Minute)
	a := mustPooled(t, pool, map[string]ServerConfig{"fake": cfg}, cwd)
	pid := serverPID(t, a)
	_ = a.Close()
	b := mustPooled(t, pool, map[string]ServerConfig{"fake": cfg}, cwd)
	if got := serverPID(t, b); got != pid {
		t.Fatalf("idle process not reused: %d vs %d", got, pid)
	}
}

func TestPoolCrashRestartsOnce(t *testing.T) {
	cfg, events := fakeStdioConfig(t, nil)
	cwd := t.TempDir()
	pool := newTestPool(t, time.Minute)
	a := mustPooled(t, pool, map[string]ServerConfig{"fake": cfg}, cwd)
	b := mustPooled(t, pool, map[string]ServerConfig{"fake": cfg}, cwd)
	old := serverPID(t, a)
	starts := countEvents(events, "start")
	_ = execTool(t, findTool(t, a, "mcp__fake__exit"), `{}`)
	waitEvent(t, events, "exit-tool")
	if canCheckProcess {
		eventually(t, "server exited", func() bool { return !processAlive(old) })
	}
	var wg sync.WaitGroup
	pids := make([]int, 8)
	for i := range pids {
		m := a
		if i%2 == 1 {
			m = b
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			tool := findTool(t, m, "mcp__fake__pid")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			res, err := tool.Execute(ctx, []byte(`{}`), nil)
			if err == nil && !res.IsError {
				pids[i], _ = strconv.Atoi(resultText(res))
			}
		}()
	}
	wg.Wait()
	// Each Manager may have a call that raced the exit; every successful
	// call must land on the single restarted process.
	restarted := 0
	for _, p := range pids {
		if p == 0 {
			continue
		}
		if p == old {
			t.Fatalf("call reached the exited process %d", old)
		}
		if restarted == 0 {
			restarted = p
		} else if p != restarted {
			t.Fatalf("more than one restart: %v", pids)
		}
	}
	if restarted == 0 {
		t.Fatalf("no call succeeded after restart: %v", pids)
	}
	if got := countEvents(events, "start") - starts; got != 1 {
		t.Fatalf("restarts = %d, want 1", got)
	}
}

func TestPoolToolsListChangedFansOut(t *testing.T) {
	cfg, _ := fakeStdioConfig(t, nil)
	cwd := t.TempDir()
	pool := newTestPool(t, time.Minute)
	a := mustPooled(t, pool, map[string]ServerConfig{"fake": cfg}, cwd)
	b := NewManager(Config{Servers: map[string]ServerConfig{"fake": cfg}}, cwd, ClientInfo{}, nil)
	b.SetPool(pool)
	var fired atomic.Int32
	b.SetToolsChangedObserver(func(server string) {
		if server == "fake" {
			fired.Add(1)
		}
	})
	t.Cleanup(func() { _ = b.Close() })
	if failures, err := b.Start(context.Background()); len(failures) != 0 || err != nil {
		t.Fatal(failures, err)
	}
	if hasTool(b, "mcp__fake__extra") {
		t.Fatal("extra exists too early")
	}
	_ = execTool(t, findTool(t, a, "mcp__fake__add_tool"), `{}`)
	eventually(t, "B sees extra", func() bool { return hasTool(b, "mcp__fake__extra") })
	eventually(t, "B callback fired", func() bool { return fired.Load() > 0 })
	if !hasTool(a, "mcp__fake__extra") {
		t.Error("A must see extra too")
	}
}

func TestPoolAbsoluteCWDKeepsSessionRoots(t *testing.T) {
	cfg, _ := fakeStdioConfig(t, nil)
	cfg.CWD = t.TempDir()
	session := t.TempDir()
	pool := newTestPool(t, time.Minute)
	m := mustPooled(t, pool, map[string]ServerConfig{"fake": cfg}, session)
	text := resultText(execTool(t, findTool(t, m, "mcp__fake__ask_roots"), `{}`))
	if !strings.Contains(text, "file://") || !strings.Contains(text, filepath.Base(session)) {
		t.Fatalf("roots answer = %q, want session cwd %q", text, session)
	}
}

func TestPoolCloseStopsProcessesAndRejectsConnects(t *testing.T) {
	cfg, _ := fakeStdioConfig(t, nil)
	cwd := t.TempDir()
	pool := NewPool(time.Minute, nil)
	a := mustPooled(t, pool, map[string]ServerConfig{"fake": cfg}, cwd)
	pid := serverPID(t, a)
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if canCheckProcess {
		eventually(t, "process stopped", func() bool { return !processAlive(pid) })
	}
	_, failures, _ := pooledManager(t, pool, map[string]ServerConfig{"fake": cfg}, cwd)
	if len(failures) != 1 || !strings.Contains(failures[0].Error(), "pool closed") {
		t.Fatalf("failures = %v", failures)
	}
	res := execTool(t, findTool(t, a, "mcp__fake__pid"), `{}`)
	if !res.IsError || !strings.Contains(resultText(res), "pool closed") {
		t.Fatalf("call after pool close = %q", resultText(res))
	}
}

func TestPoolRequiredFailureReportedPerManager(t *testing.T) {
	cfg, _ := fakeStdioConfig(t, map[string]string{"FAKE_INIT_ERR": "1"})
	cfg.Required = true
	cwd := t.TempDir()
	pool := newTestPool(t, time.Minute)
	for _, name := range []string{"A", "B"} {
		_, failures, reqErr := pooledManager(t, pool, map[string]ServerConfig{"fake": cfg}, cwd)
		if len(failures) != 1 || reqErr == nil || !strings.Contains(reqErr.Error(), "init refused") {
			t.Fatalf("%s: failures=%v reqErr=%v", name, failures, reqErr)
		}
	}
}

func TestPoolWithStartBackground(t *testing.T) {
	cfg, _ := fakeStdioConfig(t, map[string]string{"FAKE_INIT_DELAY_MS": "300"})
	cwd := t.TempDir()
	pool := newTestPool(t, time.Minute)
	ready := make(chan string, 2)
	var managers []*Manager
	for range 2 {
		m := NewManager(Config{Servers: map[string]ServerConfig{"fake": cfg}}, cwd, ClientInfo{}, nil)
		m.SetPool(pool)
		t.Cleanup(func() { _ = m.Close() })
		if _, err := m.StartBackground(context.Background(), func(_ context.Context, name string, _ time.Duration, err error) {
			if err != nil {
				t.Errorf("ready err: %v", err)
			}
			ready <- name
		}); err != nil {
			t.Fatal(err)
		}
		managers = append(managers, m)
	}
	for range 2 {
		select {
		case <-ready:
		case <-time.After(10 * time.Second):
			t.Fatal("background start never ready")
		}
	}
	if serverPID(t, managers[0]) != serverPID(t, managers[1]) {
		t.Fatal("background starts must share one process")
	}
}

func TestPoolSlowSubscriberDoesNotDelayOthers(t *testing.T) {
	cfg, _ := fakeStdioConfig(t, nil)
	cwd := t.TempDir()
	pool := newTestPool(t, time.Minute)
	block := make(chan struct{})
	slowEntered := make(chan struct{}, 1)
	fastFired := make(chan struct{}, 1)
	start := func(observer func(string)) *Manager {
		m := NewManager(Config{Servers: map[string]ServerConfig{"fake": cfg}}, cwd, ClientInfo{}, nil)
		m.SetPool(pool)
		m.SetToolsChangedObserver(observer)
		t.Cleanup(func() { _ = m.Close() })
		if failures, err := m.Start(context.Background()); len(failures) != 0 || err != nil {
			t.Fatal(failures, err)
		}
		return m
	}
	slow := start(func(string) {
		select {
		case slowEntered <- struct{}{}:
		default:
		}
		<-block
	})
	fast := start(func(string) {
		select {
		case fastFired <- struct{}{}:
		default:
		}
	})
	defer close(block)
	_ = execTool(t, findTool(t, slow, "mcp__fake__add_tool"), `{}`)
	for _, w := range []struct {
		ch   chan struct{}
		what string
	}{{slowEntered, "slow callback entered"}, {fastFired, "fast callback while slow blocked"}} {
		select {
		case <-w.ch:
		case <-time.After(10 * time.Second):
			t.Fatal("timed out: " + w.what)
		}
	}
	if !hasTool(fast, "mcp__fake__extra") {
		t.Fatal("fast manager missing the new tool while slow is blocked")
	}
}

func TestPoolFlightUsesLargestWaiterTimeout(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "release")
	cfg, events := fakeStdioConfig(t, map[string]string{"FAKE_INIT_GATE": gate})
	short := cfg
	short.StartupTimeoutSec = 0.2
	long := cfg
	long.StartupTimeoutSec = 30
	cwd := t.TempDir()
	pool := newTestPool(t, time.Minute)
	type result struct{ failures []error }
	start := func(c ServerConfig) chan result {
		ch := make(chan result, 1)
		m := NewManager(Config{Servers: map[string]ServerConfig{"fake": c}}, cwd, ClientInfo{}, nil)
		m.SetPool(pool)
		t.Cleanup(func() { _ = m.Close() })
		go func() { f, _ := m.Start(context.Background()); ch <- result{f} }()
		return ch
	}
	shortDone := start(short)
	waitEvent(t, events, "init-held")
	longDone := start(long)
	// The short waiter gives up with its own timeout in the message.
	select {
	case r := <-shortDone:
		if len(r.failures) != 1 || !strings.Contains(r.failures[0].Error(), "timed out after 200ms") {
			t.Fatalf("short failures = %v", r.failures)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("short waiter did not time out")
	}
	// The shared dial lives on with the long timeout: release it and the
	// long waiter succeeds on the same process.
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-longDone:
		if len(r.failures) != 0 {
			t.Fatalf("long failures = %v", r.failures)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("long waiter never finished")
	}
	if got := countEvents(events, "start"); got != 1 {
		t.Fatalf("server starts = %d, want 1", got)
	}
}

// lockedBuf is a goroutine-safe log destination.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestPoolForwardsRedactedStderrToLog(t *testing.T) {
	const secret = "POOLSECRET-abcdefghij"
	cfg, _ := fakeStdioConfig(t, map[string]string{"FAKE_SECRET": secret})
	var buf lockedBuf
	pool := NewPool(time.Minute, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { _ = pool.Close() })
	m := mustPooled(t, pool, map[string]ServerConfig{"fake": cfg}, t.TempDir())
	if got := resultText(execTool(t, findTool(t, m, "mcp__fake__stderr_secret"), `{}`)); got != "written" {
		t.Fatal(got)
	}
	eventually(t, "stderr forwarded to the pool log", func() bool {
		return strings.Contains(buf.String(), `msg="mcp server stderr" server=fake line=after-secret`)
	})
	out := buf.String()
	if strings.Contains(out, "POOLSECRET") || strings.Contains(out, "abcdef") {
		t.Fatal("secret leaked to the daemon log")
	}
	if strings.Contains(out, "-test.run") || strings.Contains(out, fakeServerEnv) {
		t.Fatal("args or env leaked to the daemon log")
	}
}
