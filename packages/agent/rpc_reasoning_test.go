package agent

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestRPCSetModelRejectsBusyTurn(t *testing.T) {
	var out bytes.Buffer
	s := &rpcServer{
		provider: provider.ProviderOpenCodeGo,
		model:    "old-model",
		agent:    &core.Agent{Model: "old-model", ContextWindow: 111, MaxTokens: 222},
		out:      &out,
	}
	s.turnMu.Lock()
	s.dispatch("set_model", "1", []byte(`{"model":"new-model"}`))
	s.turnMu.Unlock()
	if s.agent.Model != "old-model" || s.model != "old-model" {
		t.Fatalf("busy set_model changed active model: agent=%q server=%q", s.agent.Model, s.model)
	}
	if !strings.Contains(out.String(), `"success":false`) {
		t.Fatalf("busy set_model response = %q", out.String())
	}
}

func TestRPCSetModelAllowsUnlistedOpenCodeGoModel(t *testing.T) {
	preserveProviderCatalog(t)
	var out bytes.Buffer
	const model = "muse-spark-1.2-contributor"
	s := &rpcServer{
		provider: "opencode-go",
		model:    "kimi-k2.6",
		agent:    &core.Agent{Model: "kimi-k2.6"},
		out:      &out,
	}
	s.dispatch("set_model", "1", []byte(`{"model":"  `+model+`  "}`))

	if s.agent.Model != model || s.model != model {
		t.Fatalf("model = agent:%q server:%q, want trimmed %q", s.agent.Model, s.model, model)
	}
	if s.agent.ContextWindow != 128000 || s.agent.MaxTokens != 16384 {
		t.Fatalf("bootstrap limits = context %d output %d, want 128000/16384", s.agent.ContextWindow, s.agent.MaxTokens)
	}
	if !strings.Contains(out.String(), `"success":true`) || !strings.Contains(out.String(), `"model":"`+model+`"`) {
		t.Fatalf("response = %q", out.String())
	}
}

func TestRPCSetModelUsesKnownModelMetadata(t *testing.T) {
	preserveProviderCatalog(t)
	provider.SetLiveModels([]provider.Model{{
		Provider:      provider.ProviderOpenCodeGo,
		ID:            "served-model",
		ContextWindow: 300000,
		MaxOutput:     60000,
	}})

	var out bytes.Buffer
	s := &rpcServer{
		provider: provider.ProviderOpenCodeGo,
		model:    "old-model",
		agent:    &core.Agent{Model: "old-model", ContextWindow: 1, MaxTokens: 2},
		out:      &out,
	}
	s.dispatch("set_model", "1", []byte(`{"model":"served-model"}`))
	if s.agent.ContextWindow != 300000 || s.agent.MaxTokens != 60000 {
		t.Fatalf("known limits = context %d output %d, want 300000/60000", s.agent.ContextWindow, s.agent.MaxTokens)
	}
}

func TestRPCSetModelPreservesExistingLimitsWhenMetadataIsMissing(t *testing.T) {
	preserveProviderCatalog(t)
	provider.SetLiveModels([]provider.Model{{
		Provider: provider.ProviderOpenCodeGo,
		ID:       "metadata-without-limits",
	}})

	var out bytes.Buffer
	s := &rpcServer{
		provider: provider.ProviderOpenCodeGo,
		model:    "old-model",
		agent:    &core.Agent{Model: "old-model", ContextWindow: 111, MaxTokens: 222},
		out:      &out,
	}
	s.dispatch("set_model", "1", []byte(`{"model":"metadata-without-limits"}`))
	if s.agent.ContextWindow != 111 || s.agent.MaxTokens != 222 {
		t.Fatalf("missing limits changed agent values = context %d output %d, want 111/222", s.agent.ContextWindow, s.agent.MaxTokens)
	}
}

func TestRPCSetModelRejectsBlankOpenCodeGoModel(t *testing.T) {
	for _, model := range []string{"", "   "} {
		var out bytes.Buffer
		s := &rpcServer{
			provider: "opencode-go",
			model:    "kimi-k2.6",
			agent:    &core.Agent{Model: "kimi-k2.6"},
			out:      &out,
		}
		s.dispatch("set_model", "1", []byte(`{"model":"`+model+`"}`))
		if s.agent.Model != "kimi-k2.6" || s.model != "kimi-k2.6" {
			t.Fatalf("blank model %q changed active model: agent=%q server=%q", model, s.agent.Model, s.model)
		}
		if !strings.Contains(out.String(), `"success":false`) {
			t.Fatalf("blank model %q response = %q", model, out.String())
		}
	}
}

func TestRPCSetReasoningMax(t *testing.T) {
	var out bytes.Buffer
	s := &rpcServer{agent: &core.Agent{}, out: &out}
	s.dispatch("set_reasoning", "1", []byte(`{"reasoning":"max"}`))

	if s.agent.Reasoning != "max" {
		t.Fatalf("reasoning = %q, want max", s.agent.Reasoning)
	}
	if !strings.Contains(out.String(), `"reasoning":"max"`) {
		t.Fatalf("response = %q", out.String())
	}
}

func TestRPCSetReasoningRejectsUnknownLevel(t *testing.T) {
	var out bytes.Buffer
	s := &rpcServer{agent: &core.Agent{}, out: &out}
	s.dispatch("set_reasoning", "1", []byte(`{"reasoning":"extreme"}`))

	if s.agent.Reasoning != "" {
		t.Fatalf("reasoning = %q, want unchanged", s.agent.Reasoning)
	}
	if !strings.Contains(out.String(), `"success":false`) {
		t.Fatalf("response = %q", out.String())
	}
}
