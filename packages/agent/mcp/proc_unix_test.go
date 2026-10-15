//go:build !windows

package mcp

import "syscall"

// processAlive reports whether pid still exists.
func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

const canCheckProcess = true
