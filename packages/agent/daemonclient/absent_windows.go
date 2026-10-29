//go:build windows

package daemonclient

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// absent reports dial errors meaning nothing listens on the socket. Windows
// AF_UNIX reports a missing socket as ERROR_FILE_NOT_FOUND/ERROR_PATH_NOT_FOUND
// and no listener as WSAECONNREFUSED (10061).
func absent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, windows.WSAECONNREFUSED) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}
