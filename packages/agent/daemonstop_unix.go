//go:build !windows

package agent

import (
	"fmt"
	"os"
	"syscall"
)

func signalDaemonPID(pid int) (string, error) {
	p, err := os.FindProcess(pid)
	if err == nil {
		err = p.Signal(syscall.SIGTERM)
	}
	if err != nil {
		return "", fmt.Errorf("stop daemon pid %d with SIGTERM: %w", pid, err)
	}
	return "SIGTERM", nil
}
