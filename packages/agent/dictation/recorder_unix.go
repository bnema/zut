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

// expectedStop accepts any exit after a requested SIGINT: recorders disagree
// on the status they report (pw-record exits 1, ffmpeg 255, others die from
// the signal). The output file checks decide whether the recording is usable.
func expectedStop(err error, _ string) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit)
}

type processGuard struct{ cmd *exec.Cmd }

func newProcessGuard(cmd *exec.Cmd) (*processGuard, error) { return &processGuard{cmd}, nil }
func (g *processGuard) terminate(_ bool)                   { killTree(g.cmd) }
func (g *processGuard) cleanup()                           { killTree(g.cmd) }
