//go:build netbsd

package daemon

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

func socketPeerUID(fd int) (int, error) {
	// NetBSD LOCAL_PEEREID (3), at level 0, returns struct unpcbid.
	var cred struct {
		PID      int32
		UID, GID uint32
	}
	size := uint32(unsafe.Sizeof(cred))
	_, _, err := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), 0, 3, uintptr(unsafe.Pointer(&cred)), uintptr(unsafe.Pointer(&size)), 0)
	if err != 0 {
		return -1, err
	}
	return int(cred.UID), nil
}
