package session

import (
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/require"
)

func TestExternalUsageKeepsContextUsed(t *testing.T) {
	c, ctx := newTestController(t)
	var contextUsed int
	var usage provider.Usage
	require.NoError(t, c.call(ctx, func() {
		c.applyAgentEvent(core.EvUsage{Usage: provider.Usage{InputTokens: 300, CacheReadTokens: 200}})
		c.applyAgentEvent(core.EvUsage{Usage: provider.Usage{InputTokens: 40, CostUSD: 0.25}, External: true})
		contextUsed = c.turn.projection.ContextUsed
		usage = c.turn.projection.Usage
	}))
	require.Equal(t, 500, contextUsed)
	require.Equal(t, 340, usage.InputTokens)
	require.Equal(t, 0.25, usage.CostUSD)
}

func TestGoalCountsExternalUsageOnce(t *testing.T) {
	var s goalState
	s.run = &goalRun{}
	s.observe(core.EvUsage{Usage: provider.Usage{InputTokens: 10, OutputTokens: 5}})
	s.observe(core.EvUsage{Usage: provider.Usage{InputTokens: 40, OutputTokens: 2}, External: true})
	require.Equal(t, uint64(57), s.run.tokens)
}
