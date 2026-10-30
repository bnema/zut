//go:build openbsd

package daemon

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

func socketPeerUID(fd int) (int, error) {
	// OpenBSD SO_PEERCRED returns struct sockpeercred.
	var cred struct {
		UID, GID uint32
		PID      int32
	}
	size := uint32(unsafe.Sizeof(cred))
	_, _, err := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), unix.SOL_SOCKET, unix.SO_PEERCRED, uintptr(unsafe.Pointer(&cred)), uintptr(unsafe.Pointer(&size)), 0)
	if err != 0 {
		return -1, err
	}
	return int(cred.UID), nil
}
