//go:build !windows

package dictation

import (
	"errors"
	"io"
	"os/exec"
	"syscall"
)

func isolate(cmd *exec.Cmd)  { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func killTree(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
func graceful(cmd *exec.Cmd, _ io.Writer, _ string) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
}
func expectedStop(err error, name string) bool {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		status, ok := exit.Sys().(syscall.WaitStatus)
		return (ok && status.Signaled() && status.Signal() == syscall.SIGINT) || (name == "ffmpeg" && exit.ExitCode() == 255)
	}
	return false
}
