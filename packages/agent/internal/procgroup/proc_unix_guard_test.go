//go:build !windows

package procgroup

import (
	"os/exec"
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

func newGuardForTest(pgid int, k *fakeKill) *Guard {
	return &Guard{pgid: pgid, kill: k.kill}
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
	g.Terminate(false)
	g.Terminate(true)
	if g.alive() {
		t.Error("alive after gone")
	}
	g.Cleanup()
	if k.count() != before {
		t.Errorf("signals sent to a group already observed gone: %v", k.calls)
	}
}

func TestGuardCleanupIsOneShot(t *testing.T) {
	k := &fakeKill{} // every probe/signal "succeeds": the group never empties
	g := newGuardForTest(4243, k)
	g.Cleanup() // escalates SIGTERM then SIGKILL, then gives up
	after := k.count()
	if after == 0 {
		t.Fatal("cleanup sent nothing to a live group")
	}
	for i := 0; i < 3; i++ {
		g.Cleanup()
		g.Terminate(true)
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
	g.Cleanup()
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

// The second non-reaping wait must still observe our child after Cleanup.
// This runs on BSD/macOS as well as Linux, exercising kqueue there.
func TestCleanupReservesLeaderUntilCallerWaits(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	Isolate(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	g, err := New(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !g.AwaitLeaderExit() {
		t.Fatal("non-reaping wait failed")
	}
	g.Cleanup()
	if err := waitLeaderExited(cmd.Process.Pid); err != nil {
		t.Fatalf("leader reaped before caller Wait: %v", err)
	}
	if g.pgid != 0 {
		t.Fatal("guard retained signalable group after cleanup")
	}
}

func TestRepeatedCleanupDoesNotSignalAfterReap(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	Isolate(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g, err := New(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if g.AwaitLeaderExit() {
		g.Cleanup()
	}
	_ = cmd.Wait()
	k := &fakeKill{}
	g.kill = k.kill
	g.Cleanup()
	g.Terminate(true)
	if k.count() != 0 {
		t.Fatal("signaled group after leader was reaped")
	}
}
