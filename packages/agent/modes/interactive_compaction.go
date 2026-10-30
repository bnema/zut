package modes

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/provider"
)

// estimateCompactedContextInput approximates the input tokens of the
// just-compacted transcript for the status display. It uses the same rough
// 1-token-per-4-chars rule as compaction's own estimate, summed over the
// textual timeline content. Empty transcripts yield zero; image bytes are
// not counted. This is display-only: the next provider usage replaces it,
// and immediate continuations bypass the pre-turn guard until then.
func estimateCompactedContextInput(msgs []provider.Message) int {
	total := 0
	for _, msg := range msgs {
		for _, c := range msg.Content {
			switch v := c.(type) {
			case provider.TextBlock:
				total += len(v.Text)
			case provider.ToolCallBlock:
				total += len(v.Name) + len(v.Arguments)
			case provider.ToolResultBlock:
				for _, inner := range v.Content {
					if text, ok := inner.(provider.TextBlock); ok {
						total += len(text.Text)
					}
				}
			case provider.ReasoningBlock:
				total += len(v.Summary)
			}
		}
	}
	return total / 4
}
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
func (i *Interactive) currentCompactHandoff() json.RawMessage {
	if current := i.cfg.CurrentCompactHandoff; current != nil {
		return current()
	}
	return nil
}
func (i *Interactive) restoreCompactHandoff(state compactContinuationState) {
	i.mu.Lock()
	i.compactContinuation = state
	i.mu.Unlock()
}
func (i *Interactive) restoreCurrentCompactHandoff() compactContinuationState {
	state := decodeCompactHandoff(i.currentCompactHandoff())
	i.restoreCompactHandoff(state)
	return state
}

func (i *Interactive) runCompact(parent context.Context, request compactContinuationRequest) {
	i.portSettingsCall("compact", func(ctx context.Context, port session.Port) error { return port.Compact(ctx) })
}
