package tui

import "time"

// StringSpinner selects one text frame for each configured interval. It owns
// no ticker or goroutine; callers render it from their existing UI clock.
type StringSpinner struct {
	frames   []string
	interval time.Duration
}

// NewStringSpinner creates a reusable text animation. Empty frames render an
// empty string, and a non-positive interval keeps the first frame stable.
func NewStringSpinner(frames []string, interval time.Duration) StringSpinner {
	return StringSpinner{
		frames:   append([]string(nil), frames...),
		interval: interval,
	}
}

// FrameAt returns the frame for now relative to startedAt. Times before the
// start clamp to the first frame.
func (s StringSpinner) FrameAt(startedAt, now time.Time) string {
	if len(s.frames) == 0 {
		return ""
	}
	if s.interval <= 0 || startedAt.IsZero() || now.IsZero() || now.Before(startedAt) {
		return s.frames[0]
	}
	index := int(now.Sub(startedAt)/s.interval) % len(s.frames)
	return s.frames[index]
}
