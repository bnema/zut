//go:build !windows

package agent

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

const testProcessCheckable = true

// testProcessAlive reports whether pid runs; an unreaped zombie counts as dead.
func testProcessAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	if raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		if i := strings.LastIndexByte(string(raw), ')'); i >= 0 && i+2 < len(raw) && raw[i+2] == 'Z' {
			return false
		}
	}
	return true
}
