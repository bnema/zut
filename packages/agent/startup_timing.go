package agent

import (
	"log/slog"
	"time"
)

// startupTimer logs one DEBUG line per startup phase with its duration. A
// nil timer or nil logger is a no-op, so non-daemon paths stay silent.
type startupTimer struct {
	log  *slog.Logger
	last time.Time
}

func newStartupTimer(log *slog.Logger) *startupTimer {
	if log == nil {
		return nil
	}
	return &startupTimer{log: log, last: time.Now()}
}

// phase logs the time since the previous phase (or creation) and restarts
// the clock.
func (t *startupTimer) phase(name string, attrs ...any) {
	if t == nil {
		return
	}
	now := time.Now()
	t.log.Debug("session phase", append([]any{"phase", name, "duration", now.Sub(t.last)}, attrs...)...)
	t.last = now
}

// item logs a per-item duration (MCP server, extension) measured elsewhere.
func (t *startupTimer) item(kind, name string, d time.Duration, err error, attrs ...any) {
	if t == nil {
		return
	}
	attrs = append([]any{"kind", kind, "name", name, "duration", d}, attrs...)
	if err != nil {
		attrs = append(attrs, "failed", true)
	}
	t.log.Debug("session item", attrs...)
}

// reset restarts the clock without logging, to exclude unrelated work.
func (t *startupTimer) reset() {
	if t != nil {
		t.last = time.Now()
	}
}
