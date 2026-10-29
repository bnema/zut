//go:build windows

package detach

import (
	"os/exec"
	"syscall"
)

// Windows has no setsid equivalent; these flags give the child a console-less
// process group that Ctrl+C on the parent does not reach.
func configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	}
}
