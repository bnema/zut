package session

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/agent/internal/orchestration"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestResidentHostUpdates(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "busy"}[busy], func(t *testing.T) {
			c, ctx := newTestController(t)
			turn := newMockturnSeam(t)
			var inputs []Input
			turn.EXPECT().enqueue(mock.Anything).Run(func(in Input) { inputs = append(inputs, in) }).Twice()
			require.NoError(t, c.call(ctx, func() {
				c.turnSeam = turn
				s := &c.res
				s.observe(core.EvTurnStart{})
				for _, id := range []string{"one", "two"} {
					s.coordinator.Apply(orchestration.Event{Kind: orchestration.EventWorkerRegistered, WorkerID: id, AgentID: id})
				}
				if !busy {
					s.observe(core.EvTurnEnd{})
				}
				for _, id := range []string{"one", "two"} {
					s.deliver(s.coordinator.Apply(orchestration.Event{Kind: orchestration.EventWorkerFinished, WorkerID: id, Completion: subagents.Completion{AgentID: id, Summary: "done"}}).Actions)
				}
				require.Len(t, inputs, 2)
				for _, in := range inputs {
					require.True(t, in.HostEvent)
					require.Equal(t, InputOriginResident, in.Origin)
					require.Contains(t, in.Text, "done")
				}
				require.Contains(t, inputs[0].Text, "Still running: two")
			}))
			// Restore the ordinary seam before any projection reads.
			require.NoError(t, c.call(context.Background(), func() { c.turnSeam = c }))
		})
	}
}

func TestResidentRunningProjection(t *testing.T) {
	c, ctx := newTestController(t)
	require.NoError(t, c.call(ctx, func() {
		require.False(t, c.residentsRunning())
		require.Empty(t, c.residentSummaries())
	}))
}
