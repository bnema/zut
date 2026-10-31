package session

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestManualCompactionPublishesRunState(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, ctx := newSyncTestController(t)
				turn := lane9Turn(t, c, ctx)
				release := make(chan struct{})
				client := providermocks.NewMockClient(t)
				client.EXPECT().Name().Return("openai").Maybe()
				client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(compactCtx context.Context, _ provider.Request) (<-chan provider.Event, error) {
					select {
					case <-release:
					case <-compactCtx.Done():
						return nil, compactCtx.Err()
					}
					if outcome == "failure" {
						return nil, errors.New("summary unavailable")
					}
					events := make(chan provider.Event, 2)
					events <- provider.EventTextDelta{Delta: "summary"}
					events <- provider.EventDone{}
					close(events)
					return events, nil
				}).Once()
				ag := core.NewAgent(client, "model", "", nil)
				for range 6 {
					ag.AppendUserContext("original", nil)
				}
				turn.EXPECT().currentAgent().Return(ag).Maybe()
				turn.EXPECT().drainQueued().Return().Once()
				events, err := c.Subscribe(ctx, 0)
				require.NoError(t, err)
				require.NoError(t, c.Compact(ctx))

				info := nextKinds(t, ctx, events, EvInfo).Data.(InfoEvent).Info
				require.Equal(t, RunStateCompacting, info.Run)
				require.Equal(t, "compacting", info.Activity.Kind)
				require.Equal(t, RunStateCompacting, c.Info().Run)
				require.Equal(t, "start", nextKinds(t, ctx, events, EvCompaction).Data.(CompactionEvent).Phase)
				require.ErrorIs(t, c.Compact(ctx), ErrBusy)

				if outcome == "cancel" {
					// This is the same session operation used by interactive Esc.
					require.NoError(t, c.Abort(ctx))
				} else {
					close(release)
				}
				info = nextKinds(t, ctx, events, EvInfo).Data.(InfoEvent).Info
				require.Equal(t, RunStateIdle, info.Run)
				require.NotEqual(t, "compacting", info.Activity.Kind)
				phase := "failed"
				if outcome == "success" {
					phase = "done"
				}
				require.Equal(t, phase, nextKinds(t, ctx, events, EvCompaction).Data.(CompactionEvent).Phase)
				require.Equal(t, RunStateIdle, c.Info().Run)
				synctest.Wait()
			})
		})
	}
}
