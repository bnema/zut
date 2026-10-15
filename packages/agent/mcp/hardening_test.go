package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the review findings: write path, SSE limits, session
// expiry, diagnostics hygiene, process cleanup, refresh coalescing, and the
// cancellation rules.

func connectFake(t *testing.T, extraEnv map[string]string) (*Client, string) {
	t.Helper()
	cfg, events := fakeStdioConfig(t, extraEnv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Connect(ctx, "fake", cfg, "", ClientInfo{Name: "t"}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, events
}

func callText(t *testing.T, c *Client, tool, args string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, tool, []byte(args))
	if err != nil {
		t.Fatalf("CallTool %s: %v", tool, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("CallTool %s: no content", tool)
	}
	return res.Content[0].Text
}

// ---- 1, 8: cancellable writes, close behind a stuck writer ----

func TestStdioWriteStuckChildDoesNotWedgeCancelOrClose(t *testing.T) {
	c, events := connectFake(t, nil)

	// The server stops reading stdin after receiving this call.
	stallCtx, stallCancel := context.WithCancel(context.Background())
	stallDone := make(chan error, 1)
	go func() {
		_, err := c.CallTool(stallCtx, "stall", []byte(`{}`))
		stallDone <- err
	}()
	waitEvent(t, events, "stalled")

	// A frame far larger than the pipe buffer now blocks the writer.
	big := `{"text":"` + strings.Repeat("a", 4<<20) + `"}`
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.CallTool(ctx, "echo", []byte(big))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("request ignored its context for %v", d)
	}

	closed := make(chan struct{})
	go func() { _ = c.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung behind a blocked write")
	}
	select {
	case <-c.Done():
	default:
		t.Error("Done not closed after Close")
	}
	stallCancel()
	select {
	case <-stallDone:
	case <-time.After(5 * time.Second):
		t.Fatal("pending call not released by Close")
	}
}

// ---- 2: SSE aggregate limit ----

func TestReadSSEFramesAggregateLimit(t *testing.T) {
	line := "data: " + strings.Repeat("x", 9) + "\n" // 9 bytes of data per line
	var delivered int
	handle := func(message) bool { delivered++; return false }

	// Many short lines of one event exceed the limit.
	err := readSSEFramesLimit(strings.NewReader(strings.Repeat(line, 200)+"\n"), 1000, handle)
	var tooLarge errSSEEventTooLarge
	if !errors.As(err, &tooLarge) || !strings.Contains(err.Error(), "1000") {
		t.Fatalf("err = %v, want errSSEEventTooLarge", err)
	}
	if delivered != 0 {
		t.Errorf("oversized event delivered %d frames", delivered)
	}

	// The limit is per event: a blank line resets it.
	ok := `data: {"jsonrpc":"2.0","method":"a"}` + "\n\n"
	delivered = 0
	if err := readSSEFramesLimit(strings.NewReader(strings.Repeat(ok, 50)), 100, handle); err != nil {
		t.Fatal(err)
	}
	if delivered != 50 {
		t.Errorf("delivered %d, want 50", delivered)
	}

	// A single line longer than the limit is the same explicit error.
	err = readSSEFramesLimit(strings.NewReader("data: "+strings.Repeat("y", 5000)+"\n\n"), 1000, handle)
	if !errors.As(err, &tooLarge) {
		t.Errorf("long line err = %v", err)
	}
}

func TestSSEEndlessEventStopsAtLimit(t *testing.T) {
	// A server that never ends its event must not grow memory without bound.
	err := readSSEFramesLimit(&infiniteSSE{}, 64<<10, func(message) bool { return false })
	var tooLarge errSSEEventTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v", err)
	}
}

type infiniteSSE struct{}

func (*infiniteSSE) Read(p []byte) (int, error) {
	line := []byte("data: " + strings.Repeat("q", 100) + "\n")
	n := copy(p, line)
	return n, nil
}

// ---- 3: session expiry ----

func TestHTTPSessionExpiredReconnects(t *testing.T) {
	for _, mode := range []struct {
		name string
		sse  bool
	}{{"json", false}, {"sse", true}} {
		t.Run(mode.name, func(t *testing.T) {
			fake := newHTTPFake(mode.sse)
			m, failures := startHTTP(t, fake, nil)
			if len(failures) != 0 {
				t.Fatal(failures)
			}
			echo := findTool(t, m, "mcp__web__echo")
			fake.expireNextCall.Store(true)
			res := execTool(t, echo, `{"text":"one"}`)
			if !res.IsError || !strings.Contains(resultText(res), "session expired") {
				t.Fatalf("expired call = %q (error=%v)", resultText(res), res.IsError)
			}
			// The transport is unusable, so the status reflects it at once.
			if st := m.Status()[0]; st.Connected {
				t.Errorf("still marked connected after 404: %+v", st)
			}
			// The next call reconnects (fresh initialize) and succeeds.
			if got := resultText(execTool(t, echo, `{"text":"two"}`)); got != "echo: two" {
				t.Fatalf("after reconnect = %q", got)
			}
			inits, calls := fake.counts()
			if inits != 2 {
				t.Errorf("initialize count = %d, want 2", inits)
			}
			if calls != 2 {
				t.Errorf("tools/call count = %d, want 2 (the failed call must not be replayed)", calls)
			}
			// No DELETE for the dead session.
			select {
			case sid := <-fake.deleted:
				t.Errorf("DELETE sent for expired session %q", sid)
			default:
			}
			// The new session carries its own id.
			var last httpRecord
			for _, r := range fake.snapshot() {
				if r.RPCMethod == "tools/call" {
					last = r
				}
			}
			if last.Session != "sess-abc-2" {
				t.Errorf("session after reconnect = %q", last.Session)
			}
		})
	}
}

func TestHTTPNotFoundWithoutSessionIsOrdinaryError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	tr, err := newHTTPTransport("x", ServerConfig{URL: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	_, err = tr.request(context.Background(), 1, "initialize", nil)
	var he *HTTPStatusError
	if !errors.As(err, &he) || he.Status != 404 {
		t.Fatalf("err = %v", err)
	}
	select {
	case <-tr.done():
		t.Error("a 404 before any session must not poison the transport")
	default:
	}
}

// ---- 4, 13: diagnostics hygiene ----

func TestScrubURLsAndErrors(t *testing.T) {
	red := newRedactor(ServerConfig{})
	uerr := &url.Error{Op: "Post", URL: "https://user:pw@h.example:8443/mcp/v1?token=abc&x=1#frag", Err: errors.New("connection refused")}
	got := red.scrubError(uerr).Error()
	if want := `Post "https://h.example:8443/mcp/v1": connection refused`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	var target *url.Error
	if !errors.As(red.scrubError(uerr), &target) {
		t.Error("original error must stay reachable via errors.As")
	}
	for in, want := range map[string]string{
		"see http://a.example/p?k=v for details":       "see http://a.example/p for details",
		"ws://u@h/x and https://h2/y?q=1":              "ws://h/x and https://h2/y",
		"no urls here":                                 "no urls here",
		"odd-scheme://x:y@z/p?secret=1,next":           "odd-scheme://z/p",
		"bad scheme:// and http://host?only=query end": "bad scheme:// and http://host end",
	} {
		if got := scrubURLs(in); got != want {
			t.Errorf("scrubURLs(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactorValues(t *testing.T) {
	t.Setenv("MCP_T_HDR", "hdr-secret-77")
	t.Setenv("MCP_T_BEARER2", " bearer-secret-99 ")
	t.Setenv("MCP_T_PASS2", "passthru-secret-5")
	red := newRedactor(ServerConfig{
		Env:               map[string]string{"A": "env-secret-1", "SHORT": "ab"},
		Headers:           map[string]string{"Authorization": "Bearer static-secret-2"},
		EnvHeaders:        map[string]string{"X": "MCP_T_HDR"},
		BearerTokenEnvVar: "MCP_T_BEARER2",
		EnvVars:           []string{"MCP_T_PASS2"},
		URL:               "https://u:urlpass-3@h.example/p?key=querysecret-4",
	})
	in := "env-secret-1 static-secret-2 Bearer static-secret-2 hdr-secret-77 bearer-secret-99 passthru-secret-5 urlpass-3 querysecret-4 ab"
	out := red.scrub(in)
	for _, secret := range []string{"env-secret-1", "static-secret-2", "hdr-secret-77", "bearer-secret-99", "passthru-secret-5", "urlpass-3", "querysecret-4"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q leaked in %q", secret, out)
		}
	}
	if !strings.HasSuffix(out, " ab") {
		t.Errorf("short values must not be redacted: %q", out)
	}
	var nilRed *redactor
	if got := nilRed.scrub("plain \x1b[1mtext"); got != "plain text" {
		t.Errorf("nil redactor = %q", got)
	}
}

func TestSanitizeText(t *testing.T) {
	cases := []struct {
		name, in string
		keepNL   bool
		want     string
	}{
		{"csi", "a\x1b[31mred\x1b[0mb", true, "aredb"},
		{"osc bel", "x\x1b]0;title\x07y", true, "xy"},
		{"osc st", "\x1b]8;;http://e\x1b\\link\x1b]8;;\x1b\\", true, "link"},
		{"dcs", "p\x1bPq#0;2\x1b\\q", true, "pq"},
		{"c1 csi", "\u009b31mX", true, "X"},
		{"cr dropped", "a\rb", true, "ab"},
		{"tab", "a\tb", true, "a b"},
		{"newline kept", "l1\nl2", true, "l1\nl2"},
		{"newline folded", "l1\nl2", false, "l1 l2"},
		{"bidi", "\u202eevil\u2066", true, "evil"},
		{"controls", "\x00a\x07b\x7fc", true, "abc"},
		{"lone esc", "end\x1b", true, "end"},
		{"unicode kept", "héllo ✓ 日本", true, "héllo ✓ 日本"},
		{"unterminated osc", "a\x1b]0;never ends", true, "a"},
		{"esc cancels osc", "a\x1b]0;t\x1b[31mred", true, "ared"},
		{"charset", "a\x1b(Bb", true, "ab"},
	}
	for _, c := range cases {
		if got := sanitizeText(c.in, c.keepNL); got != c.want {
			t.Errorf("%s: sanitizeText(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestFormatStatusStripsControlSequences(t *testing.T) {
	text := FormatStatus([]ServerStatus{{
		Name:      "ev\x1b]0;pwn\x07il",
		Transport: "stdio",
		Err:       "boom \x1b[31mred\x1b[0m\x1b]52;c;ZXZpbA==\x07\nsecond line",
	}})
	if strings.ContainsAny(text, "\x1b\x07") {
		t.Errorf("control characters survived: %q", text)
	}
	if !strings.Contains(text, "evil (stdio): disconnected") || !strings.Contains(text, "error: boom red") || strings.Contains(text, "second line") {
		t.Errorf("text = %q", text)
	}
}

func TestStdioDiagnosticsAreScrubbed(t *testing.T) {
	const secret = "s3cr3t-value"
	cfg, _ := fakeStdioConfig(t, map[string]string{"FAKE_INIT_ERR": "1", "FAKE_ERR_ECHO": secret, "FAKE_ANSI": "1"})
	_, failures, _ := startManager(t, map[string]ServerConfig{"s": cfg}, "")
	if len(failures) != 1 {
		t.Fatalf("failures = %v", failures)
	}
	msg := failures[0].Error()
	if strings.Contains(msg, secret) {
		t.Errorf("secret leaked: %q", msg)
	}
	if strings.ContainsAny(msg, "\x1b\x07") || strings.Contains(msg, "evil-title") {
		t.Errorf("control sequences survived: %q", msg)
	}
	for _, want := range []string{redactedMarker, "fatal: missing credentials", "http://example.invalid/x", "red end"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %q", want, msg)
		}
	}
	if strings.Contains(msg, "?k=") {
		t.Errorf("URL query survived: %q", msg)
	}
}

func TestHTTPDiagnosticsAreScrubbed(t *testing.T) {
	t.Run("error body", func(t *testing.T) {
		t.Setenv("MCP_T_BEARER", "tok-abcdef123")
		fake := newHTTPFake(false)
		fake.errorBody = "denied tok-abcdef123 \x1b[31mred\x1b[0m at http://x.example/p?token=tok-abcdef123"
		_, failures := startHTTP(t, fake, func(c *ServerConfig) { c.BearerTokenEnvVar = "MCP_T_BEARER" })
		if len(failures) != 1 {
			t.Fatalf("failures = %v", failures)
		}
		msg := failures[0].Error()
		if strings.Contains(msg, "tok-abcdef123") || strings.ContainsRune(msg, 0x1b) || strings.Contains(msg, "?token") {
			t.Errorf("unsanitized: %q", msg)
		}
		if !strings.Contains(msg, redactedMarker) || !strings.Contains(msg, "500") {
			t.Errorf("message = %q", msg)
		}
	})
	t.Run("url error", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		base := srv.URL
		srv.Close()
		cfg := ServerConfig{Type: "http", URL: base + "/mcp?access_token=QUERYSECRET"}
		_, failures, _ := startManager(t, map[string]ServerConfig{"gone": cfg}, "")
		if len(failures) != 1 {
			t.Fatalf("failures = %v", failures)
		}
		msg := failures[0].Error()
		if strings.Contains(msg, "QUERYSECRET") || strings.Contains(msg, "access_token") {
			t.Errorf("query leaked: %q", msg)
		}
		if !strings.Contains(msg, "127.0.0.1") {
			t.Errorf("host should remain for debugging: %q", msg)
		}
	})
}

// ---- 5: process cleanup ----

func TestStdioCloseKillsDescendants(t *testing.T) {
	if !canCheckProcess {
		t.Skip("process liveness is not observable on this platform")
	}
	for _, mode := range []string{"sleeper", "sleeper-stubborn"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := connectFake(t, nil)
			pid, err := strconv.Atoi(callText(t, c, "spawn", `{"mode":"`+mode+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			if !processAlive(pid) {
				t.Fatal("descendant is not running before Close")
			}
			// Make sure we never leak it even if the assertion below fails.
			t.Cleanup(func() { killPID(pid) })
			_ = c.Close()
			eventually(t, "descendant to die", func() bool { return !processAlive(pid) })
		})
	}
}

// ---- 6, 10: refresh coalescing ----

func TestToolRefresherCoalescesAndSerializes(t *testing.T) {
	r := newToolRefresher()
	var running, maxRunning, runs atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 16)

	r.notify() // before start: remembered, not lost
	r.start(func(ctx context.Context) {
		n := running.Add(1)
		for {
			old := maxRunning.Load()
			if n <= old || maxRunning.CompareAndSwap(old, n) {
				break
			}
		}
		runs.Add(1)
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		running.Add(-1)
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("notification before start never triggered a listing")
	}
	for i := 0; i < 100; i++ {
		r.notify() // all arrive during the running listing
	}
	close(release)
	eventually(t, "refresher to go idle", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return !r.running
	})
	if got := runs.Load(); got != 2 {
		t.Errorf("runs = %d, want exactly 2 (initial + one coalesced re-run)", got)
	}
	if got := maxRunning.Load(); got != 1 {
		t.Errorf("max concurrent listings = %d, want 1", got)
	}
}

func TestToolRefresherStop(t *testing.T) {
	r := newToolRefresher()
	entered := make(chan struct{})
	finished := make(chan struct{})
	var runs atomic.Int32
	r.start(func(ctx context.Context) {
		runs.Add(1)
		close(entered)
		<-ctx.Done() // only a stop can end this listing
		close(finished)
	})
	r.notify()
	<-entered
	r.notify() // dirty again while running
	r.stop()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not cancel the running listing")
	}
	r.notify()
	eventually(t, "refresher idle", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return !r.running
	})
	if got := runs.Load(); got != 1 {
		t.Errorf("runs after stop = %d, want 1", got)
	}
}

func TestManagerStopsRefresherOnClose(t *testing.T) {
	m, _ := startFake(t, nil, nil)
	s := m.servers["fake"]
	s.mu.Lock()
	r := s.refresher
	s.mu.Unlock()
	if r == nil {
		t.Fatal("no refresher installed")
	}
	_ = m.Close()
	r.mu.Lock()
	stopped := r.stopped
	r.mu.Unlock()
	if !stopped {
		t.Error("refresher still active after Close")
	}
	s.mu.Lock()
	left := s.refresher
	s.mu.Unlock()
	if left != nil {
		t.Error("refresher reference kept after Close")
	}
}

func TestManagerListChangedDuringInitialListing(t *testing.T) {
	// The server announces a changed tool set right after answering the last
	// page of the first listing, before Start has published the client.
	m, _ := startFake(t, map[string]string{"FAKE_NOTIFY_ON_LIST": "1"}, nil)
	eventually(t, "tool added during initial listing to appear", func() bool {
		for _, tool := range m.ServerTools("fake") {
			if tool.Name == "extra" {
				return true
			}
		}
		return false
	})
}

// ---- 7: stderr draining ----

func TestStdioStderrFloodDoesNotBlockChild(t *testing.T) {
	c, _ := connectFake(t, nil)
	// 3 MiB on one stderr line, then a short one. The child blocks on the
	// stderr pipe unless the client keeps draining it.
	if got := callText(t, c, "stderr_flood", `{}`); got != "flooded" {
		t.Fatalf("got %q", got)
	}
	st := c.t.(*stdioTransport)
	eventually(t, "stderr tail to include the last line", func() bool {
		return strings.Contains(st.stderrTail(), "after-flood")
	})
	tail := st.stderrTail()
	if !strings.Contains(tail, "[line truncated]") {
		t.Error("long line should be marked truncated")
	}
	if len(tail) > stderrTailLines*(maxStderrLine+64) {
		t.Errorf("tail not bounded: %d bytes", len(tail))
	}
	// The server still works afterwards.
	if got := callText(t, c, "echo", `{"text":"ok"}`); got != "echo: ok" {
		t.Errorf("after flood: %q", got)
	}
}

// ---- 8, 9: cancellation notifications ----

func TestStdioCancelNotificationRules(t *testing.T) {
	// initialize never answers within the deadline.
	cfg, _ := fakeStdioConfig(t, map[string]string{"FAKE_INIT_DELAY_MS": "5000"})
	st, err := startStdio("x", cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := st.request(ctx, 1, "initialize", map[string]any{"protocolVersion": LatestProtocolVersion}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if got := st.cancelsSent.Load(); got != 0 {
		t.Errorf("notifications/cancelled sent for initialize (%d)", got)
	}
	// Any other method is announced exactly once, without blocking the caller.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	start := time.Now()
	_, err = st.request(ctx2, 2, "tools/call", map[string]any{"name": "slow", "arguments": map[string]any{}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Error("caller delayed by the cancel notification")
	}
	if got := st.cancelsSent.Load(); got != 1 {
		t.Errorf("cancel count for tools/call = %d, want 1", got)
	}
}

func TestHTTPInitializeNotCancelledOnTimeout(t *testing.T) {
	fake := newHTTPFake(false)
	fake.hangInit = true
	srv := fake.start(t)
	cfg := httpServerConfig(srv)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := Connect(ctx, "web", cfg, "", ClientInfo{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	for _, r := range fake.snapshot() {
		if r.RPCMethod == "notifications/cancelled" {
			t.Fatal("notifications/cancelled sent for initialize")
		}
	}
}

func TestHTTPCancelNotificationIsBoundedAndOwnedByTransport(t *testing.T) {
	fake := newHTTPFake(false)
	fake.cancelGate = make(chan struct{})
	t.Cleanup(func() { close(fake.cancelGate) })
	m, failures := startHTTP(t, fake, func(c *ServerConfig) { c.ToolTimeoutSec = 0.2 })
	if len(failures) != 0 {
		t.Fatal(failures)
	}
	start := time.Now()
	res := execTool(t, findTool(t, m, "mcp__web__slow"), `{}`)
	if !res.IsError || !strings.Contains(resultText(res), "toolTimeoutSec") {
		t.Fatalf("got %q", resultText(res))
	}
	// The cancellation POST is stuck on the server (gate closed); the caller
	// must not wait for it.
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("caller delayed by the cancel notification: %v", d)
	}
	eventually(t, "cancel notification to reach the server", func() bool {
		for _, r := range fake.snapshot() {
			if r.RPCMethod == "notifications/cancelled" {
				return true
			}
		}
		return false
	})
	// Closing the transport aborts the stuck notification; otherwise the
	// httptest server would wait for it at cleanup.
	closed := make(chan struct{})
	go func() { _ = m.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung behind the cancel notification")
	}
}

// ---- 11: file URIs ----

func TestFileURI(t *testing.T) {
	cases := []struct {
		name, path string
		windows    bool
		want       string
	}{
		{"unix", "/home/me/proj", false, "file:///home/me/proj"},
		{"unix space", "/home/me/my proj", false, "file:///home/me/my%20proj"},
		{"drive", `C:\Users\me\proj`, true, "file:///C:/Users/me/proj"},
		{"drive space", `D:\My Docs\x`, true, "file:///D:/My%20Docs/x"},
		{"drive root", `C:\`, true, "file:///C:/"},
		{"drive forward slashes", `C:/x/y`, true, "file:///C:/x/y"},
		{"unc", `\\host\share\dir`, true, "file://host/share/dir"},
		{"unc share only", `\\host\share`, true, "file://host/share"},
		{"extended drive", `\\?\C:\x`, true, "file:///C:/x"},
		{"extended unc", `\\?\UNC\host\share\d`, true, "file://host/share/d"},
	}
	for _, c := range cases {
		if got := fileURI(c.path, c.windows); got != c.want {
			t.Errorf("%s: fileURI(%q) = %q, want %q", c.name, c.path, got, c.want)
		}
	}
}

// ---- 12: DELETE shape ----

func TestHTTPDeleteCarriesProtocolVersionAndHeaders(t *testing.T) {
	t.Setenv("MCP_T_BEARER", "tok-delete-1")
	fake := newHTTPFake(false)
	m, failures := startHTTP(t, fake, func(c *ServerConfig) {
		c.BearerTokenEnvVar = "MCP_T_BEARER"
		c.Headers = map[string]string{"X-Static": "static"}
	})
	if len(failures) != 0 {
		t.Fatal(failures)
	}
	_ = m.Close()
	select {
	case <-fake.deleted:
	case <-time.After(5 * time.Second):
		t.Fatal("no DELETE")
	}
	var del *httpRecord
	for _, r := range fake.snapshot() {
		if r.Method == http.MethodDelete {
			r := r
			del = &r
		}
	}
	if del == nil {
		t.Fatal("DELETE not recorded")
	}
	if del.Protocol != "2025-06-18" || del.Session != "sess-abc" || del.Auth != "Bearer tok-delete-1" || del.Static != "static" {
		t.Errorf("DELETE = %+v", *del)
	}
}

// ---- 14: resource listing budget ----

func TestListResourcesAppliesBudgetWhileFormatting(t *testing.T) {
	m, events := startFake(t, map[string]string{"FAKE_MANY_RESOURCES": "2000"}, nil)
	text := resultText(execTool(t, findTool(t, m, ListResourcesToolName), `{}`))
	marker := "\n... [truncated at 51200 bytes]"
	if !strings.Contains(text, marker) {
		t.Fatalf("no truncation marker (len=%d)", len(text))
	}
	if len(text) > 50*1024+len(marker) {
		t.Errorf("output not bounded: %d bytes", len(text))
	}
	if !strings.Contains(text, "mem://item/0") || strings.Contains(text, "mem://item/1999") {
		t.Error("expected the head of the list, not the tail")
	}
	// Once the budget is spent no further requests are made.
	if n := countEvents(events, "list-templates"); n != 0 {
		t.Errorf("templates were fetched after the budget was exhausted (%d)", n)
	}
	if n := countEvents(events, "list-resources"); n != 1 {
		t.Errorf("list-resources calls = %d", n)
	}
}

func TestBudgetedText(t *testing.T) {
	b := &budgetedText{}
	b.write(strings.Repeat("a", maxResultTextBytes-1))
	if b.full() {
		t.Fatal("full too early")
	}
	b.write("bcdef")
	if !b.full() {
		t.Fatal("not full after overflow")
	}
	before := b.String()
	b.write("more")
	b.printf("%s", "more")
	if b.String() != before {
		t.Error("writes accepted after the budget was exhausted")
	}
	if !strings.HasSuffix(before, "[truncated at 51200 bytes]") {
		t.Errorf("tail = %q", before[len(before)-40:])
	}
}
