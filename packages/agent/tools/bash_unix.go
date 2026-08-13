//go:build !windows

package tools

import (
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const bashProcessWaitDelay = 5 * time.Second

// configureBashProcess makes context cancellation terminate the shell's
// process group and closes the output pipe exactly once. CommandContext calls
// cmd.Cancel from its context watcher, so no second cancellation watcher is
// needed here.
func configureBashProcess(cmd *exec.Cmd, output io.Closer) func() {
	setProcessGroup(cmd)
	cmd.WaitDelay = bashProcessWaitDelay
	var closeOutput sync.Once
	closePipe := func() {
		closeOutput.Do(func() {
			if output != nil {
				_ = output.Close()
			}
		})
	}
	cmd.Cancel = func() error {
		closePipe()
		return killBashProcessGroup(cmd)
	}
	return closePipe
}

// setProcessGroup puts the command in its own process group so
// killProcessGroup can target the entire tree including background
// children spawned with &.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killBashProcessGroup forcefully terminates the entire process group so
// backgrounded children (cmd &) cannot survive cancellation or retain an
// output pipe.
func killBashProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid := cmd.Process.Pid
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

// killProcessGroup sends SIGTERM then SIGKILL to the entire process group.
// It is also used by the create_worktree helper, which keeps its existing
// graceful cancellation behavior.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.AfterFunc(3*time.Second, func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	})
}
