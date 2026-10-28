package mcp

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIsOAuthChallenge(t *testing.T) {
	for _, tt := range []struct {
		values []string
		want   bool
	}{
		{nil, false}, {[]string{`Bearer realm="api"`}, false}, {[]string{"Basic"}, false},
		{[]string{"Basic", `Bearer RESOURCE_METADATA="https://example.invalid/private"`}, true},
	} {
		if got := isOAuthChallenge(http.Header{"Www-Authenticate": tt.values}); got != tt.want {
			t.Errorf("%v: got %v", tt.values, got)
		}
	}
}

func TestHTTPStatusErrorOAuthHint(t *testing.T) {
	for _, tt := range []struct {
		status int
		oauth  bool
		want   string
	}{
		{401, true, "server requires OAuth sign-in"}, {403, false, "check headers"}, {500, true, ""},
	} {
		msg := (&HTTPStatusError{Status: tt.status, OAuth: tt.oauth, Body: "body"}).Error()
		if tt.want != "" && !strings.Contains(msg, tt.want) {
			t.Fatal(msg)
		}
		if tt.oauth && strings.Contains(msg, "check headers") {
			t.Fatal(msg)
		}
		if tt.status == 500 && strings.Contains(msg, "sign-in") {
			t.Fatal(msg)
		}
	}
}

func TestManagerHTTPOAuthChallenge(t *testing.T) {
	t.Setenv("MCP_OAUTH_TEST_TOKEN", "synthetic-bearer-value")
	fake := newHTTPFake(false)
	fake.wantAuth = "Bearer different-value"
	fake.challenge = `Bearer resource_metadata="https://example.invalid/private?secret=value"`
	_, failures := startHTTP(t, fake, func(c *ServerConfig) { c.BearerTokenEnvVar = "MCP_OAUTH_TEST_TOKEN" })
	if len(failures) != 1 {
		t.Fatalf("failures: %v", failures)
	}
	var he *HTTPStatusError
	if !errors.As(failures[0], &he) || !he.OAuth {
		t.Fatalf("missing OAuth error: %v", failures)
	}
	for _, forbidden := range []string{"resource_metadata", "example.invalid", "synthetic-bearer-value"} {
		if strings.Contains(failures[0].Error(), forbidden) {
			t.Fatal(failures[0])
		}
	}
}

func TestScrubLiveKeepsAuthorizationRequestQuery(t *testing.T) {
	const auth = "https://example.invalid/auth?response_type=code&client_id=client&state=state"
	r := newRedactor(ServerConfig{Env: map[string]string{"SECRET": "synthetic-secret"}})
	for _, tt := range []struct{ name, in, want string }{
		{"authorization", auth, auth},
		{"userinfo fragment", "https://user:pass@example.invalid/auth?response_type=code&client_id=client#fragment", "https://example.invalid/auth?response_type=code&client_id=client"},
		{"callback", "https://example.invalid/callback?code=private", "https://example.invalid/callback"},
		{"access token", auth + "&access_token=private", "https://example.invalid/auth"},
		{"api key", "https://example.invalid/api?api_key=private", "https://example.invalid/api"},
		{"secret", "synthetic-secret " + auth, "[redacted] " + auth},
		{"ANSI", "\x1b[31m" + auth + "\x1b[0m", auth},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.scrubLive(tt.in); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
	if got := r.scrub(auth); got != "https://example.invalid/auth" {
		t.Fatal(got)
	}
}

type liveTestBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *liveTestBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *liveTestBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

func TestStderrSinkStreamsAndStopsOnClose(t *testing.T) {
	cfg, _ := fakeStdioConfig(t, nil)
	var b liveTestBuffer
	sink := &stderrSink{mu: &sync.Mutex{}, w: &b, prefix: "mcp[test]: "}
	st, err := startStdio("test", cfg, "", sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.close() })
	sink.line("before")
	st.fail(errors.New("test failure"))
	sink.line("after failure")
	if err := st.close(); err != nil {
		t.Fatal(err)
	}
	before := b.String()
	sink.line("after close")
	if b.String() != before || !strings.Contains(before, "mcp[test]: after failure\n") {
		t.Fatal(b.String())
	}
	var absent *stderrSink
	absent.line("ignored")
	absent.stop()
}

func TestManagerStreamsStderrBeforeInitialize(t *testing.T) {
	const auth = "https://example.invalid/auth?response_type=code&client_id=client"
	lineFile := filepath.Join(t.TempDir(), "stderr-line")
	if err := os.WriteFile(lineFile, []byte(auth), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _ := fakeStdioConfig(t, map[string]string{"FAKE_STDERR_LINE": lineFile, "FAKE_INIT_DELAY_MS": "5000"})
	m := NewManager(Config{Servers: map[string]ServerConfig{"notion": cfg}}, "", ClientInfo{}, nil)
	var b liveTestBuffer
	m.SetStderrWriter(&b)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done; _ = m.Close() })
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !strings.Contains(b.String(), "mcp[notion]: "+auth) {
		select {
		case <-done:
			t.Fatal("startup ended before live stderr")
		case <-deadline.C:
			t.Fatalf("no live stderr: %q", b.String())
		case <-tick.C:
		}
	}
	select {
	case <-done:
		t.Fatal("initialize already finished")
	default:
	}
}
