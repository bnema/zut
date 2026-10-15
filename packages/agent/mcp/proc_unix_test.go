//go:build !windows

package mcp

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// processAlive reports whether pid still runs. A zombie that nobody reaped
// (common when the test runs as PID 1's child in a container) counts as dead.
func processAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	if raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		// Format: pid (comm) S ...; comm may contain spaces and parens.
		if i := strings.LastIndexByte(string(raw), ')'); i >= 0 && i+2 < len(raw) && raw[i+2] == 'Z' {
			return false
		}
	}
	return true
}

const canCheckProcess = true

// killPID is a best-effort cleanup for processes a test started.
func killPID(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }
