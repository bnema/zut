//go:build !windows

package procgroup

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Isolate puts the server in its own process group so it does not
// receive terminal signals aimed at zut and so Cleanup can stop its children.
func Isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Guard owns the process group of one stdio server.
//
// A process-group id may be reused by an unrelated process as soon as no
// process uses the number any more. A group stays in use while ANY member
// exists, and that includes a zombie: the leader's pid (which is the pgid) is
// not freed until the leader is reaped. So the guard keeps the leader
// unreaped until group signaling finishes. AwaitLeaderExit must run before
// cmd.Wait; even if its non-reaping wait fails, it cleans or disarms the guard
// before returning, so no saved PGID is signalable after the leader is reaped.
type Guard struct {
	mu     sync.Mutex
	pgid   int // 0 once cleaned up or observed gone
	leader int // pid of the group leader (== original pgid)
	proc   *os.Process
	kill   func(pid int, sig syscall.Signal) error // syscall.Kill; replaced in tests
	// zombie is set once the leader has exited but not been reaped. The pgid
	// is then reserved, so Cleanup always ends with a group SIGKILL; the
	// process table is read only to end the SIGTERM grace period early.
	zombie bool

	once sync.Once
}

// New is called right after cmd.Start.
func New(cmd *exec.Cmd) (*Guard, error) {
	g := &Guard{kill: func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }}
	if cmd != nil && cmd.Process != nil {
		g.pgid = cmd.Process.Pid
		g.leader = cmd.Process.Pid
		g.proc = cmd.Process
	}
	return g, nil
}

// AwaitLeaderExit blocks until the group leader has exited, without reaping
// it. The caller must Cleanup before cmd.Wait when it returns true. On wait
// failure it finishes cleanup (or disarms on ECHILD) before returning false.
func (g *Guard) AwaitLeaderExit() bool {
	if g == nil || g.leader == 0 {
		return false
	}
	if err := waitLeaderExited(g.leader); err != nil {
		if errors.Is(err, syscall.ECHILD) {
			// Somebody else already reaped the leader: never signal its PGID.
			g.mu.Lock()
			g.pgid = 0
			g.mu.Unlock()
		}
		g.Cleanup() // still before the caller is allowed to reap
		return false
	}
	g.mu.Lock()
	g.zombie = true
	g.mu.Unlock()
	return true
}

// Terminate signals the whole group, including descendants that outlive the
// direct child. It does nothing once the group is cleaned up.
func (g *Guard) Terminate(force bool) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pgid == 0 {
		return
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	err := g.kill(-g.pgid, sig)
	switch {
	case err == nil:
	case errors.Is(err, syscall.ESRCH):
		g.pgid = 0 // group gone: never signal this number again
	default:
		if g.proc != nil {
			_ = g.proc.Signal(sig)
		}
	}
}

// alive reports whether the group still has members.
func (g *Guard) alive() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pgid == 0 {
		return false
	}
	if g.zombie {
		// A signal-0 probe would see the zombie leader. The /proc scan is
		// advisory only (not an atomic snapshot), so it never clears pgid.
		live, known := liveGroupMembers(g.pgid)
		return live || !known
	}
	err := g.kill(-g.pgid, 0)
	if errors.Is(err, syscall.ESRCH) {
		g.pgid = 0
		return false
	}
	return true // nil, or EPERM: a member exists that we cannot signal
}

// Cleanup stops every remaining group member (SIGTERM, then SIGKILL for
// survivors) and releases the id. It runs its body once; later and concurrent
// calls wait for that run and then return.
func (g *Guard) Cleanup() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		defer func() {
			g.mu.Lock()
			g.pgid = 0
			g.proc = nil
			g.mu.Unlock()
		}()
		g.mu.Lock()
		reserved := g.zombie
		g.mu.Unlock()
		if reserved {
			// The unreaped leader keeps the pgid ours: give members a grace
			// period, then kill the group unconditionally. The kernel delivers
			// a group signal to every member, including ones forked meanwhile.
			g.Terminate(false)
			g.waitGone(300 * time.Millisecond)
			g.Terminate(true)
			return
		}
		if !g.alive() {
			return
		}
		g.Terminate(false)
		if g.waitGone(300 * time.Millisecond) {
			return
		}
		g.Terminate(true)
		g.waitGone(200 * time.Millisecond)
	})
}

func (g *Guard) waitGone(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if !g.alive() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
