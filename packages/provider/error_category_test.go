package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestErrorCategory(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"transcribe_401", &TranscribeHTTPError{StatusCode: 401}, ErrorCategoryAuth},
		{"transient_503_wrapped", fmt.Errorf("x: %w", &transientHTTPError{Status: 503, Body: "secret"}), ErrorCategoryProviderUnavailable},
		{"typed_418", &TranscribeHTTPError{StatusCode: 418}, ErrorCategoryOther},
		{"deadline", fmt.Errorf("stream: %w", context.DeadlineExceeded), ErrorCategoryTimeout},
		{"net_timeout", &net.OpError{Op: "dial", Err: timeoutErr{}}, ErrorCategoryTimeout},
		{"net_refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, ErrorCategoryNetwork},
		{"dns", &net.DNSError{Err: "no such host", Name: "x"}, ErrorCategoryNetwork},
		{"eof", fmt.Errorf("read: %w", io.EOF), ErrorCategoryNetwork},
		{"unexpected_eof", io.ErrUnexpectedEOF, ErrorCategoryNetwork},
		{"anthropic_429", errors.New("anthropic: http 429: slow down"), ErrorCategoryRateLimit},
		{"openai_401", errors.New("openai: http 401: bad key sk-x"), ErrorCategoryAuth},
		{"bedrock_403", errors.New("bedrock: http 403: denied"), ErrorCategoryPermission},
		{"gemini_502", errors.New("google: http 502: bad gateway"), ErrorCategoryProviderUnavailable},
		{"overflow_413", errors.New("anthropic: http 413: too large"), ErrorCategoryContextOverflow},
		{"overflow_text", errors.New("maximum context length is 8k"), ErrorCategoryContextOverflow},
		{"phrase_unavailable", errors.New("Service Unavailable"), ErrorCategoryProviderUnavailable},
		// Broad words no longer misclassify unrelated errors.
		{"word_eof_in_text", errors.New("unexpected token near geofence"), ErrorCategoryOther},
		{"word_quota", errors.New("disk quota exceeded"), ErrorCategoryOther},
		{"word_forbidden", errors.New("forbidden path"), ErrorCategoryOther},
		{"word_permission", errors.New("open /x: permission denied"), ErrorCategoryOther},
		{"bare_number", errors.New("tool returned 429 rows"), ErrorCategoryOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorCategory(tc.err); got != tc.want {
				t.Fatalf("ErrorCategory(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
