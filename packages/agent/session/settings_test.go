package session

import (
	"context"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestSettingsReplacementPreservesTranscript(t *testing.T) {
	c, ctx := newTestController(t)
	turn := lane9Turn(t, c, ctx)
	old := core.NewAgent(providermocks.NewMockClient(t), "old", "", nil)
	next := core.NewAgent(providermocks.NewMockClient(t), "new", "", nil)
	old.RestoreMessages([]provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "keep me"}}}})
	old.SeedCost(provider.Usage{InputTokens: 42})
	turn.EXPECT().currentAgent().Return(old).Once()
	turn.EXPECT().setAgent(next).RunAndReturn(func(ag *core.Agent) error {
		require.Equal(t, old.Messages(), ag.Messages())
		require.Equal(t, old.Cost(), ag.Cost())
		return nil
	}).Once()
	require.NoError(t, c.call(ctx, func() { c.replaceAgent(next); require.NoError(t, c.settings.replaceErr) }))
}
func lane9Turn(t *testing.T, c *Controller, ctx context.Context, projections ...turnInfoProjection) *mockturnSeam {
	t.Helper()
	m := newMockturnSeam(t)
	m.EXPECT().isBusy().Return(false).Maybe()
	m.EXPECT().turnOrigin().Return(InputOrigin("")).Maybe()
	projection := turnInfoProjection{}
	if len(projections) > 0 {
		projection = projections[0]
	}
	m.EXPECT().turnInfo().Return(projection).Maybe()
	require.NoError(t, c.call(ctx, func() { c.turnSeam = m }))
	return m
}
