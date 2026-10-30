package mcp

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type readyResult struct {
	name string
	err  error
}

func TestStartBackgroundReturnsBeforeSlowOptionalServer(t *testing.T) {
	slow, _ := fakeStdioConfig(t, map[string]string{"FAKE_INIT_DELAY_MS": "700"})
	required, _ := fakeStdioConfig(t, nil)
	required.Required = true
	m := NewManager(Config{Servers: map[string]ServerConfig{"slow": slow, "req": required}}, "", ClientInfo{}, nil)
	t.Cleanup(func() { _ = m.Close() })
	ready := make(chan readyResult, 2)
	start := time.Now()
	failures, reqErr := m.StartBackground(context.Background(), func(_ context.Context, name string, _ time.Duration, err error) {
		ready <- readyResult{name, err}
	})
	if len(failures) != 0 || reqErr != nil {
		t.Fatalf("failures=%v reqErr=%v", failures, reqErr)
	}
	if len(m.ServerTools("req")) == 0 {
		t.Fatal("required server must be connected when StartBackground returns")
	}
	if len(m.ServerTools("slow")) != 0 {
		t.Fatal("slow server ready before StartBackground returned")
	}
	if took := time.Since(start); took >= 700*time.Millisecond {
		t.Fatalf("StartBackground waited for the optional server (%s)", took)
	}
	select {
	case r := <-ready:
		if r.name != "slow" || r.err != nil {
			t.Fatalf("ready = %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("slow server never reported ready")
	}
	if len(m.ServerTools("slow")) == 0 {
		t.Fatal("slow server tools missing after ready")
	}
}

func TestStartBackgroundRequiredFailureBlocks(t *testing.T) {
	cfg, _ := fakeStdioConfig(t, map[string]string{"FAKE_INIT_ERR": "1"})
	cfg.Required = true
	m := NewManager(Config{Servers: map[string]ServerConfig{"req": cfg}}, "", ClientInfo{}, nil)
	t.Cleanup(func() { _ = m.Close() })
	failures, reqErr := m.StartBackground(context.Background(), func(context.Context, string, time.Duration, error) {
		t.Error("onReady must not run for required servers")
	})
	if len(failures) != 1 || reqErr == nil || !strings.Contains(reqErr.Error(), "req") {
		t.Fatalf("failures=%v reqErr=%v", failures, reqErr)
	}
}

func TestStartBackgroundOptionalFailureReported(t *testing.T) {
	cfg, _ := fakeStdioConfig(t, map[string]string{"FAKE_INIT_ERR": "1"})
	m := NewManager(Config{Servers: map[string]ServerConfig{"bad": cfg}}, "", ClientInfo{}, nil)
	t.Cleanup(func() { _ = m.Close() })
	ready := make(chan readyResult, 1)
	failures, reqErr := m.StartBackground(context.Background(), func(_ context.Context, name string, _ time.Duration, err error) {
		ready <- readyResult{name, err}
	})
	if len(failures) != 0 || reqErr != nil {
		t.Fatalf("failures=%v reqErr=%v", failures, reqErr)
	}
	select {
	case r := <-ready:
		if r.name != "bad" || r.err == nil || !strings.Contains(r.err.Error(), "init refused") {
			t.Fatalf("ready = %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("failure never reported")
	}
}

func TestCloseCancelsPendingBackgroundStart(t *testing.T) {
	hung, events := fakeStdioConfig(t, map[string]string{"FAKE_HANG_INIT": "1"})
	hung.StartupTimeoutSec = 60
	reaped := make(chan int, 4)
	hook := func(pid int) { reaped <- pid }
	afterReapHook.Store(&hook)
	defer afterReapHook.Store(nil)
	m := NewManager(Config{Servers: map[string]ServerConfig{"hung": hung}}, "", ClientInfo{}, nil)
	var mu sync.Mutex
	called := false
	if _, err := m.StartBackground(context.Background(), func(context.Context, string, time.Duration, error) {
		mu.Lock()
		called = true
		mu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, events, "start")
	closed := make(chan struct{})
	go func() { _ = m.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not cancel the pending start")
	}
	select {
	case <-reaped:
	case <-time.After(5 * time.Second):
		t.Fatal("pending server process was not reaped")
	}
	mu.Lock()
	defer mu.Unlock()
	if called {
		t.Fatal("onReady ran after Close")
	}
}

func TestStartBackgroundAfterClose(t *testing.T) {
	cfg, events := fakeStdioConfig(t, nil)
	m := NewManager(Config{Servers: map[string]ServerConfig{"s": cfg}}, "", ClientInfo{}, nil)
	_ = m.Close()
	if _, err := m.StartBackground(context.Background(), nil); err == nil {
		t.Fatal("expected error on closed manager")
	}
	if len(readEvents(events)) != 0 {
		t.Fatal("a closed manager must not spawn servers")
	}
}
