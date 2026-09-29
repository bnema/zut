package sdk

import (
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestRuntimeSetModelUpdatesMaxTokens(t *testing.T) {
	const to = "claude-opus-4-1-20250805"
	want, err := provider.FindModel("anthropic", to)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runtime{agent: &core.Agent{MaxTokens: 4096}, provider: "anthropic"}
	if err := r.SetModel(to); err != nil {
		t.Fatal(err)
	}
	if r.agent.MaxTokens != want.MaxOutput {
		t.Fatalf("MaxTokens = %d, want %d", r.agent.MaxTokens, want.MaxOutput)
	}
}
