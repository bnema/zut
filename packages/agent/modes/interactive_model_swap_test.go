package modes

import (
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

type metadataRecordingClient struct {
	provider.Client
	got []provider.Model
}

func (c *metadataRecordingClient) SetModelMetadata(m provider.Model) { c.got = append(c.got, m) }

// A same-provider /model switch reuses the agent, so it must also adopt the
// new model's output budget and context window. Keeping the previous model's
// MaxTokens truncated responses (e.g. a 4096 budget carried onto a 128k model).
func TestSameProviderModelSwapUpdatesModelLimits(t *testing.T) {
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

	client := &metadataRecordingClient{}
	ag := core.NewAgent(client, from, "", nil)
	ag.MaxTokens = fromModel.MaxOutput
	ag.ContextWindow = fromModel.ContextWindow
	i := &Interactive{agent: ag, cfg: InteractiveConfig{Provider: "anthropic", Model: from}}

	i.swapModel("anthropic", to, nil, false)

	if ag.Model != to {
		t.Fatalf("agent model = %q, want %q", ag.Model, to)
	}
	if ag.MaxTokens != toModel.MaxOutput {
		t.Fatalf("agent MaxTokens = %d, want %d", ag.MaxTokens, toModel.MaxOutput)
	}
	if ag.ContextWindow != toModel.ContextWindow {
		t.Fatalf("agent ContextWindow = %d, want %d", ag.ContextWindow, toModel.ContextWindow)
	}
	if len(client.got) != 1 || client.got[0].ID != to || client.got[0].MaxOutput != toModel.MaxOutput {
		t.Fatalf("client metadata = %+v, want %s", client.got, to)
	}
}
