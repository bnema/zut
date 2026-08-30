package subagents

import (
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/bnema/zut/packages/provider"
)

const (
	DefaultBudgetRatio      = 0.75
	cachedInputBudgetWeight = 0.25
)

// BudgetState is the progressive constraint applied to a resident child.
type BudgetState string

const (
	BudgetNormal     BudgetState = "normal"
	BudgetNotice     BudgetState = "notice"
	BudgetFocused    BudgetState = "focused"
	BudgetVerifying  BudgetState = "verifying"
	BudgetFinalizing BudgetState = "finalizing"
	BudgetExceeded   BudgetState = "exceeded"
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

func BudgetLimit(contextWindow int, ratio float64, tokens int64) (int64, float64, error) {
	if tokens < 0 {
		return 0, 0, errors.New("budget_tokens must be positive")
	}
	if tokens > 0 {
		return tokens, 0, nil
	}
	if ratio == 0 {
		ratio = DefaultBudgetRatio
	}
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio <= 0 || ratio > 1 {
		return 0, 0, errors.New("budget_ratio must be greater than 0 and at most 1")
	}
	if contextWindow <= 0 {
		return 0, ratio, errors.New("cannot derive a subagent budget without a parent context window")
	}
	limit := int64(math.Floor(float64(contextWindow) * ratio))
	if limit <= 0 {
		return 0, ratio, errors.New("derived subagent budget is empty")
	}
	return limit, ratio, nil
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

// EffectiveBudgetLimit preserves budgets accepted by current versions and
// derives the default for resident specs written before budgets were stored.
func EffectiveBudgetLimit(limit int64, contextWindow int) int64 {
	if limit > 0 {
		return limit
	}
	derived, _, err := BudgetLimit(contextWindow, DefaultBudgetRatio, 0)
	if err != nil {
		return 0
	}
	return derived
}

func BudgetSystemPrompt(limit int64) string {
	if limit <= 0 {
		return ""
	}
	return fmt.Sprintf("Your cumulative rollout budget is %d weighted tokens. At 50%% prioritize concrete evidence; at 70%% stop broad exploration; at 85%% verify existing candidates only; at 90%% stop using tools and return the best verified final result. The host enforces the finalization reserve and terminates work at 100%%.", limit)
}

func BudgetSnapshotFor(usage provider.Usage, limit int64) BudgetSnapshot {
	if limit <= 0 {
		return BudgetSnapshot{}
	}
	used := WeightedBudgetUsage(usage)
	percent := int(math.Floor(float64(used) * 100 / float64(limit)))
	return BudgetSnapshot{Used: used, Limit: limit, Percent: percent, State: budgetState(used, limit)}
}

func budgetState(used, limit int64) BudgetState {
	if limit <= 0 {
		return ""
	}
	ratio := float64(used) / float64(limit)
	switch {
	case ratio >= 1:
		return BudgetExceeded
	case ratio >= 0.90:
		return BudgetFinalizing
	case ratio >= 0.85:
		return BudgetVerifying
	case ratio >= 0.70:
		return BudgetFocused
	case ratio >= 0.50:
		return BudgetNotice
	default:
		return BudgetNormal
	}
}

// RolloutBudget accounts one resident child's cumulative provider usage and
// produces progressive model instructions at threshold transitions.
type RolloutBudget struct {
	mu        sync.Mutex
	limit     int64
	snapshot  BudgetSnapshot
	delivered BudgetState
}

func NewRolloutBudget(limit int64, usage provider.Usage) *RolloutBudget {
	budget := &RolloutBudget{limit: limit}
	budget.snapshot = BudgetSnapshotFor(usage, limit)
	return budget
}

func (b *RolloutBudget) Observe(usage provider.Usage) BudgetSnapshot {
	if b == nil {
		return BudgetSnapshot{}
	}
	b.mu.Lock()
	b.snapshot = BudgetSnapshotFor(usage, b.limit)
	snapshot := b.snapshot
	b.mu.Unlock()
	return snapshot
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

// TurnContext returns a one-shot model-visible transition alert. Core stores
// the resulting developer context in the transcript, including across compaction.
func (b *RolloutBudget) TurnContext() (string, bool) {
	if b == nil {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.snapshot.State
	if state == BudgetExceeded {
		return "", true
	}
	if budgetStateRank(state) <= budgetStateRank(b.delivered) {
		return "", false
	}
	b.delivered = state
	return budgetInstruction(b.snapshot), false
}

func budgetStateRank(state BudgetState) int {
	switch state {
	case BudgetNotice:
		return 1
	case BudgetFocused:
		return 2
	case BudgetVerifying:
		return 3
	case BudgetFinalizing:
		return 4
	case BudgetExceeded:
		return 5
	default:
		return 0
	}
}

func budgetInstruction(snapshot BudgetSnapshot) string {
	remaining := max(snapshot.Limit-snapshot.Used, 0)
	prefix := fmt.Sprintf("[subagent budget: %d%% used, %d weighted tokens remaining] ", snapshot.Percent, remaining)
	switch snapshot.State {
	case BudgetNotice:
		return prefix + "Keep the remaining investigation bounded and prioritize concrete evidence."
	case BudgetFocused:
		return prefix + "Stop broad exploration. Investigate only concrete unresolved hypotheses."
	case BudgetVerifying:
		return prefix + "Do not identify new candidates. Verify or reject current candidates and prepare the final result."
	case BudgetFinalizing:
		return prefix + "Finalization is required now. Return the best verified result without using tools or starting new investigations."
	default:
		return ""
	}
}
