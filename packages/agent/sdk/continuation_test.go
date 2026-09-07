package sdk

import (
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// TestTurnEndPreservesContinuationStop pins that the SDK event converter
// forwards the additive continuation stop value instead of treating the
// intermediate inference as overall completion.
func TestTurnEndPreservesContinuationStop(t *testing.T) {
	ev := toEvent(core.EvTurnEnd{Stop: provider.StopContinue})
	if ev.Type != "turn_end" {
		t.Fatalf("event type = %q, want turn_end", ev.Type)
	}
	if ev.Stop != "continue" {
		t.Fatalf("stop = %q, want continue", ev.Stop)
	}
}
