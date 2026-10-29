//go:build !windows

package agent

import (
	"errors"
	"os"
	"syscall"
)

// daemonAbsent reports dial errors meaning nothing listens on the socket. It
// mirrors daemonclient's unexported classifier.
func daemonAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

// pidAlive reports whether pid names a live process (signal 0 probe).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	// EPERM: the process exists but belongs to someone else.
	return err == nil || errors.Is(err, syscall.EPERM)
}
