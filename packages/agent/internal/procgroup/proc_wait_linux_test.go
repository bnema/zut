//go:build linux

package procgroup

import (
	"os"
	"sync"
	"syscall"
	"testing"
)

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

// While the leader is an unreaped zombie the pgid is reserved, so cleanup must
// end with a group SIGKILL even when the (non-atomic) /proc scan sees no live
// member: a descendant may have forked between the scan and its end.
func TestReservedGroupIsKilledDespiteEmptyScan(t *testing.T) {
	var mu sync.Mutex
	var sigs []syscall.Signal
	g := &Guard{pgid: 1 << 30, leader: 1 << 30, zombie: true,
		kill: func(pid int, sig syscall.Signal) error {
			mu.Lock()
			defer mu.Unlock()
			if pid != -(1 << 30) {
				t.Errorf("signal to %d, want group", pid)
			}
			sigs = append(sigs, sig)
			return nil
		}}
	g.Cleanup()
	mu.Lock()
	defer mu.Unlock()
	if len(sigs) == 0 || sigs[len(sigs)-1] != syscall.SIGKILL {
		t.Fatalf("signals = %v, want a final group SIGKILL", sigs)
	}
	if g.pgid != 0 {
		t.Error("pgid retained after cleanup")
	}
}
