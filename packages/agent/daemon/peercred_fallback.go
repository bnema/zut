//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !windows

package daemon

import (
	"fmt"
	"net"
	"os"
)

func checkPeerUID(net.Conn, int) (int, error) { return -1, fmt.Errorf("peer credentials unsupported") }
func mkdirPrivate(dir string) error           { return os.Mkdir(dir, 0o700) }

func secureSocketDir(string, bool) error {
	return fmt.Errorf("daemon socket protection unsupported on this platform")
}
