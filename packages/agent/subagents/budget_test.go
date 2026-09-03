package subagents

import (
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestContextBudgetLimitUsesFullModelContext(t *testing.T) {
	limit, err := ContextBudgetLimit(500_000)
	if err != nil {
		t.Fatal(err)
	}
	if limit != 500_000 {
		t.Fatalf("ContextBudgetLimit = %d, want 500000", limit)
	}
	if _, err := ContextBudgetLimit(0); err == nil {
		t.Fatal("ContextBudgetLimit(0) succeeded")
	}
}

func TestEffectiveBudgetLimitPreservesLegacyLimit(t *testing.T) {
	if got := EffectiveBudgetLimit(0, 500_000); got != 500_000 {
		t.Fatalf("default budget limit = %d, want 500000", got)
	}
	if got := EffectiveBudgetLimit(42_000, 500_000); got != 42_000 {
		t.Fatalf("persisted budget limit = %d, want 42000", got)
	}
}

func TestWeightedBudgetUsageDiscountsCacheReads(t *testing.T) {
	usage := provider.Usage{InputTokens: 100, CacheReadTokens: 100, CacheWriteTokens: 20, OutputTokens: 30, ReasoningTokens: 10, ReasoningTokensKnown: true}
	if got := WeightedBudgetUsage(usage); got != 175 {
		t.Fatalf("WeightedBudgetUsage = %d, want 175", got)
	}
}

func TestRolloutBudgetOnlyExhaustsAtLimit(t *testing.T) {
	budget := NewRolloutBudget(1_000, provider.Usage{})
	for _, used := range []int{500, 700, 850, 900, 999} {
		budget.Observe(provider.Usage{InputTokens: used})
		if got := budget.Snapshot().State; got != BudgetNormal {
			t.Fatalf("usage %d state = %q, want %q", used, got, BudgetNormal)
		}
	}
	budget.Observe(provider.Usage{InputTokens: 1_000})
	if got := budget.Snapshot().State; got != BudgetExceeded {
		t.Fatalf("exhausted state = %q, want %q", got, BudgetExceeded)
	}
}
