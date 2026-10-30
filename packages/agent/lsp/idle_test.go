package lsp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func idleTestManager(t *testing.T, seconds int, hang bool) (*Manager, string, <-chan string) {
	t.Helper()
	t.Setenv("ZUT_HOME", t.TempDir())
	root := t.TempDir()
	config := `{"autoDetect":false,"servers":{"fake":{"command":` + strconv.Quote(os.Args[0]) + `,"args":["-test.run=^TestFakeLSPProcess$"],"idleTimeoutSeconds":` + strconv.Itoa(seconds) + `,"env":{"ZUT_FAKE_LSP":"1","ZUT_FAKE_LSP_HANG_INITIALIZE":` + strconv.Quote(map[bool]string{true: "1", false: "0"}[hang]) + `}}}}`
	if err := os.WriteFile(filepath.Join(root, "lsp.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	logs := make(chan string, 10)
	m := NewManagerWithOptions(ManagerOptions{OnLog: func(_, message string) { logs <- message }})
	t.Cleanup(func() { _ = m.Close() })
	return m, root, logs
}

func awaitIdleLog(t *testing.T, logs <-chan string, want string) {
	t.Helper()
	select {
	case got := <-logs:
		if got != want {
			t.Fatalf("log = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not reach synchronization point")
	}
}

func TestIdleServerClosesAndRestarts(t *testing.T) {
	for _, seconds := range []int{0, 2, -1} {
		t.Run(strconv.Itoa(seconds), func(t *testing.T) {
			m, root, _ := idleTestManager(t, seconds, false)
			if _, err := m.Request(context.Background(), root, "fake", "test/echo", nil); err != nil {
				t.Fatal(err)
			}
			ws, _ := m.workspace(root)
			m.mu.Lock()
			client := ws.clients["fake"]
			last := ws.usage["fake"].lastUsed
			ws.opened["fake"] = map[string]string{"old": "hash"}
			m.mu.Unlock()
			timeout := time.Duration(seconds) * time.Second
			if seconds <= 0 {
				timeout = 5 * time.Minute
			}
			m.sweepIdle(last.Add(timeout - time.Nanosecond))
			m.mu.Lock()
			same := ws.clients["fake"] == client
			m.mu.Unlock()
			if !same {
				t.Fatal("server expired before deadline")
			}
			m.sweepIdle(last.Add(timeout))
			if seconds < 0 {
				m.mu.Lock()
				same = ws.clients["fake"] == client
				m.mu.Unlock()
				if !same {
					t.Fatal("disabled expiry stopped server")
				}
				return
			}
			select {
			case <-client.done:
			default:
				t.Fatal("idle client not closed")
			}
			m.mu.Lock()
			stale := ws.opened["fake"] != nil
			m.mu.Unlock()
			if stale {
				t.Fatal("document state survived eviction")
			}
			if _, err := m.Request(context.Background(), root, "fake", "test/echo", nil); err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			restarted := ws.clients["fake"] != nil && ws.clients["fake"] != client
			m.mu.Unlock()
			if !restarted {
				t.Fatal("next request did not restart server")
			}
		})
	}
}

func TestIdleSweepProtectsInflightRequest(t *testing.T) {
	m, root, logs := idleTestManager(t, 1, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := m.Request(ctx, root, "fake", "test/block", nil); result <- err }()
	awaitIdleLog(t, logs, "blocked")
	ws, _ := m.workspace(root)
	m.sweepIdle(time.Now().Add(time.Hour))
	if _, err := m.Request(context.Background(), root, "fake", "test/echo", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		t.Fatalf("in-flight request stopped: %v", err)
	default:
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("request error = %v", err)
	}
	m.mu.Lock()
	client := ws.clients["fake"]
	last := ws.usage["fake"].lastUsed
	m.mu.Unlock()
	m.sweepIdle(last.Add(time.Second))
	select {
	case <-client.done:
	default:
		t.Fatal("released client not evicted")
	}
}

func TestIdleSweepProtectsStartingServer(t *testing.T) {
	m, root, logs := idleTestManager(t, 1, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := m.Capabilities(ctx, root, "", "fake"); result <- err }()
	awaitIdleLog(t, logs, "initializing")
	m.sweepIdle(time.Now().Add(time.Hour))
	ws, _ := m.workspace(root)
	m.mu.Lock()
	starting := ws.starting["fake"] != nil && ws.statuses["fake"] == "starting"
	m.mu.Unlock()
	if !starting {
		t.Fatal("starting server was evicted")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("startup error = %v", err)
	}
}
