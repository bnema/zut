package agent

import "fmt"

func signalDaemonPID(pid int) (string, error) {
	return "", fmt.Errorf("daemon protocol mismatch: stop daemon process pid %d manually, then retry", pid)
}
