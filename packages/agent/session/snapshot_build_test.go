package session

import (
	"context"
	"errors"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/require"
)

// A failed lazy agent build must surface as an error note instead of a
// silently empty history, and the next snapshot retries the build.
func TestSnapshotReportsAgentBuildFailureAndRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	builder := newMockAgentBuilderLocal(t)
	builder.EXPECT().BuildAgent().Return(nil, "", "", errors.New("no credentials")).Once()
	c := NewController(ctx, Deps{AgentBuilder: builder})

	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Empty(t, snap.Messages)
	require.NotEmpty(t, snap.Notes)
	last := snap.Notes[len(snap.Notes)-1]
	require.Equal(t, "error", last.Level)
	require.Contains(t, last.Text, "no credentials")

	ag := core.NewAgent(nil, "m", "", nil)
	ag.SetMessages([]provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "restored"}}}})
	builder.EXPECT().BuildAgent().Return(ag, "p", "m", nil).Once()
	snap, err = c.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snap.Messages, 1)
	for _, n := range snap.Notes {
		require.NotContains(t, n.Text, "session history unavailable")
	}
}
