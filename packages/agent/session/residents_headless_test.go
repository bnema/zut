package session

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/agent/internal/orchestration"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Headless --orchestrate sessions keep the pre-daemon completion wording:
// HeadlessCompletionInstruction and no still-running list.
func TestResidentHeadlessOrchestrateWording(t *testing.T) {
	c, ctx := newTestController(t)
	turn := newMockturnSeam(t)
	var inputs []Input
	turn.EXPECT().enqueue(mock.Anything).Run(func(in Input) { inputs = append(inputs, in) }).Twice()
	require.NoError(t, c.call(ctx, func() {
		c.deps.HeadlessOrchestrate = true
		c.turnSeam = turn
		s := &c.res
		c.residentsRunStarted()
		for _, id := range []string{"one", "two"} {
			s.coordinator.Apply(orchestration.Event{Kind: orchestration.EventWorkerRegistered, WorkerID: id, AgentID: id})
		}
		for _, id := range []string{"one", "two"} {
			s.deliver(s.coordinator.Apply(orchestration.Event{Kind: orchestration.EventWorkerFinished, WorkerID: id, Completion: subagents.Completion{AgentID: id, Summary: "done"}}).Actions)
		}
		require.Len(t, inputs, 2)
		for i, in := range inputs {
			batch := []subagents.Completion{{AgentID: []string{"one", "two"}[i], Summary: "done"}}
			require.Equal(t, subagents.FormatCompletionUpdate(batch, HeadlessCompletionInstruction), in.Text)
			require.NotContains(t, in.Text, "Still running")
			require.True(t, in.HostEvent)
			require.Equal(t, InputOriginResident, in.Origin)
		}
	}))
	require.NoError(t, c.call(context.Background(), func() { c.turnSeam = c }))
}
