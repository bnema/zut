package modes

import (
	"context"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// nextPromptTimeout bounds one best-effort prediction request.
const nextPromptTimeout = 20 * time.Second

// nextPromptSuggestionsEnabled reports whether next prompt suggestions are
// on. nil means disabled: the feature is opt-in because every prediction is
// an extra model request.
func nextPromptSuggestionsEnabled(enabled *bool) bool {
	return enabled != nil && *enabled
}

// Locking: i.ed is owned by the Run loop. The functions below that touch it
// run on the Run loop (key handling, settings toggles, session events,
// loopCalls); the request goroutine only posts its result through
// i.loopCalls. nextPromptVersion and nextPromptCancel are guarded by i.mu.

// invalidateNextPromptLocked drops any in-flight prediction so a late
// result is ignored. The caller holds i.mu.
func (i *Interactive) invalidateNextPromptLocked() {
	i.nextPromptVersion++
	if i.nextPromptCancel != nil {
		i.nextPromptCancel()
		i.nextPromptCancel = nil
	}
}

// dismissNextPromptLocked invalidates the request and hides the ghost. The
// caller holds i.mu on the Run loop.
func (i *Interactive) dismissNextPromptLocked() {
	i.invalidateNextPromptLocked()
	if i.ed != nil {
		i.ed.SetGhost("")
	}
}

// clearNextPromptSuggestion invalidates any request and removes the shown
// ghost text. Run loop only; the caller must not hold i.mu.
func (i *Interactive) clearNextPromptSuggestion() {
	i.mu.Lock()
	i.dismissNextPromptLocked()
	i.mu.Unlock()
}

// nextPromptTurnEndedCleanly reports whether end finished without error,
// cancellation, abort, or truncation.
func nextPromptTurnEndedCleanly(end *core.EvTurnEnd) bool {
	if end == nil || end.Err != nil {
		return false
	}
	return end.Stop != provider.StopAborted && end.Stop != provider.StopLength
}

// nextPromptEligibleLocked reports whether a suggestion may be requested or
// shown now. The caller holds i.mu on the Run loop.
func (i *Interactive) nextPromptEligibleLocked() bool {
	return nextPromptSuggestionsEnabled(i.cfg.NextPromptSuggestionsEnabled) &&
		i.port != nil &&
		i.ed != nil &&
		!i.busy &&
		!i.autoCompacting &&
		!i.sessionLoading &&
		!i.shellRunning &&
		len(i.portQueue) == 0 &&
		len(i.portPending) == 0 &&
		!i.confirmDialog.Active() &&
		!i.confirmChildActiveLocked() &&
		i.ed.IsEmpty()
}

// maybeRequestNextPromptLocked starts a prediction after a clean turn end
// when the UI is idle and the input is empty. The caller holds i.mu on the
// Run loop. Errors are ignored: suggestions are best effort.
func (i *Interactive) maybeRequestNextPromptLocked(end *core.EvTurnEnd) {
	i.invalidateNextPromptLocked()
	if !nextPromptTurnEndedCleanly(end) || !i.nextPromptEligibleLocked() {
		return
	}
	parent := i.runCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, nextPromptTimeout)
	i.nextPromptCancel = cancel
	version := i.nextPromptVersion
	port := i.port
	finished := i.nextPromptFinished
	go func() {
		defer cancel()
		if finished != nil {
			defer finished()
		}
		text, err := port.PredictNextPrompt(ctx)
		if err != nil || text == "" {
			return
		}
		apply := func() { i.applyNextPrompt(version, text) }
		select {
		case i.loopCalls <- apply:
		case <-ctx.Done():
		}
	}()
}

// applyNextPrompt shows text when it still belongs to the current request
// and the UI is still eligible. Run loop only.
func (i *Interactive) applyNextPrompt(version uint64, text string) {
	i.mu.Lock()
	if version != i.nextPromptVersion {
		i.mu.Unlock()
		return
	}
	ok := i.nextPromptEligibleLocked()
	i.invalidateNextPromptLocked()
	if ok {
		i.ed.SetGhost(text)
	}
	i.mu.Unlock()
	if ok {
		i.invalidate()
	}
}

// isNextPromptViewKey reports pure view keys (scrolling, tool expansion)
// that keep both a shown ghost and an in-flight request.
func isNextPromptViewKey(k tui.Key) bool {
	switch k.Kind {
	case tui.KeyPageUp, tui.KeyPageDown, tui.KeyMouseWheelUp, tui.KeyMouseWheelDown, tui.KeyCtrlO:
		return true
	}
	return false
}

// noteNextPromptKey runs for every key before routing. Any key other than a
// view key invalidates an in-flight request; keys other than a bare Tab or
// Escape also dismiss the shown ghost, which those two handle in
// handleNextPromptKey.
func (i *Interactive) noteNextPromptKey(k tui.Key) {
	if isNextPromptViewKey(k) {
		return
	}
	i.mu.Lock()
	i.invalidateNextPromptLocked()
	if i.ed != nil && !isNextPromptAcceptKey(k) && !isUnmodifiedEscape(k) {
		i.ed.SetGhost("")
	}
	i.mu.Unlock()
}

func isNextPromptAcceptKey(k tui.Key) bool {
	return k.Kind == tui.KeyTab && !k.Ctrl && !k.Alt && !k.Super && !k.Shift
}

// handleNextPromptKey lets the main input accept (Tab) or dismiss (Escape)
// a shown ghost. It reports whether it consumed the key. Run loop only.
func (i *Interactive) handleNextPromptKey(k tui.Key) bool {
	// Ghost() is empty once text is typed, so slash and @ popups, which
	// need typed text, always keep Tab.
	if i.ed == nil || i.ed.Ghost() == "" {
		return false
	}
	switch {
	case isNextPromptAcceptKey(k):
		i.ed.AcceptGhost()
	case isUnmodifiedEscape(k):
		i.ed.SetGhost("")
		i.mu.Lock()
		i.doubleEscape.Reset()
		i.mu.Unlock()
	case k.Kind == tui.KeyMouseWheelUp || k.Kind == tui.KeyMouseWheelDown:
		// The editor ignores the wheel but would drop the ghost.
		return true
	default:
		return false
	}
	i.invalidate()
	return true
}

// nextPromptGhostStyle renders the ghost in the theme's muted color.
func nextPromptGhostStyle(th tui.Theme) func(string) string {
	return func(s string) string { return th.FGColor(th.Muted, s) }
}
