package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

func signalDaemonPID(pid int, lockPath string) (string, error) {
	// Acquire the stable handle before examining /proc. Never fall back to kill(2).
	p, err := os.FindProcess(pid)
	if err != nil {
		return "", unverifiedDaemonPID(pid)
	}
	defer p.Release()
	if err := p.WithHandle(func(uintptr) {}); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return "", err
		}
		// FindProcess represents an already absent PID as a done process,
		// which may have no handle. No signal is needed to confirm absence.
		if _, statErr := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); errors.Is(statErr, os.ErrNotExist) {
			return "", os.ErrProcessDone
		}
		return "", unverifiedDaemonPID(pid)
	}
	if !processHasDaemonLock(pid, lockPath) {
		// Preserve an exit race for the caller's lock recheck, without signaling.
		if err := p.Signal(syscall.Signal(0)); errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return "", err
		}
		return "", unverifiedDaemonPID(pid)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return "", fmt.Errorf("stop daemon pid %d with SIGTERM: %w", pid, err)
	}
	return "SIGTERM", nil
}

func processHasDaemonLock(pid int, lockPath string) bool {
	resolved, err := filepath.EvalSymlinks(lockPath)
	if err != nil {
		return false
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return false
	}
	lockInfo, err := os.Stat(resolved)
	if err != nil {
		return false
	}
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		fd := filepath.Join(dir, entry.Name())
		target, err := os.Readlink(fd)
		if err != nil || target != resolved {
			continue
		}
		info, err := os.Stat(fd)
		if err == nil && os.SameFile(lockInfo, info) {
			return true
		}
	}
	return false
}
