// Package detach configures child processes to outlive the spawning terminal.
package detach

import "os/exec"

// Configure makes cmd run detached from the parent's terminal so tty signals
// sent to the parent (SIGINT when the terminal closes, SIGHUP on logout) do not
// reach the child. Unix uses a new session (setsid); Windows uses
// DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP. It replaces cmd.SysProcAttr.
func Configure(cmd *exec.Cmd) { configure(cmd) }
