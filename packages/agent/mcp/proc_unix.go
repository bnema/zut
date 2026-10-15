//go:build !windows

package mcp

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// isolateProcess puts the server in its own process group so it does not
// receive terminal signals aimed at zut and so cleanup can stop its children.
func isolateProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// processGuard owns the process group of one stdio server.
//
// A process-group id may be reused by an unrelated process as soon as no
// process uses the number any more. A group stays in use while ANY member
// exists, and that includes a zombie: the leader's pid (which is the pgid) is
// not freed until the leader is reaped. So the guard keeps the leader
// unreaped, as a zombie, until it has cleaned the group, wherever the platform
// can wait for a child without reaping it (waitLeaderExited; Linux). Only then
// is the leader reaped by cmd.Wait.
//
// Where that is not available (darwin, the BSDs) cleanup runs after the leader
// was reaped; the id is then only guaranteed to be ours while other group
// members live, which is the best that can be done and is documented as a
// residual race. In every case the guard
//
//	(a) cleans the group exactly once,
//	(b) forgets the id as soon as the group is observed empty or cleaned,
//	(c) is cleaned as soon as the direct child exits, so no stale id stays
//	    signalable for the life of the transport.
type processGuard struct {
	mu     sync.Mutex
	pgid   int // 0 once cleaned up or observed gone
	leader int // pid of the group leader (== original pgid)
	proc   *os.Process
	kill   func(pid int, sig syscall.Signal) error // syscall.Kill; replaced in tests
	// zombie is set once the leader has exited but not been reaped. A probe
	// with signal 0 cannot tell an empty group from one whose only member is
	// that zombie, so liveness is then read from the process table.
	zombie bool

	once sync.Once
}

// newProcessGuard is called right after cmd.Start.
func newProcessGuard(cmd *exec.Cmd) (*processGuard, error) {
	g := &processGuard{kill: func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }}
	if cmd != nil && cmd.Process != nil {
		g.pgid = cmd.Process.Pid
		g.leader = cmd.Process.Pid
		g.proc = cmd.Process
	}
	return g, nil
}

// awaitLeaderExit blocks until the group leader has exited, without reaping
// it. It reports false when the platform cannot do that, in which case it
// returns immediately and the caller must Wait first.
func (g *processGuard) awaitLeaderExit() bool {
	if g == nil || g.leader == 0 {
		return false
	}
	if !waitLeaderExited(g.leader) {
		return false
	}
	g.mu.Lock()
	g.zombie = true
	g.mu.Unlock()
	return true
}

// terminate signals the whole group, including descendants that outlive the
// direct child. It does nothing once the group is cleaned up.
func (g *processGuard) terminate(force bool) {
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
func (g *processGuard) alive() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pgid == 0 {
		return false
	}
	if g.zombie {
		if live, known := liveGroupMembers(g.pgid); known {
			if !live {
				g.pgid = 0
			}
			return live
		}
	}
	err := g.kill(-g.pgid, 0)
	if errors.Is(err, syscall.ESRCH) {
		g.pgid = 0
		return false
	}
	return true // nil, or EPERM: a member exists that we cannot signal
}

// cleanup stops every remaining group member (SIGTERM, then SIGKILL for
// survivors) and releases the id. It runs its body once; later and concurrent
// calls wait for that run and then return.
func (g *processGuard) cleanup() {
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
		if !g.alive() {
			return
		}
		g.terminate(false)
		if g.waitGone(300 * time.Millisecond) {
			return
		}
		g.terminate(true)
		g.waitGone(200 * time.Millisecond)
	})
}

func (g *processGuard) waitGone(d time.Duration) bool {
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
