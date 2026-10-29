//go:build unix

package daemon

import (
	"os"
	"syscall"
)

// fileOwner returns the owner uid of fi.
func fileOwner(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
