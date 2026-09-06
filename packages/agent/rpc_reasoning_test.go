package agent

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
)

func TestRPCSetModelAllowsUnlistedOpenCodeGoModel(t *testing.T) {
	var out bytes.Buffer
	const model = "muse-spark-1.2-contributor"
	s := &rpcServer{
		provider: "opencode-go",
		model:    "kimi-k2.6",
		agent:    &core.Agent{Model: "kimi-k2.6"},
		out:      &out,
	}
	s.dispatch("set_model", "1", []byte(`{"model":"`+model+`"}`))

	if s.agent.Model != model || s.model != model {
		t.Fatalf("model = agent:%q server:%q, want %q", s.agent.Model, s.model, model)
	}
	if !strings.Contains(out.String(), `"success":true`) {
		t.Fatalf("response = %q", out.String())
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
