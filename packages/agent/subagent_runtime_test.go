package agent

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

func TestExpandWebCapabilityTools(t *testing.T) {
	catalogue := append([]string{"read"}, tools.WebCapabilityNames...)
	got := expandWebCapabilityTools([]string{"read", "web_search"}, catalogue, func(string) bool { return true })
	for _, name := range append([]string{"read"}, tools.WebCapabilityNames...) {
		found := false
		for _, candidate := range got {
			if candidate == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expanded child tools = %v, missing %s", got, name)
		}
	}
}

func TestExpandWebCapabilityToolsPreservesPolicyAndNonWebSelections(t *testing.T) {
	catalogue := append([]string{"read"}, tools.WebCapabilityNames...)
	partial := expandWebCapabilityTools([]string{"web_open"}, catalogue, func(string) bool { return true })
	for _, name := range tools.WebCapabilityNames {
		found := false
		for _, candidate := range partial {
			if candidate == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("partial web selection did not expand: %v", partial)
		}
	}
	denied := expandWebCapabilityTools([]string{"read", "web_search"}, catalogue, func(name string) bool { return name == "read" })
	if len(denied) != 1 || denied[0] != "read" {
		t.Fatalf("denied web capability = %v, want [read]", denied)
	}
	nonWeb := expandWebCapabilityTools([]string{"read"}, catalogue, func(string) bool { return true })
	if len(nonWeb) != 1 || nonWeb[0] != "read" {
		t.Fatalf("non-web selection changed to %v", nonWeb)
	}
}

func TestResidentChildSpecSnapshotsCurrentProviderTransportSettings(t *testing.T) {
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args: Args{}, Root: t.TempDir(), RepoRoot: t.TempDir(),
		Provider: "openai", Model: "gpt-5.6-sol", BaseURL: "https://old.example/v1", InsecureTLS: false, ContextWindow: 500_000,
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	runtime.SetProviderSettings("https://current.example/v1", true)
	runtime.SetModel("gpt-current")
	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if spec.BaseURL != "https://current.example/v1" || !spec.InsecureTLS || spec.Model != "gpt-current" {
		t.Fatalf("resident spec transport/model = %q insecure=%t model=%q", spec.BaseURL, spec.InsecureTLS, spec.Model)
	}
}

func TestResidentChildSpecUsesSelectedModelContextForBudget(t *testing.T) {
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args: Args{}, Root: t.TempDir(), RepoRoot: t.TempDir(),
		Provider: "openai", Model: "gpt-5.6-sol", ContextWindow: 42_000,
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if spec.BudgetLimit != 500_000 || spec.BudgetSource != "model_context" {
		t.Fatalf("child model budget = %#v", spec)
	}
}

func TestResidentChildSpecDoesNotCarryTransportAcrossProviderOverride(t *testing.T) {
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args: Args{}, Root: t.TempDir(), RepoRoot: t.TempDir(),
		Provider: "openai", Model: "gpt-5.6-sol", BaseURL: "https://parent.example/v1", InsecureTLS: true, ContextWindow: 500_000,
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{
		Task: "review", Provider: "anthropic", Model: "claude-sonnet-4-5",
	}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if spec.BaseURL != "" || spec.InsecureTLS {
		t.Fatalf("cross-provider spec inherited transport: %#v", spec)
	}
}
