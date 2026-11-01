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

func TestWaitingResidentsActivity(t *testing.T) {
	for _, withoutTurn := range []bool{false, true} {
		name := "idle_turn"
		if withoutTurn {
			name = "nil_turn"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Unix(100, 0)
			c := &Controller{deps: Deps{Clock: func() time.Time { return now }}}
			running := true
			residents := newMockresidentSeam(t)
			residents.EXPECT().residentsRunning().RunAndReturn(func() bool { return running })
			residents.EXPECT().residentSummaries().Return(nil)
			c.residentSeam = residents
			busy := false
			projection := Activity{Kind: "idle", Since: now.Add(-time.Minute)}
			if !withoutTurn {
				turn := newMockturnSeam(t)
				turn.EXPECT().isBusy().RunAndReturn(func() bool { return busy })
				turn.EXPECT().turnOrigin().Return("")
				turn.EXPECT().turnInfo().RunAndReturn(func() turnInfoProjection {
					return turnInfoProjection{Activity: projection}
				})
				c.turnSeam = turn
			}
			compacting := false
			compact := newMockcompactSeam(t)
			compact.EXPECT().compacting().RunAndReturn(func() bool { return compacting })
			c.compactSeam = compact

			c.updateInfo()
			started := now
			require.Equal(t, RunStateRunning, c.Info().Run)
			require.Equal(t, Activity{Kind: ActivityKindWaitingResidents, Since: started}, c.Info().Activity)
			now = now.Add(time.Second)
			c.updateInfo()
			require.Equal(t, started, c.Info().Activity.Since)

			if !withoutTurn {
				busy = true
				projection.Kind = "streaming"
				c.updateInfo()
				require.Equal(t, projection, c.Info().Activity)
				require.True(t, c.waitingResidentsSince.IsZero())
				busy = false
				now = now.Add(time.Second)
				c.updateInfo()
				require.Equal(t, now, c.Info().Activity.Since)
			}

			compacting = true
			c.updateInfo()
			require.Equal(t, "compacting", c.Info().Activity.Kind)
			require.True(t, c.waitingResidentsSince.IsZero())
			compacting = false
			c.updateInfo()
			require.Equal(t, ActivityKindWaitingResidents, c.Info().Activity.Kind)

			running = false
			c.updateInfo()
			require.Equal(t, RunStateIdle, c.Info().Run)
			if withoutTurn {
				require.Equal(t, Activity{Kind: "idle"}, c.Info().Activity)
			} else {
				require.Equal(t, projection, c.Info().Activity)
			}
			require.True(t, c.waitingResidentsSince.IsZero())
			now = now.Add(time.Second)
			running = true
			c.updateInfo()
			require.Equal(t, Activity{Kind: ActivityKindWaitingResidents, Since: now}, c.Info().Activity)
		})
	}
}
