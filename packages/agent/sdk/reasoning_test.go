package sdk

import (
	"testing"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

func TestRuntimeSetModelAllowsUnlistedOpenCodeGoModel(t *testing.T) {
	const model = "muse-spark-1.2-contributor"
	r := &Runtime{
		provider: "opencode-go",
		model:    "kimi-k2.6",
		agent:    &core.Agent{Model: "kimi-k2.6"},
	}
	if err := r.SetModel(model); err != nil {
		t.Fatal(err)
	}
	if r.model != model || r.agent.Model != model {
		t.Fatalf("model = runtime:%q agent:%q, want %q", r.model, r.agent.Model, model)
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
