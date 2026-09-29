package modes

import (
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// A same-provider /model switch reuses the agent, so it must also adopt
// the new model's output budget. Keeping the previous model's MaxTokens
// truncated responses (e.g. a 4096 budget carried onto a 128k model).
func TestSameProviderModelSwapUpdatesMaxTokens(t *testing.T) {
	const from, to = "claude-3-5-haiku-20241022", "claude-opus-4-1-20250805"
	fromModel, err := provider.FindModel("anthropic", from)
	if err != nil {
		t.Fatal(err)
	}
	toModel, err := provider.FindModel("anthropic", to)
	if err != nil {
		t.Fatal(err)
	}
	if fromModel.MaxOutput == toModel.MaxOutput || toModel.MaxOutput <= 0 {
		t.Fatalf("test needs distinct positive limits, got %d and %d", fromModel.MaxOutput, toModel.MaxOutput)
	}

	ag := core.NewAgent(nil, from, "", nil)
	ag.MaxTokens = fromModel.MaxOutput
	i := &Interactive{agent: ag, cfg: InteractiveConfig{Provider: "anthropic", Model: from}}

	i.swapModel("anthropic", to, nil, false)

	if ag.Model != to {
		t.Fatalf("agent model = %q, want %q", ag.Model, to)
	}
	if ag.MaxTokens != toModel.MaxOutput {
		t.Fatalf("agent MaxTokens = %d, want %d", ag.MaxTokens, toModel.MaxOutput)
	}
}
