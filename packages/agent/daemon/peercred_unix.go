//go:build linux || darwin || freebsd || netbsd || openbsd

package daemon

import (
	"fmt"
	"net"
)

// checkPeerUID returns only the peer UID for rejection diagnostics. Unknown
// credentials are represented by -1; credential lookup errors fail closed.
func checkPeerUID(conn net.Conn, expectedUID int) (int, error) {
	uid := -1
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return uid, fmt.Errorf("not a Unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return uid, err
	}
	var credErr error
	if err := raw.Control(func(fd uintptr) { uid, credErr = socketPeerUID(int(fd)) }); err != nil {
		return uid, err
	}
	if credErr != nil {
		return uid, credErr
	}
	if uid != expectedUID {
		return uid, fmt.Errorf("peer UID mismatch")
	}
	return uid, nil
}

func secureSocketDir(string, bool) error { return nil }
