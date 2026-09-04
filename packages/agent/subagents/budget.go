package subagents

import (
	"errors"
	"math"
	"sync"

	"github.com/bnema/zut/packages/provider"
)

const cachedInputBudgetWeight = 0.25

// BudgetState is the host-visible state of a resident child's cumulative
// rollout budget.
type BudgetState string

const (
	BudgetNormal   BudgetState = "normal"
	BudgetExceeded BudgetState = "exceeded"
)

var ErrBudgetExceeded = errors.New("resident subagent rollout budget exceeded")

// BudgetSnapshot is the bounded public projection of a child's cumulative
// rollout budget. Used is weighted usage, not the active context size.
type BudgetSnapshot struct {
	Used    int64       `json:"used_tokens"`
	Limit   int64       `json:"limit_tokens"`
	Percent int         `json:"percent"`
	State   BudgetState `json:"state"`
}

// ContextBudgetLimit derives the deterministic rollout limit for a child from
// its resolved model metadata.
func ContextBudgetLimit(contextWindow int) (int64, error) {
	if contextWindow <= 0 {
		return 0, errors.New("cannot derive a subagent budget without a model context window")
	}
	return int64(contextWindow), nil
}

// WeightedBudgetUsage charges uncached and cache-write input fully, cache-read
// input at a reduced weight, and output once (provider OutputTokens already
// includes reasoning tokens where the provider reports them).
func WeightedBudgetUsage(usage provider.Usage) int64 {
	weighted := float64(max(usage.InputTokens, 0)) +
		float64(max(usage.CacheWriteTokens, 0)) +
		float64(max(usage.OutputTokens, 0)) +
		float64(max(usage.CacheReadTokens, 0))*cachedInputBudgetWeight
	return int64(math.Ceil(weighted))
}

func BudgetSnapshotFor(usage provider.Usage, limit int64) BudgetSnapshot {
	if limit <= 0 {
		return BudgetSnapshot{}
	}
	return budgetSnapshotSince(usage, limit, 0)
}

func budgetSnapshotSince(usage provider.Usage, limit, baseline int64) BudgetSnapshot {
	if limit <= 0 {
		return BudgetSnapshot{}
	}
	used := max(WeightedBudgetUsage(usage)-baseline, 0)
	percent := int(math.Floor(float64(used) * 100 / float64(limit)))
	return BudgetSnapshot{Used: used, Limit: limit, Percent: percent, State: budgetState(used, limit)}
}

func budgetState(used, limit int64) BudgetState {
	if limit <= 0 {
		return ""
	}
	if used >= limit {
		return BudgetExceeded
	}
	return BudgetNormal
}

// RolloutBudget accounts one resident child's cumulative provider usage.
type RolloutBudget struct {
	mu       sync.Mutex
	limit    int64
	baseline int64
	usage    provider.Usage
	snapshot BudgetSnapshot
}

func NewRolloutBudget(limit int64, usage provider.Usage) *RolloutBudget {
	budget := &RolloutBudget{limit: limit, usage: usage}
	budget.snapshot = BudgetSnapshotFor(usage, limit)
	return budget
}

func (b *RolloutBudget) Observe(usage provider.Usage) BudgetSnapshot {
	if b == nil {
		return BudgetSnapshot{}
	}
	b.mu.Lock()
	b.usage = usage
	b.snapshot = budgetSnapshotSince(usage, b.limit, b.baseline)
	snapshot := b.snapshot
	b.mu.Unlock()
	return snapshot
}

// SetBaseline applies an explicitly accepted recovery allowance without
// discarding cumulative cost or conversation history.
func (b *RolloutBudget) SetBaseline(baseline int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.baseline = baseline
	b.snapshot = budgetSnapshotSince(b.usage, b.limit, baseline)
	b.mu.Unlock()
}

func (b *RolloutBudget) Snapshot() BudgetSnapshot {
	if b == nil {
		return BudgetSnapshot{}
	}
	b.mu.Lock()
	snapshot := b.snapshot
	b.mu.Unlock()
	return snapshot
}
