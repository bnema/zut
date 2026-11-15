//go:build !linux

package agent

func signalDaemonPID(pid int, _ string) (string, error) {
	return "", unverifiedDaemonPID(pid)
}
