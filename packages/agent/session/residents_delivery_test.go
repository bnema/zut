package session

import (
	"context"
	"github.com/bnema/zut/packages/core"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/internal/orchestration"
	"github.com/bnema/zut/packages/agent/subagents"

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
				c.residentsRunStarted()
				for _, id := range []string{"one", "two"} {
					s.coordinator.Apply(orchestration.Event{Kind: orchestration.EventWorkerRegistered, WorkerID: id, AgentID: id})
				}
				if !busy {
					c.residentsRunEnded()
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

// Exercise callbacks while the actor is inside a manager call: none may wait
// for the actor, and the retry boundary must not seal the manager run.
func TestResidentObserverRetryAndReentry(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "busy"}[busy], func(t *testing.T) {
			m := newMockResidentHostLocal(t)
			var accepted func(subagents.ResidentChildSpec, string, string)
			var completion func(subagents.ResidentCompletion) bool
			var update func(string)
			m.EXPECT().SetAcceptedObserver(mock.Anything).Run(func(f func(subagents.ResidentChildSpec, string, string)) { accepted = f })
			m.EXPECT().SetCompletionObserver(mock.Anything).Run(func(f func(subagents.ResidentCompletion) bool) { completion = f })
			m.EXPECT().SetUpdateObserver(mock.Anything).Run(func(f func(string)) { update = f })
			m.EXPECT().Snapshot().Return(nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			c := NewController(ctx, Deps{ResidentManager: m})
			defer func() { cancel(); <-c.Done() }()
			turn := newMockturnSeam(t)
			turn.EXPECT().isBusy().Return(busy)
			turn.EXPECT().turnOrigin().Return(InputOriginUser)
			turn.EXPECT().turnInfo().Return(turnInfoProjection{})
			delivered := make(chan Input, 1)
			var enqueued atomic.Bool
			turn.EXPECT().enqueue(mock.Anything).Run(func(in Input) { enqueued.Store(true); delivered <- in }).Once()
			// An idle manager has no turn end to drain the FIFO: the resident
			// mailbox must start the queued update itself, after enqueueing it.
			drained := make(chan bool, 4)
			turn.EXPECT().drainQueued().Run(func() { drained <- enqueued.Load() }).Maybe()
			require.NoError(t, c.call(ctx, func() {
				c.turnSeam = turn
				if busy {
					c.residentsRunStarted()
				}
			}))
			m.EXPECT().Resume(ctx, "child", "task").RunAndReturn(func(context.Context, string, string) error {
				accepted(subagents.ResidentChildSpec{ID: "child"}, "turn", "task")
				update("child")
				return nil
			})
			require.NoError(t, c.call(ctx, func() {
				require.NoError(t, c.ResidentAction(ctx, ResidentCommand{Op: "resume", ChildID: "child", Prompt: "task"}))
			}))
			require.NoError(t, c.call(ctx, func() {
				c.res.observe(core.EvTurnEnd{})
				require.True(t, completion(subagents.ResidentCompletion{ChildID: "child", TurnID: "turn", Summary: "done"}))
			}))
			select {
			case in := <-delivered:
				require.True(t, in.HostEvent)
				require.Equal(t, InputOriginResident, in.Origin)
				if busy {
					require.Contains(t, in.Text, "worker evidence")
				} else {
					require.Contains(t, in.Text, "collective outcome")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			for afterEnqueue := false; !afterEnqueue; {
				select {
				case afterEnqueue = <-drained:
				case <-ctx.Done():
					t.Fatal("queued completion update was never drained")
				}
			}
			require.NoError(t, c.call(ctx, func() { c.residentsRunEnded(); c.turnSeam = c }))
		})
	}
}
