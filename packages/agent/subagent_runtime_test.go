package agent

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
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

func TestResidentChildSpecInheritsAndOverridesRolloutBudget(t *testing.T) {
	ratio := 0.6
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args: Args{}, Root: t.TempDir(), RepoRoot: t.TempDir(),
		Provider: "openai", Model: "gpt-5.6-sol", ContextWindow: 500_000,
		Policy: subagents.SubagentPolicy{BudgetRatio: ratio, BudgetRatioConfigured: true},
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if spec.BudgetLimit != 300_000 || spec.BudgetRatio != ratio || spec.BudgetSource != "config_ratio" {
		t.Fatalf("inherited budget = %#v", spec)
	}

	tokens := int64(42_000)
	spec, err = runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{
		Task: "deep review", BudgetTokens: &tokens,
	}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if spec.BudgetLimit != tokens || spec.BudgetRatio != 0 || spec.BudgetSource != "spawn_tokens" {
		t.Fatalf("absolute budget = %#v", spec)
	}
}

func TestResolveResidentBudgetPrecedence(t *testing.T) {
	configRatio, profileRatio, spawnRatio := 0.6, 0.5, 0.4
	profileTokens, spawnTokens := int64(200_000), int64(150_000)
	for _, tc := range []struct {
		name        string
		profile     *subagents.Profile
		spawnRatio  *float64
		spawnTokens *int64
		wantLimit   int64
		wantRatio   float64
		wantSource  string
	}{
		{name: "default", wantLimit: 375_000, wantRatio: 0.75, wantSource: "default_ratio"},
		{name: "config ratio", wantLimit: 300_000, wantRatio: configRatio, wantSource: "config_ratio"},
		{name: "profile ratio", profile: &subagents.Profile{BudgetRatio: &profileRatio}, wantLimit: 250_000, wantRatio: profileRatio, wantSource: "profile_ratio"},
		{name: "profile tokens", profile: &subagents.Profile{BudgetTokens: &profileTokens}, wantLimit: profileTokens, wantSource: "profile_tokens"},
		{name: "spawn ratio over profile tokens", profile: &subagents.Profile{BudgetTokens: &profileTokens}, spawnRatio: &spawnRatio, wantLimit: 200_000, wantRatio: spawnRatio, wantSource: "spawn_ratio"},
		{name: "spawn tokens over profile ratio", profile: &subagents.Profile{BudgetRatio: &profileRatio}, spawnTokens: &spawnTokens, wantLimit: spawnTokens, wantSource: "spawn_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policyRatio := configRatio
			if tc.name == "default" {
				policyRatio = 0
			}
			limit, ratio, source, err := resolveResidentBudget(500_000, policyRatio, tc.name != "default", tc.profile, tc.spawnRatio, tc.spawnTokens)
			if err != nil {
				t.Fatal(err)
			}
			if limit != tc.wantLimit || ratio != tc.wantRatio || source != tc.wantSource {
				t.Fatalf("resolved budget = (%d, %v, %q), want (%d, %v, %q)", limit, ratio, source, tc.wantLimit, tc.wantRatio, tc.wantSource)
			}
		})
	}
	limit, ratio, source, err := resolveResidentBudget(500_000, 0.75, true, nil, nil, nil)
	if err != nil || limit != 375_000 || ratio != 0.75 || source != "config_ratio" {
		t.Fatalf("explicit default ratio = (%d, %v, %q, %v)", limit, ratio, source, err)
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
