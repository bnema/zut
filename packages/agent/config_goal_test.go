package agent

import (
	"encoding/json"
	"testing"
)

func TestGoalsConfigTokenBudgetIsOptional(t *testing.T) {
	var unlimited Config
	if err := json.Unmarshal([]byte(`{}`), &unlimited); err != nil {
		t.Fatal(err)
	}
	if unlimited.Goals.MaxTokenBudget != nil {
		t.Fatalf("missing budget = %v, want nil", *unlimited.Goals.MaxTokenBudget)
	}

	var limited Config
	if err := json.Unmarshal([]byte(`{"goals":{"max_token_budget":1000000}}`), &limited); err != nil {
		t.Fatal(err)
	}
	if limited.Goals.MaxTokenBudget == nil || *limited.Goals.MaxTokenBudget != 1_000_000 {
		t.Fatalf("configured budget = %#v", limited.Goals.MaxTokenBudget)
	}
}
