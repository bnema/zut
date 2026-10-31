package session

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestResidentRunStateAfterParentTurnEnds(t *testing.T) {
	manager := newMockResidentHostLocal(t)
	var update func(string)
	var running atomic.Bool
	running.Store(true)
	manager.EXPECT().SetAcceptedObserver(mock.Anything).Return()
	manager.EXPECT().SetCompletionObserver(mock.Anything).Return()
	manager.EXPECT().SetUpdateObserver(mock.Anything).Run(func(f func(string)) { update = f })
	manager.EXPECT().Snapshot().RunAndReturn(func() []subagents.ResidentSnapshot {
		state := subagents.ResidentIdle
		if running.Load() {
			state = subagents.ResidentRunning
		}
		return []subagents.ResidentSnapshot{{ID: "child", State: state}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	c := NewController(ctx, Deps{ResidentManager: manager})
	defer func() { cancel(); <-c.Done() }()
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("test").Maybe()
	var seq uint64
	require.NoError(t, c.call(ctx, func() {
		c.turn.agent = core.NewAgent(client, "test", "", nil)
		c.turn.busy = true
		c.turn.started = c.deps.Clock()
		c.residentsRunStarted()
		c.residentsRunEnded()
		c.finishTurn(ctx, nil)
		require.False(t, c.isBusy(), "resident work must not gate parent input")
		require.Equal(t, RunStateRunning, c.Info().Run)
		seq = c.seq
	}))
	events, err := c.Subscribe(ctx, seq)
	require.NoError(t, err)
	running.Store(false)
	update("child")
	for {
		select {
		case ev, ok := <-events:
			require.True(t, ok)
			if ev.Kind != EvInfo {
				continue
			}
			info := ev.Data.(InfoEvent).Info
			if info.Run != RunStateIdle {
				continue
			}
			require.Equal(t, RunStateIdle, c.Info().Run)
			return
		case <-ctx.Done():
			t.Fatal("resident completion did not republish idle info:", ctx.Err())
		}
	}
}
