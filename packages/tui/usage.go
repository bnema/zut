package tui

import (
	"fmt"

	"github.com/bnema/zut/packages/provider"
)

// UsageStatsParams describes cumulative usage shown by compact status surfaces.
type UsageStatsParams struct {
	Usage        provider.Usage
	Subscription bool
	Compact      bool // main status bar; detailed surfaces retain billing labels and precision
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
		read := "R" + formatTokens(p.Usage.CacheReadTokens)
		if !p.Compact {
			read += "/"
		}
		parts = append(parts, read)
	}
	if ratio, ok := p.Usage.CacheHitRatio(); ok {
		precision := 1
		if p.Compact {
			precision = 0
		}
		parts = append(parts, fmt.Sprintf("C%.*f%%", precision, ratio*100))
	}
	if p.Usage.CacheWriteTokens > 0 {
		parts = append(parts, fmt.Sprintf("W%s", formatTokens(p.Usage.CacheWriteTokens)))
	}
	if p.Usage.CostUSD > 0 || p.Subscription {
		precision := 3
		if p.Compact {
			precision = 2
		}
		cost := fmt.Sprintf("$%.*f", precision, p.Usage.CostUSD)
		if p.Subscription && !p.Compact {
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
