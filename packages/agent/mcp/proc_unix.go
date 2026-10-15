//go:build !windows

package mcp

import (
	"os/exec"
	"syscall"
)

// isolateProcess puts the server in its own process group so it does not
// receive terminal signals aimed at zut and so close can stop its children.
func isolateProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// processGuard tracks everything a stdio server spawned: the process group on
// Unix, a Job Object on Windows.
type processGuard struct {
	cmd *exec.Cmd
}

// newProcessGuard is called right after cmd.Start.
func newProcessGuard(cmd *exec.Cmd) *processGuard { return &processGuard{cmd: cmd} }

// terminate signals the whole process group, including descendants that
// outlive the direct child.
func (g *processGuard) terminate(force bool) {
	if g == nil || g.cmd == nil || g.cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	err := syscall.Kill(-g.cmd.Process.Pid, sig)
	if err != nil && err != syscall.ESRCH {
		_ = g.cmd.Process.Signal(sig)
	}
}

// alive reports whether any member of the process group still exists.
func (g *processGuard) alive() bool {
	if g == nil || g.cmd == nil || g.cmd.Process == nil {
		return false
	}
	err := syscall.Kill(-g.cmd.Process.Pid, 0)
	return err == nil || err == syscall.EPERM
}

// release frees resources held for the group. Nothing is held on Unix.
func (g *processGuard) release() {}
