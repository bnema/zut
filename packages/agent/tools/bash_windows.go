//go:build windows

package tools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const bashProcessWaitDelay = 5 * time.Second

// configureBashProcess makes CommandContext cancellation terminate the
// shell and its descendants on Windows. Windows does not provide a Unix-like
// process-group kill operation, so killProcessGroup uses taskkill's tree mode.
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
		return killBashProcessTree(cmd)
	}
	return closePipe
}

// setProcessGroup gives the shell its own Windows process group. The group
// flag prevents it from being attached to the supervisor's console group;
// taskkill /T below is still needed because Windows group creation alone does
// not terminate descendants.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

// killBashProcessTree terminates the shell process tree. taskkill is part of
// Windows and /T follows the parent-child relationships to include processes
// spawned by the shell.
func killBashProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := forceKillProcess(cmd.Process.Pid)
	var taskkillErr *taskkillError
	if errors.As(err, &taskkillErr) && taskkillErr.processDone() {
		return os.ErrProcessDone
	}
	return err
}

// killProcessGroup preserves the process-tree cancellation helper used by
// other tools while the Bash path uses the error-returning Cancel hook above.
func killProcessGroup(cmd *exec.Cmd) {
	_ = killBashProcessTree(cmd)
}

func forceKillProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	output, err := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").CombinedOutput()
	if err == nil {
		return nil
	}
	return &taskkillError{pid: pid, err: err, diagnostics: string(output)}
}

type taskkillError struct {
	pid         int
	err         error
	diagnostics string
}

func (e *taskkillError) Error() string {
	if diagnostics := strings.TrimSpace(e.diagnostics); diagnostics != "" {
		return fmt.Sprintf("taskkill process tree for PID %d: %v: %s", e.pid, e.err, diagnostics)
	}
	return fmt.Sprintf("taskkill process tree for PID %d: %v", e.pid, e.err)
}

func (e *taskkillError) Unwrap() error { return e.err }

// taskkill returns exit code 128 when the requested process no longer exists.
// Other taskkill failures remain errors so callers can distinguish a
// cancellation race from a real process-control failure.
func (e *taskkillError) processDone() bool {
	var exitErr *exec.ExitError
	return errors.As(e.err, &exitErr) && exitErr.ExitCode() == 128
}
