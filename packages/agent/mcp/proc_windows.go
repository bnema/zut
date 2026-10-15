//go:build windows

package mcp

import (
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

func isolateProcess(_ *exec.Cmd) {}

// processGuard assigns the server to a Job Object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so descendants die with the server (and
// with zut, should zut itself be killed).
//
// Assignment happens right after Start; a server that spawns children in the
// first instants of its life can escape the job. If the job cannot be created
// or assigned, the guard falls back to killing the direct child.
type processGuard struct {
	cmd *exec.Cmd

	mu  sync.Mutex
	job windows.Handle
}

func newProcessGuard(cmd *exec.Cmd) *processGuard {
	g := &processGuard{cmd: cmd}
	if cmd == nil || cmd.Process == nil {
		return g
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return g
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return g
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return g
	}
	defer windows.CloseHandle(proc)
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.CloseHandle(job)
		return g
	}
	g.job = job
	return g
}

// terminate kills the job (every process in it) or, without a job, the direct
// child. Windows has no graceful group signal, so force is ignored.
func (g *processGuard) terminate(_ bool) {
	if g == nil {
		return
	}
	g.mu.Lock()
	job := g.job
	g.mu.Unlock()
	if job != 0 {
		_ = windows.TerminateJobObject(job, 1)
		return
	}
	if g.cmd != nil && g.cmd.Process != nil {
		_ = g.cmd.Process.Kill()
	}
}

// alive is always false: terminate is already forceful.
func (g *processGuard) alive() bool { return false }

// release closes the job handle, which kills any survivor.
func (g *processGuard) release() {
	if g == nil {
		return
	}
	g.mu.Lock()
	job := g.job
	g.job = 0
	g.mu.Unlock()
	if job != 0 {
		_ = windows.CloseHandle(job)
	}
}
