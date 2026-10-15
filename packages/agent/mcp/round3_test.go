package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the third review round (platform-independent parts).

// ---- GO-004: HTTP error bodies ----

func TestHTTPErrorBodySecretStraddlingDisplayCut(t *testing.T) {
	// End to end: secrets straddling the display cut and split by escapes.
	const secret = "HTTPSECRET-0123456789abcdef"
	t.Setenv("MCP_T_BEARER", secret)
	// The secret straddles the 300-byte display cut, and a second copy is
	// split by escape sequences so it only matches after stripping.
	body := strings.Repeat("x", maxStatusBodyDisplay-len(secret)/2) + secret + strings.Repeat("y", 2000) +
		"HTTPSE\x1b[0mCRET-0123456789abcdef"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, body, http.StatusInternalServerError)
	}))
	defer srv.Close()
	cfg := ServerConfig{Type: "http", URL: srv.URL, BearerTokenEnvVar: "MCP_T_BEARER"}
	tr, err := newHTTPTransport("x", cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	if got := tr.red.overlap(); got < len(secret) {
		t.Fatalf("overlap %d does not cover the secret length %d", got, len(secret))
	}
	_, err = tr.request(context.Background(), 1, "tools/list", nil)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, frag := range []string{secret, "HTTPSECR", "HTTPS", "SECRET"} {
		if strings.Contains(err.Error(), frag) {
			t.Errorf("secret fragment %q leaked: %q", frag, err)
		}
	}
}

func TestStatusErrorBodyTrimsPartialSecrets(t *testing.T) {
	red := newRedactor(ServerConfig{Env: map[string]string{"K": "TOPSECRET-99"}})
	// Body cut by the reader in the middle of the secret.
	err := red.statusErrorBody(500, []byte("failed with token TOPSECR"), true)
	if strings.Contains(err.Error(), "TOP") {
		t.Errorf("reader-cut prefix survived: %q", err)
	}
	// The same text, complete: a fragment that is not the secret stays.
	err = red.statusErrorBody(500, []byte("failed with token TOPSECR"), false)
	if !strings.Contains(err.Error(), "TOPSECR") {
		t.Errorf("complete bodies must not be trimmed: %q", err)
	}
	// A secret hidden by control sequences is matched after stripping, so the
	// 300-byte display cut never lands inside it. (The trim after that cut is
	// defense in depth: redaction runs first, so a whole secret is already the
	// marker when the cut happens and no behavioural input reaches the trim.)
	long := strings.Repeat("a", 296) + "TOPS" + "\x1b[0mECRET-99" + strings.Repeat("b", 50)
	err = red.statusErrorBody(500, []byte(long), false)
	if strings.Contains(err.Error(), "TOPS") || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("display cut exposed a secret prefix: %q", err)
	}
	if !strings.Contains(err.Error(), "…") {
		t.Errorf("long body not marked cut: %q", err)
	}
	if n := len(err.(*HTTPStatusError).Body); n > maxStatusBodyDisplay+len("…") {
		t.Errorf("body not bounded: %d", n)
	}
}

// ---- GO-021: writer-side withdrawal ----

// lateCtx reports an error from Err before Done closes, which lets a test
// order the writer's and the caller's observations of the cancellation.
type lateCtx struct {
	context.Context
	done chan struct{}
	err  atomic.Bool
}

func newLateCtx() *lateCtx { return &lateCtx{Context: context.Background(), done: make(chan struct{})} }

func (c *lateCtx) Done() <-chan struct{} { return c.done }
func (c *lateCtx) Err() error {
	if c.err.Load() {
		return context.Canceled
	}
	return nil
}

func TestWriteFrameWriterSideSkipDoesNotFailTransport(t *testing.T) {
	c, _ := connectFake(t, nil)
	st := c.t.(*stdioTransport)
	ctx := newLateCtx()
	ctx.err.Store(true) // the writer will see an ended context at dequeue...

	skipped := make(chan struct{})
	release := make(chan struct{})
	st.hookSkipped = func() {
		close(ctx.done) // ...and only now does the caller learn of it
		close(skipped)
		<-release // keep the writer from reporting, so the caller takes the Done branch
	}
	type result struct {
		sent bool
		err  error
	}
	res := make(chan result, 1)
	go func() {
		sent, err := st.writeFrame(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/x"}`))
		res <- result{sent, err}
	}()
	select {
	case <-skipped:
	case <-time.After(5 * time.Second):
		t.Fatal("writer never skipped the frame")
	}
	var r result
	select {
	case r = <-res:
	case <-time.After(5 * time.Second):
		t.Fatal("caller did not return")
	}
	close(release)
	if r.sent || !errors.Is(r.err, context.Canceled) {
		t.Errorf("sent=%v err=%v, want withdrawn", r.sent, r.err)
	}
	select {
	case <-st.done():
		t.Fatalf("a frame withdrawn by the writer failed the transport: %v", st.failure())
	default:
	}
	// The transport still works.
	if got := callText(t, c, "echo", `{"text":"ok"}`); got != "echo: ok" {
		t.Errorf("after skip: %q", got)
	}
}

// ---- GO-022: budget marker after rune rounding ----

func TestBudgetedFieldMarksTruncationAfterRuneRounding(t *testing.T) {
	b := &budgetedText{}
	b.write(strings.Repeat("a", maxResultTextBytes-4)) // remaining = 4
	// remaining+1 = 5 bytes lands inside the second 3-byte rune; rounding
	// down gives 3 bytes, which is within budget, yet text was dropped.
	got := b.field("日日日日")
	if got != "日" {
		t.Fatalf("field = %q", got)
	}
	if !b.clipped {
		t.Fatal("clipping not recorded")
	}
	b.write("- " + got)
	if !b.full() {
		t.Error("truncation flag not set although text was clipped")
	}
	if !strings.HasSuffix(b.String(), "[truncated at 51200 bytes]") {
		t.Errorf("marker missing: %q", b.String()[len(b.String())-40:])
	}
	// Later writes are ignored.
	before := b.String()
	b.write("more")
	if b.String() != before {
		t.Error("write accepted after truncation")
	}
	// A field that fits is neither clipped nor marked.
	c := &budgetedText{}
	if got := c.field("日本"); got != "日本" || c.clipped {
		t.Errorf("field = %q clipped=%v", got, c.clipped)
	}
	c.write("- 日本\n")
	if c.full() || strings.Contains(c.String(), "truncated") {
		t.Error("unclipped field must not be marked")
	}
}
