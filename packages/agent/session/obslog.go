package session

import (
	"context"
	"errors"
	"log/slog"

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
	return provider.ErrorCategory(err)
}

// logOrigin maps an input origin to a known constant; anything else logs as
// "other" so client-supplied strings never reach the log.
func logOrigin(o InputOrigin) string {
	switch o {
	case "":
		return "none"
	case InputOriginUser, InputOriginScheduler, InputOriginGoal, InputOriginExtension, InputOriginResident, InputOriginInterSession, InputOriginTelegram:
		return string(o)
	}
	return "other"
}
