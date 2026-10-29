//go:build windows

package agent

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// daemonAbsent reports dial errors meaning nothing listens on the socket. It
// mirrors daemonclient's unexported classifier.
func daemonAbsent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, windows.WSAECONNREFUSED) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}
