package agent

import "testing"

func TestSubagentPolicyDefaultsBudgetRatio(t *testing.T) {
	policy := subagentPolicyFromConfig(SubagentsConfig{})
	if policy.BudgetRatio != 0.75 || policy.BudgetRatioConfigured {
		t.Fatalf("default budget policy = %#v", policy)
	}
	ratio := 0.75
	policy = subagentPolicyFromConfig(SubagentsConfig{BudgetRatio: &ratio})
	if policy.BudgetRatio != 0.75 || !policy.BudgetRatioConfigured {
		t.Fatalf("configured budget policy = %#v", policy)
	}
}

func TestValidateSubagentBudgetRatio(t *testing.T) {
	valid := 0.6
	if err := validateSubagentConfig(SubagentsConfig{BudgetRatio: &valid}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []float64{0, -0.1, 1.1} {
		value := value
		if err := validateSubagentConfig(SubagentsConfig{BudgetRatio: &value}); err == nil {
			t.Fatalf("budget ratio %v accepted", value)
		}
	}
}
