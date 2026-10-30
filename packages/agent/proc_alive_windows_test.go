//go:build windows

package agent

const testProcessCheckable = false

func testProcessAlive(int) bool { return false }
