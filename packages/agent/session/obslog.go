package session

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// Session logging records only IDs, kinds, durations and error categories:
// never prompts, message text, tool arguments, notes or raw error strings,
// which can echo any of them.

func (c *Controller) logger() *slog.Logger {
	if c.deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.deps.Logger
}

// errorCategory maps an error to a fixed, content-free label.
func errorCategory(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, core.ErrRepetitiveLoop) {
		return "repetitive_loop"
	}
	if provider.IsContextOverflowError(err) {
		return "context_overflow"
	}
	low := strings.ToLower(err.Error())
	for _, c := range []struct {
		name    string
		needles []string
	}{
		{"network", []string{"timeout", "deadline exceeded", "connection refused", "connection reset", "no such host", "tls handshake", "eof"}},
		{"auth", []string{"http 401", " 401:", "invalid_authentication", "token expired", "api key appears to be invalid"}},
		{"permission", []string{"http 403", " 403:", "permission denied", "forbidden"}},
		{"rate_limit", []string{"http 429", " 429:", "rate limit", "rate_limit", "too many requests", "quota"}},
		{"provider_unavailable", []string{"http 500", "http 502", "http 503", "http 504", " 500:", " 502:", " 503:", " 504:", "upstream connect error", "service unavailable", "internal server error", "bad gateway", "gateway timeout"}},
	} {
		for _, n := range c.needles {
			if strings.Contains(low, n) {
				return c.name
			}
		}
	}
	return "other"
}
