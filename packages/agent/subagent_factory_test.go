package agent

import (
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
)

func TestResidentChildRegistryUsesExactToolListAndForbidsDelegation(t *testing.T) {
	catalogue := core.Registry{
		"read":           nil,
		"bash":           nil,
		"subagent_spawn": nil,
		"update_goal":    nil,
	}
	registry, err := residentChildRegistry(catalogue, []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	if len(registry) != 1 {
		t.Fatalf("registry = %#v", registry)
	}
	for _, name := range []string{"subagent_spawn", "update_goal", "missing"} {
		_, err := residentChildRegistry(catalogue, []string{name})
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("residentChildRegistry(%q) error = %v", name, err)
		}
	}
}

func TestResidentChildArgsPreserveDurableProfileInheritance(t *testing.T) {
	no := false
	next := residentChildArgs(Args{NoSkill: false, NoContextFiles: false, BaseURL: "https://stale.example/v1", InsecureTLS: false}, "openai", subagents.ResidentChildSpec{
		Provider: "openai", BaseURL: "https://current.example/v1", InsecureTLS: true, Model: "gpt-5", Workspace: "/repo/child",
		InheritSkills: &no, InheritProjectContext: &no,
	})
	if next.Provider != "openai" || next.BaseURL != "https://current.example/v1" || !next.InsecureTLS || next.Model != "gpt-5" || next.CWD != "/repo/child" || !next.NoSkill || !next.NoContextFiles {
		t.Fatalf("resident child args = %#v", next)
	}
}

func TestResidentChildArgsDoNotForwardCLIKeyAcrossProviders(t *testing.T) {
	parent := Args{Provider: "openai", APIKey: "parent-key"}
	child := subagents.ResidentChildSpec{Provider: "anthropic", Model: "claude"}
	if next := residentChildArgs(parent, "openai", child); next.APIKey != "" {
		t.Fatalf("cross-provider child inherited API key %q", next.APIKey)
	}
	child.Provider = "openai"
	if next := residentChildArgs(parent, "openai", child); next.APIKey != "parent-key" {
		t.Fatalf("same-provider child API key = %q, want parent key", next.APIKey)
	}
}
