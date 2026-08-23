package modes

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/bnema/zut/packages/tui"
)

const (
	themeWatchInterval = 500 * time.Millisecond
	themeMissingGrace  = 2 * time.Second
)

type themeWatchEvent struct {
	path    string
	source  *tui.ThemeSource
	err     error
	deleted bool
}

func (i *Interactive) restartThemeWatch() {
	if i.themeWatchCancel != nil {
		i.themeWatchCancel()
		i.themeWatchCancel = nil
	}
	if i.runCtx == nil || i.activeThemeSource == nil {
		return
	}
	ctx, cancel := context.WithCancel(i.runCtx)
	i.themeWatchCancel = cancel
	interval := i.cfg.ThemeWatchInterval
	if interval <= 0 {
		interval = themeWatchInterval
	}
	go watchThemeSource(ctx, i.cfg.ZutHome, i.cfg.EffectiveThemeName, i.activeThemeSource, interval, i.themeWatchEvents)
}

func (i *Interactive) stopThemeWatch() {
	if i.themeWatchCancel != nil {
		i.themeWatchCancel()
		i.themeWatchCancel = nil
	}
}

func watchThemeSource(ctx context.Context, home, preference string, accepted *tui.ThemeSource, interval time.Duration, events chan<- themeWatchEvent) {
	if accepted == nil || accepted.Path == "" {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	watchThemeSourceTicks(ctx, home, preference, accepted, ticker.C, events)
}

// watchThemeSourceTicks is the test seam for polling: tests advance explicit
// timestamps rather than synchronizing with sleeps.
func watchThemeSourceTicks(ctx context.Context, home, preference string, accepted *tui.ThemeSource, ticks <-chan time.Time, events chan<- themeWatchEvent) {
	if accepted == nil || accepted.Path == "" {
		return
	}
	lastDigest := accepted.Digest
	var missingSince time.Time
	lastError := ""
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticks:
			if _, statErr := os.Stat(accepted.Path); errors.Is(statErr, os.ErrNotExist) {
				if missingSince.IsZero() {
					missingSince = now
				}
				if now.Sub(missingSince) >= themeMissingGrace {
					sendThemeWatchEvent(ctx, events, themeWatchEvent{path: accepted.Path, deleted: true})
					return
				}
				continue
			}
			source, err := tui.LoadThemeSource(home, preference)
			if err == nil && source != nil {
				missingSince = time.Time{}
				lastError = ""
				if source.Path != accepted.Path || source.Digest == lastDigest {
					continue
				}
				lastDigest = source.Digest
				sendThemeWatchEvent(ctx, events, themeWatchEvent{path: accepted.Path, source: source})
				continue
			}
			if err != nil && err.Error() != lastError {
				lastError = err.Error()
				sendThemeWatchEvent(ctx, events, themeWatchEvent{path: accepted.Path, err: err})
			}
		}
	}
}

func sendThemeWatchEvent(ctx context.Context, events chan<- themeWatchEvent, event themeWatchEvent) {
	select {
	case events <- event:
	case <-ctx.Done():
	}
}
