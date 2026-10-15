//go:build windows

package mcp

import "os/exec"

func isolateProcess(_ *exec.Cmd) {}

// terminateProcess kills the server process. Windows has no process-group
// signal equivalent here, so both phases kill the direct child.
func terminateProcess(cmd *exec.Cmd, _ bool) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
