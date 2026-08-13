package provider

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

// TestOpenAIStreamRetriesOn503 verifies the OpenAI/Kimi stream client
// silently retries when the upstream returns 503 a couple of times,
// then succeeds. The user-visible error path should NOT fire because
// a later attempt landed on a 200.
func TestOpenAIStreamRetriesOn503(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 5 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("upstream connect error"))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = w.Write([]byte(s))
			if fl != nil {
				fl.Flush()
			}
		}
		write("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		write("data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewOpenAI("x", srv.URL)
	lifecycle := &recordingRequestLifecycle{}
	evs, err := c.Stream(context.Background(), Request{Model: "gpt-5", Lifecycle: lifecycle})
	if err != nil {
		t.Fatalf("stream returned error: %v", err)
	}
	gotText := ""
	for ev := range evs {
		if td, ok := ev.(EventTextDelta); ok {
			gotText += td.Delta
		}
	}
	if gotText != "ok" {
		t.Fatalf("text=%q", gotText)
	}
	if got := attempts.Load(); got != 5 {
		t.Fatalf("attempts=%d want 5", got)
	}
	if got := lifecycle.attempts; len(got) != 5 || got[0] != (lifecycleAttempt{attempt: 1, max: 5}) || got[4] != (lifecycleAttempt{attempt: 5, max: 5}) {
		t.Fatalf("lifecycle attempts = %#v, want 1 through 5", got)
	}
	if got := lifecycle.retries; len(got) != 4 || got[0].attempt != 2 || got[3].attempt != 5 {
		t.Fatalf("lifecycle retries = %#v, want retries before attempts 2 through 5", got)
	}
}

// TestOpenAIStreamSurfaces503AfterRetriesExhausted verifies that when
// every retry attempt returns 503 the caller sees a normal "http 503"
// error, not a retry-internal placeholder. The body bytes must be
// preserved so the rescue picker / red banner can show them.
func TestOpenAIStreamSurfaces503AfterRetriesExhausted(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("upstream connect error"))
	}))
	defer srv.Close()

	c := NewOpenAI("x", srv.URL)
	_, err := c.Stream(context.Background(), Request{Model: "gpt-5"})
	if err == nil {
		t.Fatalf("expected error after exhausted retries")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("error %q should mention 503", err)
	}
	if got := attempts.Load(); got != requestMaxRetries+1 {
		t.Fatalf("attempts=%d want %d", got, requestMaxRetries+1)
	}
}

// TestOpenAIStreamDoesNotRetryOn400 ensures non-transient errors are
// returned immediately without retrying. We don't want to amplify
// load on a provider that's actively rejecting our request shape.
func TestOpenAIStreamDoesNotRetryOn400(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
	}))
	defer srv.Close()

	c := NewOpenAI("x", srv.URL)
	_, err := c.Stream(context.Background(), Request{Model: "gpt-5"})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("want 400 err got %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts=%d want 1", got)
	}
}

func TestIsTransientHTTPStatus(t *testing.T) {
	for _, code := range []int{429, 500, 502, 503, 504, 524, 529} {
		if !isTransientHTTPStatus(code) {
			t.Errorf("%d should be transient", code)
		}
	}
	for _, code := range []int{200, 400, 401, 403, 404, 501} {
		if isTransientHTTPStatus(code) {
			t.Errorf("%d should NOT be transient", code)
		}
	}
}

// TestOpenAIStreamRetriesOn429WithRetryAfter verifies that a transient
// 429 is retried and that a server-requested Retry-After delay is
// honored instead of the fixed backoff.
func TestOpenAIStreamRetriesOn429WithRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	var firstRetryAt atomic.Int64
	start := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.Header().Set("retry-after-ms", "50")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("Our servers are currently overloaded. Please try again later."))
			return
		}
		firstRetryAt.Store(int64(time.Since(start)))
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = w.Write([]byte(s))
			if fl != nil {
				fl.Flush()
			}
		}
		write("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		write("data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewOpenAI("x", srv.URL)
	evs, err := c.Stream(context.Background(), Request{Model: "gpt-5"})
	if err != nil {
		t.Fatalf("stream returned error: %v", err)
	}
	for range evs {
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts=%d want 2", got)
	}
	if got := time.Duration(firstRetryAt.Load()); got < 50*time.Millisecond {
		t.Fatalf("retry fired after %v; want >= 50ms (server-requested delay)", got)
	}
}

// TestOpenAIStreamDoesNotRetryTerminal429 ensures quota/billing 429s
// are surfaced immediately without retrying: a usage-limit block will
// not clear within the retry window, and hammering the endpoint can
// extend the block on some backends.
func TestOpenAIStreamDoesNotRetryTerminal429(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"insufficient_quota","message":"You exceeded your current quota"}}`))
	}))
	defer srv.Close()

	c := NewOpenAI("x", srv.URL)
	_, err := c.Stream(context.Background(), Request{Model: "gpt-5"})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("want 429 err got %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts=%d want 1", got)
	}
}

// TestOpenAIStreamRejectsExcessiveRetryAfter verifies that a
// server-requested delay beyond maxServerRetryDelay fails immediately
// with an informative error instead of blocking silently.
func TestOpenAIStreamRejectsExcessiveRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "18000") // 5 hours
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer srv.Close()

	c := NewOpenAI("x", srv.URL)
	_, err := c.Stream(context.Background(), Request{Model: "gpt-5"})
	if err == nil || !strings.Contains(err.Error(), "retry delay") {
		t.Fatalf("want retry-delay err got %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts=%d want 1", got)
	}
}

type lifecycleAttempt struct {
	attempt int
	max     int
}

type lifecycleRetry struct {
	attempt int
	max     int
	delay   time.Duration
}

type lifecycleFailure struct {
	attempt  int
	max      int
	reason   RequestFailureReason
	terminal bool
}

type recordingRequestLifecycle struct {
	attempts []lifecycleAttempt
	ids      []string
	retries  []lifecycleRetry
	failures []lifecycleFailure
}

func (r *recordingRequestLifecycle) RequestAttempt(attempt, maxAttempts int) {
	r.attempts = append(r.attempts, lifecycleAttempt{attempt: attempt, max: maxAttempts})
}

func (r *recordingRequestLifecycle) RequestAttemptID(requestID string, attempt, maxAttempts int) {
	r.ids = append(r.ids, requestID)
}

func (r *recordingRequestLifecycle) RetryScheduled(attempt, maxAttempts int, delay time.Duration) {
	r.retries = append(r.retries, lifecycleRetry{attempt: attempt, max: maxAttempts, delay: delay})
}

func (r *recordingRequestLifecycle) RequestFailed(attempt, maxAttempts int, reason RequestFailureReason, terminal bool) {
	r.failures = append(r.failures, lifecycleFailure{attempt: attempt, max: maxAttempts, reason: reason, terminal: terminal})
}

func TestDoStreamWithRetryReportsLifecycle(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("servers overloaded: private provider detail"))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	lifecycle := &recordingRequestLifecycle{}
	resp, err := doStreamWithRetry(context.Background(), srv.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, nil)
	}, lifecycle)
	if err != nil {
		t.Fatalf("doStreamWithRetry: %v", err)
	}
	resp.Body.Close()

	maxAttempts := requestMaxRetries + 1
	if got := lifecycle.attempts; len(got) != 2 || got[0] != (lifecycleAttempt{attempt: 1, max: maxAttempts}) || got[1] != (lifecycleAttempt{attempt: 2, max: maxAttempts}) {
		t.Fatalf("attempts = %#v, want attempts 1 and 2 of %d", got, maxAttempts)
	}
	if len(lifecycle.ids) != 2 || lifecycle.ids[0] == "" || lifecycle.ids[0] == lifecycle.ids[1] {
		t.Fatalf("request IDs = %#v, want distinct provider-local IDs per attempt", lifecycle.ids)
	}
	if got := lifecycle.retries; len(got) != 1 || got[0].attempt != 2 || got[0].max != maxAttempts || got[0].delay != 250*time.Millisecond {
		t.Fatalf("retries = %#v, want attempt 2 of %d after 250ms", got, maxAttempts)
	}
	if got := lifecycle.failures; len(got) != 1 || got[0] != (lifecycleFailure{attempt: 1, max: maxAttempts, reason: RequestFailureOverload}) {
		t.Fatalf("failures = %#v, want one non-terminal overload", got)
	}
}

func TestDoStreamWithRetryReportsTerminalClientFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("servers overloaded: private provider detail"))
	}))
	defer srv.Close()

	lifecycle := &recordingRequestLifecycle{}
	resp, err := doStreamWithRetry(context.Background(), srv.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, nil)
	}, lifecycle)
	if err != nil {
		t.Fatalf("doStreamWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := lifecycle.failures; len(got) != 1 || got[0] != (lifecycleFailure{attempt: 1, max: requestMaxRetries + 1, reason: RequestFailureClient, terminal: true}) {
		t.Fatalf("failures = %#v, want one terminal client failure", got)
	}
	if len(lifecycle.retries) != 0 {
		t.Fatalf("retries = %#v, want none", lifecycle.retries)
	}
}

func TestDoStreamWithRetryReportsTerminalQuota(t *testing.T) {
	const privateBody = `{"error":{"type":"insufficient_quota","message":"private provider detail"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(privateBody))
	}))
	defer srv.Close()

	lifecycle := &recordingRequestLifecycle{}
	resp, err := doStreamWithRetry(context.Background(), srv.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, nil)
	}, lifecycle)
	if err != nil {
		t.Fatalf("doStreamWithRetry: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := lifecycle.failures; len(got) != 1 || got[0] != (lifecycleFailure{attempt: 1, max: requestMaxRetries + 1, reason: RequestFailureQuota, terminal: true}) {
		t.Fatalf("failures = %#v, want one terminal quota failure", got)
	}
}

func TestDoStreamWithRetryStopsLifecycleAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lifecycle := &recordingRequestLifecycle{}
	_, err := doStreamWithRetry(ctx, http.DefaultClient, func() (*http.Request, error) {
		t.Fatal("new request called after cancellation")
		return nil, nil
	}, lifecycle)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if len(lifecycle.attempts) != 0 || len(lifecycle.retries) != 0 {
		t.Fatalf("lifecycle after cancellation = %#v %#v", lifecycle.attempts, lifecycle.retries)
	}
}

func TestRetryAfterDelay(t *testing.T) {
	mk := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}
	if d, ok := retryAfterDelay(mk("retry-after-ms", "1500")); !ok || d != 1500*time.Millisecond {
		t.Errorf("retry-after-ms: got %v %v", d, ok)
	}
	if d, ok := retryAfterDelay(mk("Retry-After", "2")); !ok || d != 2*time.Second {
		t.Errorf("seconds: got %v %v", d, ok)
	}
	// retry-after-ms takes precedence over Retry-After.
	if d, ok := retryAfterDelay(mk("retry-after-ms", "100", "Retry-After", "30")); !ok || d != 100*time.Millisecond {
		t.Errorf("precedence: got %v %v", d, ok)
	}
	// HTTP-date in the past clamps to zero.
	if d, ok := retryAfterDelay(mk("Retry-After", "Mon, 02 Jan 2006 15:04:05 GMT")); !ok || d != 0 {
		t.Errorf("past date: got %v %v", d, ok)
	}
	if _, ok := retryAfterDelay(mk()); ok {
		t.Error("empty headers should not produce a delay")
	}
	if _, ok := retryAfterDelay(mk("Retry-After", "soon")); ok {
		t.Error("garbage Retry-After should not produce a delay")
	}
}

func TestIsTerminalRateLimitBody(t *testing.T) {
	terminal := []string{
		`{"error":{"type":"insufficient_quota"}}`,
		"Monthly usage limit reached",
		"you have hit your usage limit",
		"quota exceeded",
		"billing hard limit",
	}
	for _, b := range terminal {
		if !isTerminalRateLimitBody(b) {
			t.Errorf("%q should be terminal", b)
		}
	}
	transient := []string{
		"Our servers are currently overloaded. Please try again later.",
		"Rate limit reached, retry shortly",
		"",
	}
	for _, b := range transient {
		if isTerminalRateLimitBody(b) {
			t.Errorf("%q should NOT be terminal", b)
		}
	}
}

func TestIsTransientConnectError(t *testing.T) {
	good := []string{
		"read tcp: connection reset by peer",
		"dial tcp: connection refused",
		"unexpected EOF",
		"upstream connect error or disconnect/reset before headers",
		"tls handshake error: bad cert",
		"i/o timeout",
	}
	for _, m := range good {
		if !isTransientConnectError(stringErr(m)) {
			t.Errorf("%q should be transient", m)
		}
	}
	bad := []string{
		"context canceled",
		"json: cannot unmarshal",
	}
	for _, m := range bad {
		if isTransientConnectError(stringErr(m)) {
			t.Errorf("%q should NOT be transient", m)
		}
	}
}

type stringErr string

func (s stringErr) Error() string { return string(s) }

func TestDoStreamWithRetryDoesNotReportAttemptWhenRequestCreationFails(t *testing.T) {
	lifecycle := &recordingRequestLifecycle{}
	_, err := doStreamWithRetry(context.Background(), http.DefaultClient, func() (*http.Request, error) {
		return nil, errors.New("cannot build request")
	}, lifecycle)
	if err == nil || !strings.Contains(err.Error(), "cannot build request") {
		t.Fatalf("error = %v, want request creation failure", err)
	}
	if len(lifecycle.attempts) != 0 {
		t.Fatalf("request attempts = %#v, want none", lifecycle.attempts)
	}
}

type retryRoundTripper func(*http.Request) (*http.Response, error)

func (f retryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestDoStreamWithRetryDoesNotReportCancelledRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &http.Client{Transport: retryRoundTripper(func(_ *http.Request) (*http.Response, error) {
		cancel()
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     make(http.Header),
			Body:       http.NoBody,
		}, nil
	})}
	lifecycle := &recordingRequestLifecycle{}
	_, err := doStreamWithRetry(ctx, client, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", nil)
	}, lifecycle)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if len(lifecycle.retries) != 0 {
		t.Fatalf("retry lifecycle = %#v, want none after cancellation", lifecycle.retries)
	}
}
