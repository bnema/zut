package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// requestMaxRetries is the maximum number of retries for transient connect or
// HTTP failures before a streaming response opens. It matches Codex's default
// request retry guard and remains separate from agent-level stream reconnects.
const requestMaxRetries = 4

// maxServerRetryDelay caps how long we are willing to honor a
// server-requested Retry-After delay. When a provider asks for a
// longer wait (e.g. a quota that resets in hours), the request fails
// immediately with an informative error instead of silently blocking.
const maxServerRetryDelay = 60 * time.Second

// streamRetryBackoff returns the wait duration before retry attempt n
// (1-based). Short, fixed backoff: 250ms, then 750ms for later retries.
// Anything longer would feel like the agent froze; anything shorter starts
// hammering the provider when it's actually struggling.
func streamRetryBackoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 250 * time.Millisecond
	default:
		return 750 * time.Millisecond
	}
}

// doStreamWithRetry performs an HTTP request that begins a streaming
// response, with up to requestMaxRetries silent retries for transient
// connect failures. Successful responses (status 200) and non-transient
// failures (4xx, malformed bodies, etc.) are returned immediately.
//
// Retries fire on:
//   - net.Conn dial errors (connection refused, no such host, EOF
//     before headers, "upstream connect error", "connection reset")
//   - HTTP 429/500/502/503/504/524/529, except 429 responses whose
//     body indicates a terminal quota/billing limit
//
// When a retryable response carries a Retry-After (or retry-after-ms)
// header, the wait before the next attempt uses the server-requested
// delay instead of the fixed backoff, capped by maxServerRetryDelay.
//
// The handler is responsible for re-reading the request body each
// attempt; we re-create the request via newReq() because http.Request
// bodies are single-use. Callers in this package always Marshal a JSON
// body up-front, so newReq just rebuilds with bytes.NewReader on the
// captured payload.
//
// If every attempt fails, the last error/response is returned so the
// caller can format a normal error message. On context cancellation
// the loop bails out immediately with ctx.Err().
func doStreamWithRetry(ctx context.Context, client *http.Client, newReq func() (*http.Request, error), lifecycle RequestLifecycle) (*http.Response, error) {
	var lastErr error
	serverDelay := time.Duration(-1) // <0: use the default fixed backoff
	maxAttempts := requestMaxRetries + 1
	for attempt := 0; attempt <= requestMaxRetries; attempt++ {
		if attempt > 0 {
			delay := streamRetryBackoff(attempt)
			if serverDelay >= 0 {
				delay = serverDelay
			}
			serverDelay = -1
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if lifecycle != nil {
				lifecycle.RetryScheduled(attempt+1, maxAttempts, delay)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := newReq()
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reportRequestAttempt(lifecycle, attempt+1, maxAttempts)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, err
			}
			transient := isTransientConnectError(err)
			reportRequestFailure(lifecycle, attempt+1, maxAttempts, requestFailureReasonForError(err), !transient || attempt == requestMaxRetries)
			if !transient {
				return nil, err
			}
			continue
		}
		if isTransientHTTPStatus(resp.StatusCode) {
			// Drain a bit of the body so we can include it in the
			// final error if every retry exhausts. We cap the read
			// at 4 KiB because edge proxies sometimes send pages.
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			trimmed := strings.TrimSpace(string(body))
			reason := requestFailureReasonForHTTP(resp.StatusCode, trimmed)
			if resp.StatusCode == http.StatusTooManyRequests && isTerminalRateLimitBody(trimmed) {
				// Subscription/quota/billing limits are not transient
				// throttles; retrying only burns time. Surface the
				// response immediately so the caller formats the
				// normal "http 429: body" error.
				reportRequestFailure(lifecycle, attempt+1, maxAttempts, RequestFailureQuota, true)
				return synthesizeResponse(resp.StatusCode, body), nil
			}
			lastErr = &transientHTTPError{Status: resp.StatusCode, Body: trimmed}
			if attempt == requestMaxRetries {
				reportRequestFailure(lifecycle, attempt+1, maxAttempts, reason, true)
				// Wrap as a real *http.Response shape the caller
				// expects so it formats the error identically to a
				// non-retried failure.
				return synthesizeResponse(resp.StatusCode, body), nil
			}
			if d, ok := retryAfterDelay(resp.Header); ok {
				if d > maxServerRetryDelay {
					reportRequestFailure(lifecycle, attempt+1, maxAttempts, reason, true)
					return nil, fmt.Errorf("server requested %s retry delay (max %s): http %d: %s",
						d.Round(time.Second), maxServerRetryDelay, resp.StatusCode, trimmed)
				}
				serverDelay = d
			}
			reportRequestFailure(lifecycle, attempt+1, maxAttempts, reason, false)
			continue
		}
		if resp.StatusCode >= http.StatusBadRequest {
			reportRequestFailure(lifecycle, attempt+1, maxAttempts, requestFailureReasonForHTTP(resp.StatusCode, ""), true)
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = errors.New("retry loop exhausted")
	}
	return nil, lastErr
}

func reportRequestAttempt(lifecycle RequestLifecycle, attempt, maxAttempts int) {
	if lifecycle == nil {
		return
	}
	lifecycle.RequestAttempt(attempt, maxAttempts)
	if withID, ok := lifecycle.(RequestAttemptIDLifecycle); ok {
		withID.RequestAttemptID(uuid.NewString(), attempt, maxAttempts)
	}
}

// transientHTTPError is the placeholder error returned while we're
// still inside the retry loop. Callers never observe it directly;
// the loop converts the final exhausted attempt into a synthesized
// http.Response so the caller's existing "http NNN: body" formatting
// keeps working unchanged.
type transientHTTPError struct {
	Status int
	Body   string
}

func (e *transientHTTPError) Error() string { return "transient http error" }

func reportRequestFailure(lifecycle RequestLifecycle, attempt, maxAttempts int, reason RequestFailureReason, terminal bool) {
	if observer, ok := lifecycle.(RequestFailureLifecycle); ok {
		observer.RequestFailed(attempt, maxAttempts, reason, terminal)
	}
}

func requestFailureReasonForHTTP(status int, body string) RequestFailureReason {
	if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		if status == http.StatusTooManyRequests {
			return RequestFailureRateLimit
		}
		return RequestFailureClient
	}
	msg := strings.ToLower(body)
	if status == 529 || strings.Contains(msg, "overload") {
		return RequestFailureOverload
	}
	return RequestFailureServer
}

func requestFailureReasonForError(err error) RequestFailureReason {
	if err == nil {
		return RequestFailureUnknown
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "timeout") {
		return RequestFailureTimeout
	}
	if isTransientConnectError(err) {
		return RequestFailureNetwork
	}
	return RequestFailureUnknown
}

// synthesizeResponse rebuilds a closable response wrapping the captured
// body bytes so the caller's existing non-200 handling path keeps
// working without needing to know retries happened.
func synthesizeResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

// isTransientConnectError reports whether err looks like a transient
// network failure that's worth retrying. The classic "upstream connect
// error or disconnect/reset before headers" from edge proxies (Envoy,
// Cloudflare, GFE) shows up as several different concrete error types
// across HTTP/1, HTTP/2, and TLS handshakes — easier to match by
// substring on the rendered message than to enumerate every type.
func isTransientConnectError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "unexpected eof") ||
		strings.Contains(msg, "eof") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "tls handshake") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "transport failure") ||
		strings.Contains(msg, "upstream connect error") ||
		strings.Contains(msg, "disconnect/reset before headers")
}

// isTransientHTTPStatus reports whether a non-200 status code is
// likely transient. 429 covers rate limits and capacity throttles
// (terminal quota/billing 429s are filtered by isTerminalRateLimitBody
// in the retry loop). 500 shows up for transient backend failures on
// several providers, 524 is Cloudflare's origin timeout, and 529 is
// the overloaded status used by Anthropic-style backends.
func isTransientHTTPStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		524, // Cloudflare: a timeout occurred
		529: // overloaded
		return true
	}
	return false
}

// isTerminalRateLimitBody reports whether a 429 body describes a
// subscription, quota, or billing limit rather than a transient
// throttle. Retrying those only delays the inevitable error, and on
// some backends it can even extend the block.
func isTerminalRateLimitBody(body string) bool {
	msg := strings.ToLower(body)
	for _, needle := range []string{
		"gousagelimiterror", "freeusagelimiterror",
		"usage limit", "monthly usage limit",
		"available balance", "insufficient_quota",
		"out of budget", "quota exceeded", "billing",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// retryAfterDelay extracts a server-requested retry delay from
// response headers. "retry-after-ms" (milliseconds) wins over the
// standard "Retry-After", which may hold either delay-seconds or an
// HTTP-date. Returns false when no parseable delay is present.
func retryAfterDelay(h http.Header) (time.Duration, bool) {
	if v := h.Get("retry-after-ms"); v != "" {
		if ms, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			if ms < 0 {
				ms = 0
			}
			return time.Duration(ms * float64(time.Millisecond)), true
		}
	}
	v := h.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	v = strings.TrimSpace(v)
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs < 0 {
			secs = 0
		}
		return time.Duration(secs * float64(time.Second)), true
	}
	if when, err := http.ParseTime(v); err == nil {
		d := time.Until(when)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}
