//go:build !windows

package subagents

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockResidentLease(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrResidentLeaseBusy
	}
	return err
}

func unlockResidentLease(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
