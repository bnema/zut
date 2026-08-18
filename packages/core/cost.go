package core

import "github.com/bnema/zut/packages/provider"

// CostTracker accumulates usage across turns in a session.
//
// Total is the cumulative usage shown in the status bar's "$x.xx"
// readout. LastTurn is the per-turn usage of the most recent
// completed turn; the TUI uses LastTurn.InputTokens+cache as a
// proxy for "current context size" so the X%/Ymax gauge tracks the
// prompt size that just went to the model.
type CostTracker struct {
	Total    provider.Usage
	LastTurn provider.Usage
	hasTotal bool
}

// Add folds u into the running total, records u as the last-turn
// snapshot, and returns the new cumulative value.
func (c *CostTracker) Add(u provider.Usage) provider.Usage {
	if c.hasTotal {
		c.Total = c.Total.Add(u)
	} else {
		c.Total = u
		c.hasTotal = true
	}
	c.LastTurn = u
	return c.Total
}

// Seed replaces the running total with usage restored from a session.
func (c *CostTracker) Seed(u provider.Usage) {
	c.Total = u
	c.hasTotal = u.InputTokens != 0 || u.OutputTokens != 0 || u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 || u.CacheMeasuredPromptTokens != 0 || u.CacheMeasuredReadTokens != 0 || u.CostUSD != 0
}
