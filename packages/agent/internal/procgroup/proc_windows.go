//go:build windows

package procgroup

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Isolate starts the server suspended. The guard assigns it to a Job
// Object before its first instruction runs and then resumes it, so no
// descendant can be created outside the job.
func Isolate(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
}

// Guard owns a Job Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE that
// contains the server and all its descendants. The mutex is held across every
// use of the handle so a concurrent release cannot close it mid-call.
type Guard struct {
	mu   sync.Mutex
	job  windows.Handle // 0 once released
	once sync.Once
}

// New is called right after cmd.Start with the process still
// suspended. It fails closed: if the job cannot be set up, the child is killed
// and an error is returned.
func New(cmd *exec.Cmd) (*Guard, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, errors.New("process not started")
	}
	fail := func(what string, err error) (*Guard, error) {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fail("create job object", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return fail("configure job object", err)
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return fail("open server process", err)
	}
	err = windows.AssignProcessToJobObject(job, proc)
	_ = windows.CloseHandle(proc)
	if err != nil {
		_ = windows.CloseHandle(job)
		return fail("assign server to job object", err)
	}
	if err := resumeProcess(uint32(cmd.Process.Pid)); err != nil {
		_ = windows.CloseHandle(job) // kills the job's only member
		return fail("resume server", err)
	}
	return &Guard{job: job}, nil
}

// resumeProcess resumes every suspended thread of pid (a CREATE_SUSPENDED
// process has exactly its main thread).
func resumeProcess(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snap, &entry); err != nil {
		return err
	}
	resumed := 0
	for {
		if entry.OwnerProcessID == pid {
			th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return err
			}
			_, err = windows.ResumeThread(th)
			_ = windows.CloseHandle(th)
			if err != nil {
				return err
			}
			resumed++
		}
		entry.Size = uint32(unsafe.Sizeof(entry))
		if err := windows.Thread32Next(snap, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return err
		}
	}
	if resumed == 0 {
		return errors.New("no thread to resume")
	}
	return nil
}

// AwaitLeaderExit is not needed on Windows: the Job Object, not a reusable
// numeric id, identifies the group. It reports false so the caller waits for
// the process first.
func (g *Guard) AwaitLeaderExit() bool { return false }

// Terminate kills every process in the job. Windows has no graceful group
// signal, so force is ignored.
func (g *Guard) Terminate(_ bool) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job != 0 {
		_ = windows.TerminateJobObject(g.job, 1)
	}
}

// Cleanup kills the job and closes its handle, once.
func (g *Guard) Cleanup() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.job != 0 {
			_ = windows.TerminateJobObject(g.job, 1)
			_ = windows.CloseHandle(g.job)
			g.job = 0
		}
	})
}
