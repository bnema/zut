//go:build linux

package mcp

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// pidsWithEnv lists processes whose environment contains entry.
func pidsWithEnv(entry string) []int {
	dirs, _ := os.ReadDir("/proc")
	var pids []int
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + d.Name() + "/environ")
		if err != nil {
			continue
		}
		for _, kv := range bytes.Split(raw, []byte{0}) {
			if string(kv) == entry {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}

// Close must not return while a start it aborted still has a live process.
func TestPoolCloseReapsInFlightStart(t *testing.T) { testPoolCloseReapsInFlightStart(t, time.Minute) }

// With a zero idle delay the cancelled caller's release retires the entry
// while its start is still in flight; Close must wait for that start too.
func TestPoolCloseReapsRetiredInFlightStart(t *testing.T) { testPoolCloseReapsInFlightStart(t, 0) }

func testPoolCloseReapsInFlightStart(t *testing.T, idle time.Duration) {
	gate := filepath.Join(t.TempDir(), "release")
	cfg, events := fakeStdioConfig(t, map[string]string{"FAKE_INIT_GATE": gate})
	cfg.StartupTimeoutSec = 30
	pool := NewPool(idle, nil)
	t.Cleanup(func() { _ = pool.Close() })
	m := NewManager(Config{Servers: map[string]ServerConfig{"fake": cfg}}, t.TempDir(), ClientInfo{}, nil)
	m.SetPool(pool)
	t.Cleanup(func() { _ = m.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _, _ = m.Start(ctx); close(done) }()
	// The server is a real subprocess: its readiness is only observable
	// through the event file it writes.
	waitEvent(t, events, "init-held")
	entry := "FAKE_INIT_GATE=" + gate
	pids := pidsWithEnv(entry)
	if len(pids) == 0 {
		t.Fatal("held server process not found")
	}
	t.Cleanup(func() {
		// Re-match so a reaped pid reused by another process is never hit.
		for _, pid := range pidsWithEnv(entry) {
			killPID(pid)
		}
	})
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled caller did not return")
	}
	if idle == 0 {
		// Releasing the lease retires the entry with its start in flight.
		_ = m.Close()
		pool.mu.Lock()
		n := len(pool.entries)
		pool.mu.Unlock()
		if n != 0 {
			t.Fatalf("entries = %d after release, want the entry retired", n)
		}
	}
	_ = pool.Close()
	for _, pid := range pids {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err == nil {
			t.Errorf("server pid %d still exists (alive or unreaped) after Pool.Close returned", pid)
		}
	}
}
