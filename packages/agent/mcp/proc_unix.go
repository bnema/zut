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
// A numeric process-group id is only safe to signal while the group still has
// members: once the last member is gone the kernel may hand the number to an
// unrelated process. The guard therefore (a) cleans the group exactly once,
// (b) forgets the id as soon as the group is observed empty or cleaned, and
// (c) is cleaned as soon as the direct child is reaped, so no stale id stays
// signalable for the life of the transport.
type processGuard struct {
	mu   sync.Mutex
	pgid int // 0 once cleaned up or observed gone
	proc *os.Process
	kill func(pid int, sig syscall.Signal) error // syscall.Kill; replaced in tests

	once sync.Once
}

// newProcessGuard is called right after cmd.Start.
func newProcessGuard(cmd *exec.Cmd) (*processGuard, error) {
	g := &processGuard{kill: func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }}
	if cmd != nil && cmd.Process != nil {
		g.pgid = cmd.Process.Pid
		g.proc = cmd.Process
	}
	return g, nil
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
