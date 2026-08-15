package tui

import (
	"fmt"

	"github.com/bnema/zut/packages/provider"
)

// UsageStatsParams describes cumulative usage shown by compact status surfaces.
type UsageStatsParams struct {
	Usage        provider.Usage
	Subscription bool
}

// UsageStatsParts returns compact, provider-neutral token and cost labels.
func UsageStatsParts(p UsageStatsParams) []string {
	var parts []string
	if p.Usage.InputTokens > 0 {
		parts = append(parts, fmt.Sprintf("↑%s", formatTokens(p.Usage.InputTokens)))
	}
	if p.Usage.OutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("↓%s", formatTokens(p.Usage.OutputTokens)))
	}
	if p.Usage.CacheReadTokens > 0 {
		parts = append(parts, fmt.Sprintf("R%s/", formatTokens(p.Usage.CacheReadTokens)))
	}
	if ratio, ok := p.Usage.CacheHitRatio(); ok {
		parts = append(parts, fmt.Sprintf("C%.1f%%", ratio*100))
	}
	if p.Usage.CacheWriteTokens > 0 {
		parts = append(parts, fmt.Sprintf("W%s", formatTokens(p.Usage.CacheWriteTokens)))
	}
	if p.Usage.CostUSD > 0 || p.Subscription {
		cost := fmt.Sprintf("$%.3f", p.Usage.CostUSD)
		if p.Subscription {
			cost += " (sub)"
		}
		parts = append(parts, cost)
	}
	return parts
}

// ContextUsageText returns the latest request's context utilization without
// styling so callers can safely measure, truncate, and color the whole line.
func ContextUsageText(used, max int) string {
	label, _ := contextUsage(Theme{}, used, max)
	return label
}
