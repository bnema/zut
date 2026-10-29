//go:build !windows

package daemonclient

import (
	"errors"
	"os"
	"syscall"
)

// absent reports dial errors meaning nothing listens on the socket.
func absent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}
