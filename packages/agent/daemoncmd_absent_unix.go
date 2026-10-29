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
