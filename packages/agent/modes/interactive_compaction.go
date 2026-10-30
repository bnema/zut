package modes

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bnema/zut/packages/agent/session"
)

// The remaining handoff reset/serialization supports model_profiles.go and
// swapModel (#49). Remove it with those excluded legacy profile paths.
func (i *Interactive) compactHandoffLocked() json.RawMessage {
	return encodeCompactHandoff(i.compactContinuation)
}
func (i *Interactive) setCompactContinuationLocked(state compactContinuationState) (json.RawMessage, bool) {
	if i.compactContinuation == state {
		return i.compactHandoffLocked(), false
	}
	i.compactContinuation = state
	return i.compactHandoffLocked(), true
}
func (i *Interactive) resetCompactContinuationLocked() (json.RawMessage, bool) {
	return i.setCompactContinuationLocked(compactContinuationState{})
}
func (i *Interactive) persistCompactHandoff(state json.RawMessage) {
	if persist := i.cfg.PersistCompactHandoff; persist != nil {
		if err := persist(state); err != nil {
			i.ReportError(fmt.Errorf("persist compact handoff: %w", err))
		}
	}
}
func (i *Interactive) resetCompactHandoff() {
	i.mu.Lock()
	state, persist := i.resetCompactContinuationLocked()
	i.mu.Unlock()
	if persist {
		i.persistCompactHandoff(state)
	}
}

func (i *Interactive) runCompact() {
	i.portSettingsCall("compact", func(ctx context.Context, port session.Port) error { return port.Compact(ctx) })
}
