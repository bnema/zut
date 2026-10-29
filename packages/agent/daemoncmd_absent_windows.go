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

// pidAlive reports whether pid names a live process: it opens the process
// with limited query rights and checks it has not exited. Any failure to
// open is treated as not alive (unknown).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
