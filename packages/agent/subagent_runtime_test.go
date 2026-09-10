package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
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

func TestResidentChildSpecTracksFastMode(t *testing.T) {
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args: Args{}, Root: t.TempDir(), RepoRoot: t.TempDir(),
		Provider: "openai", Model: "gpt-5.6-sol", FastMode: false,
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if spec.FastMode {
		t.Fatal("initial resident fast mode = true, want false")
	}
	runtime.SetFastMode(true)
	spec, err = runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if !spec.FastMode {
		t.Fatal("resident fast mode = false after update, want true")
	}
}

func TestResidentChildSpecCarriesNoCumulativeBudget(t *testing.T) {
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args: Args{}, Root: t.TempDir(), RepoRoot: t.TempDir(),
		Provider: "openai", Model: "gpt-5.6-sol", ContextWindow: 42_000,
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Tools) != 1 || spec.Tools[0] != "read" {
		t.Fatalf("child spec tools = %#v, want resolved tool list", spec)
	}
	// The budget fields are gone from the spec struct; the serialized
	// form must carry no budget bytes either.
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "budget") {
		t.Fatalf("child spec carries budget = %s", encoded)
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

func TestResidentChildSpecOmitsParentOnlyTools(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	cwd := t.TempDir()
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args:     Args{CWD: cwd, NoSkill: true, NoContextFiles: true, NoLSP: true},
		Root:     t.TempDir(),
		RepoRoot: cwd,
		Provider: "openai", Model: "gpt-5.6-sol",
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	parent := core.Registry{
		"read":           nil,
		"schedule":       nil,
		"host-only-tool": nil,
		"subagent_spawn": nil,
		"update_goal":    nil,
	}
	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, parent)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(spec.Tools, "schedule") || slices.Contains(spec.Tools, "host-only-tool") {
		t.Fatalf("generic child inherited parent-only tools: %v", spec.Tools)
	}
	if !slices.Contains(spec.Tools, "read") {
		t.Fatalf("generic child lost supported tool: %v", spec.Tools)
	}
	for _, name := range []string{"subagent_spawn", "update_goal"} {
		if slices.Contains(spec.Tools, name) {
			t.Fatalf("generic child kept forbidden tool %q: %v", name, spec.Tools)
		}
	}
	resolved, err := Resolve(residentChildArgs(runtime.args, runtime.credentialProvider, spec), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := residentChildRegistry(resolved.ToolRegistry, spec.Tools); err != nil {
		t.Fatalf("generic spec failed child registry validation: %v (tools=%v)", err, spec.Tools)
	}
}

func TestResidentChildSpecProfileToolDeclarations(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	cwd := t.TempDir()
	newRuntime := func(policy subagents.SubagentPolicy) *subagentRuntime {
		rt := newSubagentRuntime(subagentRuntimeConfig{
			Args:     Args{CWD: cwd, NoSkill: true, NoContextFiles: true, NoLSP: true},
			Root:     t.TempDir(),
			RepoRoot: cwd,
			Provider: "openai", Model: "gpt-5.6-sol",
			Policy: policy,
		})
		t.Cleanup(func() { _ = rt.Close(context.Background()) })
		return rt
	}
	parent := core.Registry{"read": nil, "bash": nil, "schedule": nil}

	t.Run("omitted inherits like generic", func(t *testing.T) {
		rt := newRuntime(subagents.SubagentPolicy{})
		generic, err := rt.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, parent)
		if err != nil {
			t.Fatal(err)
		}
		withProfile, err := rt.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{
			Task:    "review",
			Profile: &subagents.Profile{Name: "generic"},
		}, parent)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(generic.Tools, withProfile.Tools) {
			t.Fatalf("omitted profile tools = %v, want generic %v", withProfile.Tools, generic.Tools)
		}
	})

	t.Run("explicit keeps only supported permitted names", func(t *testing.T) {
		rt := newRuntime(subagents.SubagentPolicy{})
		spec, err := rt.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{
			Task: "review",
			Profile: &subagents.Profile{
				Name: "reviewer", ToolsDeclared: true,
				Tools: []string{"read", "schedule", "host-only-tool", "missing"},
			},
		}, parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(spec.Tools) != 1 || spec.Tools[0] != "read" {
			t.Fatalf("explicit profile tools = %v, want [read]", spec.Tools)
		}
	})

	t.Run("explicit empty stays empty", func(t *testing.T) {
		rt := newRuntime(subagents.SubagentPolicy{})
		spec, err := rt.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{
			Task:    "review",
			Profile: &subagents.Profile{Name: "empty", ToolsDeclared: true, Tools: []string{}},
		}, parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(spec.Tools) != 0 {
			t.Fatalf("explicit empty profile tools = %v, want []", spec.Tools)
		}
	})

	t.Run("policy denials hold", func(t *testing.T) {
		rt := newRuntime(subagents.SubagentPolicy{AllowedTools: []string{"read"}})
		spec, err := rt.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(spec.Tools) != 1 || spec.Tools[0] != "read" {
			t.Fatalf("policy-restricted tools = %v, want [read]", spec.Tools)
		}
		explicit, err := rt.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{
			Task: "review",
			Profile: &subagents.Profile{
				Name: "reviewer", ToolsDeclared: true,
				Tools: []string{"read", "bash"},
			},
		}, parent)
		if err != nil {
			t.Fatal(err)
		}
		if len(explicit.Tools) != 1 || explicit.Tools[0] != "read" {
			t.Fatalf("policy-restricted explicit tools = %v, want [read]", explicit.Tools)
		}
	})
}

func TestResidentChildSpecNeverGrantsToolsOutsideParent(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	cwd := t.TempDir()
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args:     Args{CWD: cwd, NoSkill: true, NoContextFiles: true, NoLSP: true},
		Root:     t.TempDir(),
		RepoRoot: cwd,
		Provider: "openai", Model: "gpt-5.6-sol",
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	spec, err := runtime.buildResidentChildSpec(context.Background(),
		tools.ResidentSpawnRequest{Task: "review"}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	// bash/write exist in the resolved child runtime but were never offered
	// by the parent catalogue; the child must not receive them.
	for _, name := range []string{"bash", "write", "schedule"} {
		if slices.Contains(spec.Tools, name) {
			t.Fatalf("child gained tool %q outside parent catalogue: %v", name, spec.Tools)
		}
	}
	explicit, err := runtime.buildResidentChildSpec(context.Background(),
		tools.ResidentSpawnRequest{
			Task: "review",
			Profile: &subagents.Profile{
				Name: "reviewer", ToolsDeclared: true,
				Tools: []string{"read", "bash"},
			},
		}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(explicit.Tools) != 1 || explicit.Tools[0] != "read" {
		t.Fatalf("explicit profile gained tool outside parent catalogue: %v", explicit.Tools)
	}
}

func TestResidentChildSpecDisabledSkillsDoNotLeak(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	cwd := t.TempDir()
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args:     Args{CWD: cwd, NoSkill: true, NoContextFiles: true, NoLSP: true},
		Root:     t.TempDir(),
		RepoRoot: cwd,
		Provider: "openai", Model: "gpt-5.6-sol",
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	inherit := false
	parent := core.Registry{"read": nil, "skill": nil}
	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{
		Task:    "review",
		Profile: &subagents.Profile{Name: "noskill", InheritSkills: &inherit},
	}, parent)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(spec.Tools, "skill") {
		t.Fatalf("skill-disabled child inherited skill tool: %v", spec.Tools)
	}
	resolved, err := Resolve(residentChildArgs(runtime.args, runtime.credentialProvider, spec), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resolved.ToolRegistry["skill"]; ok {
		t.Fatalf("skill-disabled child registry contains skill tool")
	}
	if _, err := residentChildRegistry(resolved.ToolRegistry, spec.Tools); err != nil {
		t.Fatalf("skill-disabled spec failed validation: %v", err)
	}
}
