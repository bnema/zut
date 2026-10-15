package mcp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Regression tests for the second review round.

// ---- GO-015: write cancellation ----

func TestStdioQueuedFrameIsWithdrawnButStartedWriteFailsTransport(t *testing.T) {
	c, events := connectFake(t, nil)
	st := c.t.(*stdioTransport)

	// The server stops reading stdin.
	stallCtx, stallCancel := context.WithCancel(context.Background())
	defer stallCancel()
	go func() { _, _ = c.CallTool(stallCtx, "stall", []byte(`{}`)) }()
	waitEvent(t, events, "stalled")

	// Frame A is far larger than the pipe buffer: its write starts and blocks.
	started := st.writesStarted.Load()
	aCtx, aCancel := context.WithCancel(context.Background())
	big := bytes.Repeat([]byte("a"), 4<<20)
	aDone := make(chan struct {
		sent bool
		err  error
	}, 1)
	go func() {
		sent, err := st.writeFrame(aCtx, big)
		aDone <- struct {
			sent bool
			err  error
		}{sent, err}
	}()
	eventually(t, "frame A to start writing", func() bool { return st.writesStarted.Load() == started+1 })

	// Frame B queues behind A. Its caller gives up before B is dispatched:
	// B must be withdrawn, never written, and the transport must stay usable.
	bCtx, bCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer bCancel()
	sent, err := st.writeFrame(bCtx, []byte(`{"jsonrpc":"2.0","method":"notifications/never"}`))
	if sent || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued frame: sent=%v err=%v, want withdrawn with deadline error", sent, err)
	}
	select {
	case <-st.done():
		t.Fatal("withdrawing a queued frame must not fail the transport")
	default:
	}

	// Now A's caller gives up mid-write: the frame cannot be withdrawn, so the
	// transport fails and stdin is closed.
	aCancel()
	select {
	case r := <-aDone:
		if !r.sent || !errors.Is(r.err, context.Canceled) {
			t.Errorf("started frame: sent=%v err=%v", r.sent, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("caller still blocked after cancellation")
	}
	select {
	case <-st.done():
	case <-time.After(5 * time.Second):
		t.Fatal("transport still live with a half-written frame")
	}
	if err := st.failure(); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("failure = %v", err)
	}
	// B was never handed to the writer.
	if got := st.writesStarted.Load(); got != started+1 {
		t.Errorf("writes started = %d, want %d (frame B must never be written)", got, started+1)
	}
}

func TestManagerReconnectsAfterInterruptedWrite(t *testing.T) {
	m, events := startFake(t, nil, func(c *ServerConfig) { c.ToolTimeoutSec = 0.4 })
	// Park the server: the call is delivered, then it stops reading stdin.
	if res := execTool(t, findTool(t, m, "mcp__fake__stall"), `{}`); !res.IsError {
		t.Fatalf("stall result = %q", resultText(res))
	}
	waitEvent(t, events, "stalled")
	// A request too large for the pipe times out mid-write.
	big := `{"text":"` + strings.Repeat("a", 4<<20) + `"}`
	if res := execTool(t, findTool(t, m, "mcp__fake__echo"), big); !res.IsError {
		t.Fatalf("big call result = %q", resultText(res))
	}
	eventually(t, "status to show the dropped connection", func() bool { return !m.Status()[0].Connected })
	// The next call starts a fresh server and works.
	if got := resultText(execTool(t, findTool(t, m, "mcp__fake__echo"), `{"text":"again"}`)); got != "echo: again" {
		t.Fatalf("after reconnect = %q", got)
	}
	if n := countEvents(events, "start"); n != 2 {
		t.Errorf("server starts = %d, want 2", n)
	}
}

// ---- GO-004, GO-018: stderr redaction ----

func newTestStderrTransport(cfg ServerConfig) *stdioTransport {
	return &stdioTransport{red: newRedactor(cfg), stderrDone: make(chan struct{})}
}

func TestStderrSecretStraddlingTruncationIsRedacted(t *testing.T) {
	const secret = "SECRETVALUE-0123456789"
	tr := newTestStderrTransport(ServerConfig{Env: map[string]string{"K": secret}})
	// The secret starts before the 4096-byte display cut and ends after it.
	line := strings.Repeat("x", maxStderrLine-len(secret)/2) + secret + strings.Repeat("y", 300) + "\nnext line\n"
	tr.readStderr(strings.NewReader(line))
	tail := tr.stderrTail()
	for _, frag := range []string{"SECRET", "VALUE", "0123", secret[:8]} {
		if strings.Contains(tail, frag) {
			t.Errorf("secret fragment %q leaked: %.80q…", frag, tail[maxStderrLine-60:])
		}
	}
	if !strings.Contains(tail, redactedMarker) || !strings.Contains(tail, "[line truncated]") || !strings.Contains(tail, "next line") {
		t.Errorf("unexpected tail shape (len %d)", len(tail))
	}
}

func TestStderrSecretOnTheCutIsNotPartiallyShown(t *testing.T) {
	// A line whose redacted form still ends in a secret prefix would leak it;
	// trimPartialSecret removes such a tail.
	red := newRedactor(ServerConfig{Env: map[string]string{"K": "TOPSECRET99"}})
	for _, in := range []string{"abc TOP", "abc TOPSECRET9", "abc TOPSECRET", "abc"} {
		got := red.trimPartialSecret(in)
		if strings.Contains(got, "TOP") {
			t.Errorf("trimPartialSecret(%q) = %q", in, got)
		}
	}
	if got := red.trimPartialSecret("keep TOPS and"); got != "keep TOPS and" {
		t.Errorf("interior text changed: %q", got)
	}
	var nilRed *redactor
	if got := nilRed.trimPartialSecret("x"); got != "x" {
		t.Error("nil redactor must pass text through")
	}
}

func TestStdioStderrSecretEndToEnd(t *testing.T) {
	const secret = "E2ESECRET-abcdefghij"
	c, _ := connectFake(t, map[string]string{"FAKE_SECRET": secret})
	if got := callText(t, c, "stderr_secret", `{}`); got != "written" {
		t.Fatal(got)
	}
	st := c.t.(*stdioTransport)
	eventually(t, "stderr to be read", func() bool { return strings.Contains(st.stderrTail(), "after-secret") })
	tail := st.stderrTail()
	if strings.Contains(tail, "E2ESECRET") || strings.Contains(tail, "abcdef") {
		t.Errorf("secret (or prefix) leaked")
	}
}

func TestRedactionSurvivesControlSequencesInsideSecret(t *testing.T) {
	// Case 1: the server's output splits the secret with escape sequences.
	red := newRedactor(ServerConfig{Env: map[string]string{"K": "token-abcdef123"}})
	out := red.scrub("leak: tok\x1b[0men-abc\x1b]0;x\x07def123 end")
	if strings.Contains(out, "abcdef123") || strings.Contains(out, "token") || !strings.Contains(out, redactedMarker) {
		t.Errorf("out = %q", out)
	}
	// Case 2: the configured secret itself contains control bytes.
	red = newRedactor(ServerConfig{Env: map[string]string{"K": "se\x1b[1mcret-value-9"}})
	out = red.scrub("echo se\x1b[1mcret-value-9 / secret-value-9")
	if strings.Contains(out, "cret-value-9") {
		t.Errorf("out = %q", out)
	}
}

// ---- GO-016: request-building errors ----

func TestHTTPRequestBuildErrorIsScrubbed(t *testing.T) {
	const secret = "QUERYSECRET-42"
	cfg := ServerConfig{Type: "http", URL: "http://bad host/path?token=" + secret}
	tr, err := newHTTPTransport("x", cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	_, err = tr.request(context.Background(), 1, "initialize", nil)
	if err == nil {
		t.Fatal("want an error for an unparsable URL")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks the query token: %v", err)
	}
	// The same path also feeds close() and the GET stream.
	if _, err := tr.newRequest(context.Background(), "GET", nil); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("newRequest error = %v", err)
	}
}

// ---- GO-017: encoded URL secrets ----

func TestURLSecretsIncludePercentDecodedForms(t *testing.T) {
	raw := "https://us%40er:p%40ssw0rd@h.example/p?api_key=a%2Bb%20c&other=plain-value&semi=x;y=z-token"
	got := map[string]bool{}
	for _, v := range urlSecrets(raw) {
		got[v] = true
	}
	for _, want := range []string{"us%40er", "us@er", "p%40ssw0rd", "p@ssw0rd", "a%2Bb%20c", "a+b c", "plain-value"} {
		if !got[want] {
			t.Errorf("urlSecrets missing %q (have %v)", want, got)
		}
	}
	red := newRedactor(ServerConfig{URL: raw})
	out := red.scrub("server said p@ssw0rd and a+b c and a%2Bb%20c and plain-value")
	for _, secret := range []string{"p@ssw0rd", "a+b c", "a%2Bb%20c", "plain-value"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q leaked in %q", secret, out)
		}
	}
	// A URL that net/url refuses to parse still yields its secrets.
	unparsable := strings.Replace("http://bad_host/x?tok=SECRETQ99", "_", " ", 1) // space in the host
	if _, err := url.Parse(unparsable); err == nil {
		t.Fatal("test premise: URL should not parse")
	}
	bad := urlSecrets(unparsable)
	found := false
	for _, v := range bad {
		found = found || v == "SECRETQ99"
	}
	if !found {
		t.Errorf("unparsable URL secrets = %v", bad)
	}
}

// ---- GO-014: per-field budget ----

func TestBudgetedFieldIsBoundedBeforeFormatting(t *testing.T) {
	b := &budgetedText{}
	huge := strings.Repeat("é", 10<<20) // 20 MiB of two-byte runes
	got := b.field(huge)
	if len(got) > maxResultTextBytes+1 {
		t.Errorf("field not clipped: %d bytes", len(got))
	}
	if !strings.HasPrefix(huge, got) && got != "" {
		t.Error("clipped field is not a prefix")
	}
	if clipUTF8("日本語", 4) != "日" {
		t.Errorf("clipUTF8 split a rune: %q", clipUTF8("日本語", 4))
	}
	b.write(strings.Repeat("a", maxResultTextBytes-10))
	if got := b.field(strings.Repeat("b", 1000)); len(got) > 11 {
		t.Errorf("field ignores remaining budget: %d", len(got))
	}
}

func TestListResourcesHugeDescription(t *testing.T) {
	m, _ := startFake(t, map[string]string{"FAKE_HUGE_DESC": "5242880"}, nil) // 5 MiB
	text := resultText(execTool(t, findTool(t, m, ListResourcesToolName), `{}`))
	if len(text) > maxResultTextBytes+len("\n... [truncated at 51200 bytes]") {
		t.Errorf("output %d bytes exceeds the budget", len(text))
	}
	if !strings.Contains(text, "mem://huge") || !strings.Contains(text, "[truncated") {
		t.Errorf("unexpected output head: %.100q", text)
	}
}

// ---- readStderr bookkeeping ----

func TestReadStderrHandlesEOFWithoutNewline(t *testing.T) {
	tr := newTestStderrTransport(ServerConfig{})
	tr.readStderr(io.MultiReader(strings.NewReader("one\ntwo"), strings.NewReader("")))
	if got := tr.stderrTail(); got != "one\ntwo" {
		t.Errorf("tail = %q", got)
	}
}
