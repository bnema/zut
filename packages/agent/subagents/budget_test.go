package subagents

import (
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestBudgetLimitDefaultsToSeventyFivePercent(t *testing.T) {
	limit, ratio, err := BudgetLimit(500_000, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if limit != 375_000 || ratio != 0.75 {
		t.Fatalf("BudgetLimit = %d, %v; want 375000, 0.75", limit, ratio)
	}

	limit, ratio, err = BudgetLimit(500_000, 0.5, 0)
	if err != nil || limit != 250_000 || ratio != 0.5 {
		t.Fatalf("ratio override = %d, %v, %v", limit, ratio, err)
	}

	limit, ratio, err = BudgetLimit(500_000, 0.5, 42_000)
	if err != nil || limit != 42_000 || ratio != 0 {
		t.Fatalf("token override = %d, %v, %v", limit, ratio, err)
	}
}

func TestEffectiveBudgetLimitDerivesLegacyDefault(t *testing.T) {
	if got := EffectiveBudgetLimit(0, 500_000); got != 375_000 {
		t.Fatalf("legacy budget limit = %d, want 375000", got)
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

func TestBudgetThresholdInstructionsAreProgressiveAndOneShot(t *testing.T) {
	budget := NewRolloutBudget(1_000, provider.Usage{})
	cases := []struct {
		used  int
		state BudgetState
		text  string
	}{
		{500, BudgetNotice, "prioritize concrete evidence"},
		{700, BudgetFocused, "Stop broad exploration"},
		{850, BudgetVerifying, "Do not identify new candidates"},
		{900, BudgetFinalizing, "Finalization is required now"},
	}
	for _, tc := range cases {
		budget.Observe(provider.Usage{InputTokens: tc.used})
		if got := budget.Snapshot().State; got != tc.state {
			t.Fatalf("usage %d state = %q, want %q", tc.used, got, tc.state)
		}
		instruction, exceeded := budget.TurnContext()
		if exceeded || !strings.Contains(instruction, tc.text) {
			t.Fatalf("usage %d instruction = %q exceeded=%t", tc.used, instruction, exceeded)
		}
		if repeated, exceeded := budget.TurnContext(); repeated != "" || exceeded {
			t.Fatalf("usage %d repeated instruction = %q exceeded=%t", tc.used, repeated, exceeded)
		}
	}

	budget.Observe(provider.Usage{InputTokens: 1_000})
	if instruction, exceeded := budget.TurnContext(); instruction != "" || !exceeded {
		t.Fatalf("exhausted instruction = %q exceeded=%t", instruction, exceeded)
	}
}
