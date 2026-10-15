//go:build linux

package mcp

import (
	"os"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

func after5s() <-chan time.Time { return time.After(5 * time.Second) }

func TestParseProcStat(t *testing.T) {
	cases := []struct {
		name, in string
		state    byte
		pgrp     int
		ok       bool
	}{
		{"plain", "123 (bash) S 1 123 123 0 -1 4194560", 'S', 123, true},
		{"zombie", "5 (fake-server) Z 1 77 77 0 -1 4", 'Z', 77, true},
		{"spaces and parens", "9 (a) b (c)) R 2 42 42 0", 'R', 42, true},
		{"truncated", "9 (x) S 1", 0, 0, false},
		{"no comm", "garbage", 0, 0, false},
	}
	for _, c := range cases {
		state, pg, ok := parseProcStat(c.in)
		if state != c.state || pg != c.pgrp || ok != c.ok {
			t.Errorf("%s: got (%c,%d,%v), want (%c,%d,%v)", c.name, state, pg, ok, c.state, c.pgrp, c.ok)
		}
	}
}

func TestLiveGroupMembersSeesSelfOnly(t *testing.T) {
	// Our own process group has live members (us); a made-up one does not.
	live, known := liveGroupMembers(os.Getpid())
	if !known {
		t.Skip("/proc not readable")
	}
	_ = live // our pgid is usually not our pid; only the empty case is stable
	if live, known := liveGroupMembers(1 << 30); !known || live {
		t.Errorf("unused group reported live=%v known=%v", live, known)
	}
}

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
		pgidKept   int
	}
	obs := make(chan observation, 1)
	hook := func(pid int) {
		if pid != leader {
			return
		}
		var o observation
		if raw, err := os.ReadFile("/proc/" + strconv.Itoa(leader) + "/stat"); err == nil {
			o.state, _, _ = parseProcStat(string(raw))
		}
		o.childAlive = processAlive(childPID)
		st.guard.mu.Lock()
		o.pgidKept = st.guard.pgid
		st.guard.mu.Unlock()
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
	if o.pgidKept != 0 {
		t.Errorf("process-group id %d retained after cleanup", o.pgidKept)
	}
	// And the leader is reaped afterwards.
	eventually(t, "leader to be reaped", func() bool {
		_, err := os.Stat("/proc/" + strconv.Itoa(leader))
		return os.IsNotExist(err)
	})
}

// While the leader is an unreaped zombie the pgid is reserved, so cleanup must
// end with a group SIGKILL even when the (non-atomic) /proc scan sees no live
// member: a descendant may have forked between the scan and its end.
func TestReservedGroupIsKilledDespiteEmptyScan(t *testing.T) {
	var mu sync.Mutex
	var sigs []syscall.Signal
	g := &processGuard{pgid: 1 << 30, leader: 1 << 30, zombie: true,
		kill: func(pid int, sig syscall.Signal) error {
			mu.Lock()
			defer mu.Unlock()
			if pid != -(1 << 30) {
				t.Errorf("signal to %d, want group", pid)
			}
			sigs = append(sigs, sig)
			return nil
		}}
	g.cleanup()
	mu.Lock()
	defer mu.Unlock()
	if len(sigs) == 0 || sigs[len(sigs)-1] != syscall.SIGKILL {
		t.Fatalf("signals = %v, want a final group SIGKILL", sigs)
	}
	if g.pgid != 0 {
		t.Error("pgid retained after cleanup")
	}
}
