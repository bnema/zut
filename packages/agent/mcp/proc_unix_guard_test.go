//go:build !windows

package mcp

import (
	"strconv"
	"sync"
	"syscall"
	"testing"
)

// fakeKill records signals and answers from a script.
type fakeKill struct {
	mu    sync.Mutex
	calls []int // pids passed, in order (negative = group)
	err   error
}

func (f *fakeKill) kill(pid int, _ syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, pid)
	return f.err
}

func (f *fakeKill) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newGuardForTest(pgid int, k *fakeKill) *processGuard {
	return &processGuard{pgid: pgid, kill: k.kill}
}

func TestGuardForgetsGroupOnceGone(t *testing.T) {
	k := &fakeKill{err: syscall.ESRCH}
	g := newGuardForTest(4242, k)
	if g.alive() {
		t.Fatal("ESRCH means the group is gone")
	}
	before := k.count()
	// A gone group is never probed or signalled again: its number may belong
	// to somebody else by now.
	g.terminate(false)
	g.terminate(true)
	if g.alive() {
		t.Error("alive after gone")
	}
	g.cleanup()
	if k.count() != before {
		t.Errorf("signals sent to a group already observed gone: %v", k.calls)
	}
}

func TestGuardCleanupIsOneShot(t *testing.T) {
	k := &fakeKill{} // every probe/signal "succeeds": the group never empties
	g := newGuardForTest(4243, k)
	g.cleanup() // escalates SIGTERM then SIGKILL, then gives up
	after := k.count()
	if after == 0 {
		t.Fatal("cleanup sent nothing to a live group")
	}
	for i := 0; i < 3; i++ {
		g.cleanup()
		g.terminate(true)
		g.alive()
	}
	if k.count() != after {
		t.Errorf("signals after cleanup: %d more", k.count()-after)
	}
	if g.pgid != 0 {
		t.Error("process group id retained after cleanup")
	}
}

func TestGuardNoSignalWhenAlreadyEmpty(t *testing.T) {
	k := &fakeKill{err: syscall.ESRCH}
	g := newGuardForTest(4244, k)
	g.cleanup()
	for _, pid := range k.calls {
		if pid != -4244 {
			t.Errorf("unexpected target %d", pid)
		}
	}
	// Only the liveness probe ran, and nothing after it.
	if k.count() != 1 {
		t.Errorf("calls = %v, want a single probe", k.calls)
	}
}

func TestStdioRepeatedCloseDoesNotSignalAgain(t *testing.T) {
	cfg, _ := fakeStdioConfig(t, nil)
	st, err := startStdio("x", cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.close(); err != nil {
		t.Fatal(err)
	}
	k := &fakeKill{}
	st.guard.mu.Lock()
	st.guard.kill = k.kill
	st.guard.mu.Unlock()
	for i := 0; i < 3; i++ {
		if err := st.close(); err != nil {
			t.Fatal(err)
		}
	}
	if k.count() != 0 {
		t.Errorf("late Close signalled a saved process-group id: %v", k.calls)
	}
}

func TestStdioNaturalExitCleansDescendants(t *testing.T) {
	c, _ := connectFake(t, nil)
	pid, err := strconv.Atoi(callText(t, c, "spawn", `{"mode":"sleeper-stubborn"}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killPID(pid) })
	if !processAlive(pid) {
		t.Fatal("descendant not running")
	}
	// The server dies on its own (no Close): the descendant must not be left
	// running, and the transport must have released the group by itself.
	_, _ = c.CallTool(t.Context(), "exit", []byte(`{}`))
	eventually(t, "descendant to be cleaned after the leader exited", func() bool { return !processAlive(pid) })
	st := c.t.(*stdioTransport)
	eventually(t, "group id to be released", func() bool {
		st.guard.mu.Lock()
		defer st.guard.mu.Unlock()
		return st.guard.pgid == 0
	})
}
