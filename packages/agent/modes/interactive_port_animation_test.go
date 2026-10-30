package modes

import (
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/stretchr/testify/require"
)

func TestPortResidentIdleTickRequestsRedraw(t *testing.T) {
	for _, state := range []subagents.ResidentState{subagents.ResidentQueued, subagents.ResidentRunning, subagents.ResidentCompleted} {
		t.Run(string(state), func(t *testing.T) {
			// No local agent, manager, input or invalidate: only the daemon info
			// and an explicit animation tick, as in an idle daemon client.
			i := &Interactive{}
			i.portInfo = session.Info{Residents: []session.ResidentSummary{{ID: "child", State: string(state)}}}
			require.False(t, i.residentAnimating.Load())
			redraws := 0
			for n := 0; n < 2; n++ {
				i.redrawAnimationTick(func() { redraws++ })
			}
			if state == subagents.ResidentCompleted {
				require.Zero(t, redraws, "completed residents must not keep repainting")
			} else {
				require.Equal(t, 2, redraws, "each idle tick must refresh resident ages without input")
			}
		})
	}
}
