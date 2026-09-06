package sdk

import (
	"testing"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestRuntimeSetModelAllowsUnlistedOpenCodeGoModel(t *testing.T) {
	const model = "muse-spark-1.2-contributor"
	r := &Runtime{
		provider: "opencode-go",
		model:    "kimi-k2.6",
		agent:    &core.Agent{Model: "kimi-k2.6"},
	}
	if err := r.SetModel("  " + model + "  "); err != nil {
		t.Fatal(err)
	}
	if r.model != model || r.agent.Model != model {
		t.Fatalf("model = runtime:%q agent:%q, want trimmed %q", r.model, r.agent.Model, model)
	}
	if r.agent.ContextWindow != 128000 || r.agent.MaxTokens != 16384 {
		t.Fatalf("bootstrap limits = context %d output %d, want 128000/16384", r.agent.ContextWindow, r.agent.MaxTokens)
	}
}

func TestRuntimeSetModelUpdatesAgentMetadata(t *testing.T) {
	const model = "served-opencode-model"
	r := &Runtime{
		provider: provider.ProviderOpenCodeGo,
		model:    "old-model",
		agent:    &core.Agent{Model: "old-model", ContextWindow: 1, MaxTokens: 2},
		modelCatalog: []provider.Model{{
			Provider:      provider.ProviderOpenCodeGo,
			ID:            model,
			ContextWindow: 200000,
			MaxOutput:     50000,
		}},
	}
	if err := r.SetModel(model); err != nil {
		t.Fatal(err)
	}
	if r.agent.ContextWindow != 200000 || r.agent.MaxTokens != 50000 {
		t.Fatalf("agent limits = context %d output %d, want 200000/50000", r.agent.ContextWindow, r.agent.MaxTokens)
	}
}

func TestRuntimeSetModelRejectsUnknownAuthoritativeModel(t *testing.T) {
	r := &Runtime{
		provider:                  provider.ProviderOpenCodeGo,
		model:                     "served-model",
		modelCatalogAuthoritative: true,
		modelCatalog:              []provider.Model{{Provider: provider.ProviderOpenCodeGo, ID: "served-model"}},
		agent:                     &core.Agent{Model: "served-model"},
	}
	if err := r.SetModel("removed-model"); err == nil {
		t.Fatal("authoritative SDK snapshot accepted removed model")
	}
	if r.model != "served-model" || r.agent.Model != "served-model" {
		t.Fatalf("rejected switch changed model: runtime=%q agent=%q", r.model, r.agent.Model)
	}
}

func TestRuntimeSetModelRejectsBlankOpenCodeGoModel(t *testing.T) {
	for _, model := range []string{"", "   "} {
		r := &Runtime{
			provider: "opencode-go",
			model:    "kimi-k2.6",
			agent:    &core.Agent{Model: "kimi-k2.6"},
		}
		if err := r.SetModel(model); err == nil {
			t.Fatalf("SetModel(%q) returned nil", model)
		}
		if r.model != "kimi-k2.6" || r.agent.Model != "kimi-k2.6" {
			t.Fatalf("blank model %q changed active model: runtime=%q agent=%q", model, r.model, r.agent.Model)
		}
	}
}

func TestRuntimeSetModelRejectsChangesWhileBusy(t *testing.T) {
	r := &Runtime{
		provider:     "opencode-go",
		model:        "old-model",
		activeCancel: func() {},
		agent:        &core.Agent{Model: "old-model"},
	}
	if err := r.SetModel("new-model"); err != ErrBusy {
		t.Fatalf("SetModel while busy = %v, want ErrBusy", err)
	}
	if r.model != "old-model" || r.agent.Model != "old-model" {
		t.Fatalf("busy SetModel changed model: runtime=%q agent=%q", r.model, r.agent.Model)
	}
}

func TestRuntimeSetReasoningMax(t *testing.T) {
	r := &Runtime{agent: &core.Agent{}}
	if err := r.SetReasoning("max"); err != nil {
		t.Fatal(err)
	}
	if r.agent.Reasoning != "max" {
		t.Fatalf("reasoning = %q, want max", r.agent.Reasoning)
	}
}

func TestRuntimeSetReasoningRejectsUnknownLevel(t *testing.T) {
	r := &Runtime{agent: &core.Agent{}}
	if err := r.SetReasoning("extreme"); err == nil {
		t.Fatal("expected invalid reasoning error")
	}
}

func TestNewRequiresExplicitWebSearchTool(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")

	withoutWeb, err := New(Config{Provider: "openai", Model: "gpt-5", Tools: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	defer withoutWeb.Close()
	for _, name := range tools.WebCapabilityNames {
		if _, ok := withoutWeb.agent.ToolsSnapshot()[name]; ok {
			t.Fatalf("SDK registry inherited %s without explicit Config.Tools opt-in", name)
		}
	}

	withWeb, err := New(Config{Provider: "openai", Model: "gpt-5", Tools: []string{"web_search"}})
	if err != nil {
		t.Fatal(err)
	}
	defer withWeb.Close()
	for _, name := range tools.WebCapabilityNames {
		if _, ok := withWeb.agent.ToolsSnapshot()[name]; !ok {
			t.Fatalf("SDK registry omitted %s from explicit web_search capability", name)
		}
	}
}
