//go:build windows

package mcp

func processAlive(pid int) bool { return false }

const canCheckProcess = false

func killPID(pid int) {}
