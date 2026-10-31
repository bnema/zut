package daemon

import (
	"context"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPprofAddress(t *testing.T) {
	for _, tt := range []struct {
		value, want string
		invalid     bool
	}{
		{"", "", false}, {"1", "127.0.0.1:0", false}, {"true", "127.0.0.1:0", false},
		{"127.0.0.1:1234", "127.0.0.1:1234", false}, {"127.0.0.1:0", "127.0.0.1:0", false},
		{"[::1]:0", "[::1]:0", false}, {"localhost:0", "127.0.0.1:0", false},
		{":6060", "", true}, {"0.0.0.0:6060", "", true}, {"[::]:0", "", true},
		{"192.0.2.1:0", "", true}, {"example.com:0", "", true}, {"garbage", "", true},
		{"127.0.0.1:-1", "", true}, {"127.0.0.1:65536", "", true},
	} {
		t.Run(tt.value, func(t *testing.T) {
			got, err := pprofAddress(tt.value)
			if (err != nil) != tt.invalid || got != tt.want {
				t.Fatalf("got %q, %v; want %q invalid=%v", got, err, tt.want, tt.invalid)
			}
		})
	}
}

func TestPprofServesAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, done := startPprof(ctx, "127.0.0.1:0", discardLogger())
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("pprof shutdown timed out")
		}
	})
	if addr == "" {
		t.Fatal("server did not start")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://" + addr + "/debug/pprof/")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "heap") {
		t.Fatalf("index response: status=%d err=%v body=%s", resp.StatusCode, err, body)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pprof did not stop")
	}
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("listener still accepting after shutdown")
	}
}

func TestPprofDisabledAndListenFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for _, value := range []string{"", "0.0.0.0:0", ln.Addr().String()} {
		addr, done := startPprof(context.Background(), value, discardLogger())
		if addr != "" {
			t.Fatalf("unexpected server at %s", addr)
		}
		select {
		case <-done:
		default:
			t.Fatal("failed/disabled server not complete")
		}
	}
}

func TestDaemonPprofDiscovery(t *testing.T) {
	t.Setenv("ZUT_DAEMON_PPROF", "1")
	home := t.TempDir()
	r := startDaemon(t, sessionmocks.NewMockFactory(t), home, Options{})
	path := filepath.Join(home, "daemon.pprof")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimSpace(string(data))
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" || port == "0" {
		t.Fatalf("invalid discovered address %q: %v", addr, err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://" + addr + "/debug/pprof/heap")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heap status %d", resp.StatusCode)
	}
	r.stop(t)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("discovery file remains: %v", err)
	}
}
