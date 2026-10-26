//go:build windows

package dictation

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows cannot deliver SIGINT to children. ffmpeg overrides can use stdin
// 'q' (do not pass -nostdin); other recorders are killed after the timeout.
func graceful(_ *exec.Cmd, stdin io.Writer, name string) {
	if strings.EqualFold(name, "ffmpeg.exe") || strings.EqualFold(name, "ffmpeg") {
		_, _ = io.WriteString(stdin, "q\n")
	}
}
func expectedStop(error, string) bool { return false }

// isolate starts the recorder suspended. The guard assigns it to a Job
// Object before its first instruction runs and then resumes it, so no
// descendant can be created outside the job.
func isolate(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
}

// processGuard owns a Job Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE that
// contains the recorder and all its descendants. The mutex is held across every
// use of the handle so a concurrent release cannot close it mid-call.
type processGuard struct {
	mu   sync.Mutex
	job  windows.Handle // 0 once released
	once sync.Once
}

// newProcessGuard is called right after cmd.Start with the process still
// suspended. It fails closed: if the job cannot be set up, the child is killed
// and an error is returned.
func newProcessGuard(cmd *exec.Cmd) (*processGuard, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, errors.New("process not started")
	}
	fail := func(what string, err error) (*processGuard, error) {
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
		return fail("open recorder process", err)
	}
	err = windows.AssignProcessToJobObject(job, proc)
	_ = windows.CloseHandle(proc)
	if err != nil {
		_ = windows.CloseHandle(job)
		return fail("assign recorder to job object", err)
	}
	if err := resumeProcess(uint32(cmd.Process.Pid)); err != nil {
		_ = windows.CloseHandle(job) // kills the job's only member
		return fail("resume recorder", err)
	}
	return &processGuard{job: job}, nil
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

// terminate kills every process in the job. Windows has no graceful group
// signal, so force is ignored.
func (g *processGuard) terminate(_ bool) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job != 0 {
		_ = windows.TerminateJobObject(g.job, 1)
	}
}

// cleanup kills the job and closes its handle, once.
func (g *processGuard) cleanup() {
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
