//go:build linux

package mcp

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func after5s() <-chan time.Time { return time.After(5 * time.Second) }

// ---- GO-020: group cleanup happens while the leader is still a zombie ----

func TestStdioGroupCleanedBeforeLeaderIsReaped(t *testing.T) {
	c, _ := connectFake(t, nil)
	st := c.t.(*stdioTransport)
	leader := st.cmd.Process.Pid

	childPID, err := strconv.Atoi(callText(t, c, "spawn", `{"mode":"sleeper-stubborn"}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killPID(childPID) })
	if !processAlive(childPID) {
		t.Fatal("descendant not running")
	}

	type observation struct {
		state      byte
		childAlive bool
	}
	obs := make(chan observation, 1)
	hook := func(pid int) {
		if pid != leader {
			return
		}
		var o observation
		if raw, err := os.ReadFile("/proc/" + strconv.Itoa(leader) + "/stat"); err == nil {
			if strings.Contains(string(raw), ") Z ") {
				o.state = 'Z'
			}
		}
		o.childAlive = processAlive(childPID)
		select {
		case obs <- o:
		default:
		}
	}
	beforeReapHook.Store(&hook)
	t.Cleanup(func() { beforeReapHook.Store(nil) })

	// The server exits on its own.
	_, _ = c.CallTool(t.Context(), "exit", []byte(`{}`))
	var o observation
	select {
	case o = <-obs:
	case <-after5s():
		t.Fatal("cleanup hook never ran: the leader was reaped before group cleanup")
	}
	if o.state != 'Z' {
		t.Errorf("leader state at cleanup = %q, want Z (exited but not reaped, so its pid/pgid cannot be reused)", o.state)
	}
	if o.childAlive {
		t.Error("descendant still alive when the leader was about to be reaped")
	}
	// And the leader is reaped afterwards.
	eventually(t, "leader to be reaped", func() bool {
		_, err := os.Stat("/proc/" + strconv.Itoa(leader))
		return os.IsNotExist(err)
	})
}
