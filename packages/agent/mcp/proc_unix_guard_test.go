//go:build !windows

package mcp

import (
	"strconv"
	"testing"
)

func TestStdioNaturalExitCleansDescendants(t *testing.T) {
	c, _ := connectFake(t, nil)
	pid, err := strconv.Atoi(callText(t, c, "spawn", `{"mode":"sleeper-stubborn"}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killPID(pid) })
	if !processAlive(pid) {
		t.Fatal("descendant not running")
	}
	// The server dies on its own (no Close): the descendant must not be left
	// running, and the transport must have released the group by itself.
	_, _ = c.CallTool(t.Context(), "exit", []byte(`{}`))
	eventually(t, "descendant to be cleaned after the leader exited", func() bool { return !processAlive(pid) })
}
