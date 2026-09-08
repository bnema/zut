//go:build windows

package tools

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// pythonProcessWaitDelay bounds process cleanup beyond the invocation or
// probe deadline. Bounded return is required; absolute containment of every
// detached descendant is not promised.
const pythonProcessWaitDelay = 5 * time.Second

// pythonTreeKillTimeout bounds the tree-termination subprocess. On
// failure or timeout the caller falls back to a direct process kill.
const pythonTreeKillTimeout = 2 * time.Second

// maxPythonKillOutput bounds termination-command output.
const maxPythonKillOutput = 4 * 1024

// configurePythonProcess makes context cancellation terminate the Python
// process tree on Windows and closes the output pipe exactly once. Unlike
// Bash's unbounded taskkill path, tree termination is bounded with a
// direct-kill fallback.
func configurePythonProcess(cmd *exec.Cmd, output io.Closer) func() {
	if cmd != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
		}
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
			return killPythonProcessTree(cmd)
		}
	}
	return closePipe
}

// killPythonProcessTree terminates the process tree with a bounded
// taskkill call, then falls back to a direct kill on failure or timeout.
func killPythonProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := boundedTaskkill(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	return nil
}

func boundedTaskkill(pid int) error {
	if pid <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), pythonTreeKillTimeout)
	defer cancel()
	killCmd := exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	var out bytes.Buffer
	killCmd.Stdout = &limitedWriter{W: &out, N: maxPythonKillOutput}
	killCmd.Stderr = &limitedWriter{W: &out, N: maxPythonKillOutput}
	if err := killCmd.Run(); err != nil {
		return err
	}
	return ctx.Err()
}
