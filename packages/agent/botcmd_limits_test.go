package agent

import (
	"testing"

	"github.com/bnema/zut/packages/core"
)

func TestApplyResolvedModelAdoptsLimits(t *testing.T) {
	ag := core.NewAgent(nil, "old", "", nil)
	ag.MaxTokens = 4096
	ag.ContextWindow = 8000

	applyResolvedModel(ag, Resolved{Model: "new", MaxOutput: 64000, ContextWindow: 200000})
	if ag.Model != "new" || ag.MaxTokens != 64000 || ag.ContextWindow != 200000 {
		t.Fatalf("agent = model %q max %d ctx %d", ag.Model, ag.MaxTokens, ag.ContextWindow)
	}

	// Missing output metadata must not keep the previous model's cap;
	// a missing context window keeps the existing one.
	applyResolvedModel(ag, Resolved{Model: "other"})
	if ag.Model != "other" || ag.MaxTokens != 0 || ag.ContextWindow != 200000 {
		t.Fatalf("agent = model %q max %d ctx %d", ag.Model, ag.MaxTokens, ag.ContextWindow)
	}
}
