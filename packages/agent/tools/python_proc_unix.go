//go:build !windows

package tools

import (
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// pythonProcessWaitDelay bounds process cleanup beyond the invocation or
// probe deadline. Bounded return is required; absolute containment of every
// detached descendant is not promised.
const pythonProcessWaitDelay = 5 * time.Second

// configurePythonProcess makes context cancellation terminate the Python
// process group and closes the output pipe exactly once. It mirrors the
// Bash Unix mechanics without touching Bash behavior. When cancelErr is
// non-nil, the termination error from the Cancel callback is recorded
// there (guarded by its mutex): Wait folds that error into its own
// process error, so without this capture the cleanup detail is
// unreachable and a partial kill cannot report surviving descendants.
func configurePythonProcess(cmd *exec.Cmd, output io.Closer, cancelErr *pythonCancelError) func() {
	if cmd != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.WaitDelay = pythonProcessWaitDelay
	}
	var closeOutput sync.Once
	closePipe := func() {
		closeOutput.Do(func() {
			if output != nil {
				_ = output.Close()
			}
		})
	}
	if cmd != nil {
		cmd.Cancel = func() error {
			closePipe()
			err := killPythonProcessGroup(cmd)
			if cancelErr != nil {
				cancelErr.set(err)
			}
			return err
		}
	}
	return closePipe
}

// killPythonProcessGroup forcefully terminates the process group so
// descendants cannot retain the output pipe indefinitely.
func killPythonProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid := cmd.Process.Pid
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err == syscall.ESRCH {
		return nil
	} else {
		return err
	}
}
