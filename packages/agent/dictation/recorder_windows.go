//go:build windows

package dictation

import (
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
func killTree(cmd *exec.Cmd) {
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
	_ = cmd.Process.Kill()
}

// Windows cannot deliver SIGINT to children. ffmpeg overrides can use stdin
// 'q' (do not pass -nostdin); other recorders are killed after the timeout.
func graceful(_ *exec.Cmd, stdin io.Writer, name string) {
	if strings.EqualFold(name, "ffmpeg.exe") || strings.EqualFold(name, "ffmpeg") {
		_, _ = io.WriteString(stdin, "q\n")
	}
}
func expectedStop(error, string) bool { return false }
