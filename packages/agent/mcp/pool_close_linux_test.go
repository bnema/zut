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
func TestPoolCloseReapsInFlightStart(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "release")
	cfg, events := fakeStdioConfig(t, map[string]string{"FAKE_INIT_GATE": gate})
	cfg.StartupTimeoutSec = 30
	pool := NewPool(time.Minute, nil)
	t.Cleanup(func() { _ = pool.Close() })
	m := NewManager(Config{Servers: map[string]ServerConfig{"fake": cfg}}, t.TempDir(), ClientInfo{}, nil)
	m.SetPool(pool)
	t.Cleanup(func() { _ = m.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _, _ = m.Start(ctx); close(done) }()
	waitEvent(t, events, "init-held")
	pids := pidsWithEnv("FAKE_INIT_GATE=" + gate)
	if len(pids) == 0 {
		t.Fatal("held server process not found")
	}
	t.Cleanup(func() {
		for _, pid := range pids {
			killPID(pid)
		}
	})
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled caller did not return")
	}
	_ = pool.Close()
	for _, pid := range pids {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err == nil {
			t.Errorf("server pid %d still exists (alive or unreaped) after Pool.Close returned", pid)
		}
	}
}
