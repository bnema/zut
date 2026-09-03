package subagents

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverLoadsGlobalAgentsProfilesAndIgnoresPiProfiles(t *testing.T) {
	t.Setenv("ZUT_AGENT_PROFILES", "")
	home := t.TempDir()
	agentsDir := filepath.Join(home, ".agents", "agents")
	piDir := filepath.Join(home, ".pi", "agent", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(piDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeProfile(t, filepath.Join(agentsDir, "reviewer.md"), `---
name: reviewer
description: Read-only code reviewer
tools: read
model: openai-codex/gpt-5.6-luna
thinking: max
systemPromptMode: replace
inheritProjectContext: false
inheritSkills: false
fastMode: false
---
Review the requested scope without editing it.
`)
	writeProfile(t, filepath.Join(piDir, "implementer.md"), `---
name: implementer
description: Primary implementation agent
tools: [read, bash, edit, write]
model: openai-codex/gpt-5.6-luna
thinking: xhigh
---
Implement the requested change.
`)

	profiles, errs := Discover("", home)
	if len(errs) != 0 {
		t.Fatalf("Discover errors = %v", errs)
	}
	if len(profiles) != 1 {
		t.Fatalf("profiles = %d, want 1: %#v", len(profiles), profiles)
	}
	if profiles[0].Name != "reviewer" {
		t.Fatalf("profile = %q, want reviewer", profiles[0].Name)
	}
	reviewer := Find(profiles, "reviewer")
	if reviewer == nil {
		t.Fatal("reviewer profile missing")
	}
	if reviewer.SystemPromptMode != "replace" {
		t.Fatalf("system prompt mode = %q, want replace", reviewer.SystemPromptMode)
	}
	if reviewer.Model != "openai-codex/gpt-5.6-luna" || reviewer.Thinking != "max" {
		t.Fatalf("profile metadata = %#v", reviewer)
	}
	if reviewer.InheritProjectContext == nil || *reviewer.InheritProjectContext {
		t.Fatalf("inherit project context = %#v, want false", reviewer.InheritProjectContext)
	}
	if reviewer.InheritSkills == nil || *reviewer.InheritSkills {
		t.Fatalf("inherit skills = %#v, want false", reviewer.InheritSkills)
	}
	if reviewer.FastMode == nil || *reviewer.FastMode {
		t.Fatalf("fast mode = %#v, want false", reviewer.FastMode)
	}
	if implementer := Find(profiles, "implementer"); implementer != nil {
		t.Fatalf("Pi profile was discovered: %#v", implementer)
	}
}

func TestDiscoverResolvesConfiguredRelativePathAgainstCWD(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	t.Setenv("ZUT_AGENT_PROFILES", ".agents")
	writeProfile(t, filepath.Join(first, ".agents", "reviewer.md"), `---
name: reviewer
description: First session profile
---
First.
`)
	writeProfile(t, filepath.Join(second, ".agents", "reviewer.md"), `---
name: reviewer
description: Second session profile
---
Second.
`)

	for _, tc := range []struct {
		cwd         string
		description string
	}{
		{cwd: first, description: "First session profile"},
		{cwd: second, description: "Second session profile"},
	} {
		profiles, errs := Discover(tc.cwd, t.TempDir())
		if len(errs) != 0 {
			t.Fatalf("Discover(%q) errors = %v", tc.cwd, errs)
		}
		if len(profiles) != 1 || profiles[0].Description != tc.description {
			t.Fatalf("Discover(%q) = %#v, want %q", tc.cwd, profiles, tc.description)
		}
	}
}

func TestDiscoverConfiguredDirectoryWinsAndRejectsUnsafeNames(t *testing.T) {
	configured := t.TempDir()
	home := t.TempDir()
	t.Setenv("ZUT_AGENT_PROFILES", configured)
	writeProfile(t, filepath.Join(configured, "reviewer.md"), `---
name: reviewer
description: Configured reviewer
---
Configured instructions.
`)
	writeProfile(t, filepath.Join(filepath.Join(home, ".agents", "agents"), "reviewer.md"), `---
name: reviewer
description: Global reviewer
---
Global instructions.
`)
	writeProfile(t, filepath.Join(configured, "unsafe.md"), `---
name: ../unsafe
description: Should not load
---
No.
`)

	profiles, errs := Discover("", home)
	if len(errs) != 0 {
		t.Fatalf("Discover errors = %v", errs)
	}
	if len(profiles) != 1 || profiles[0].Description != "Configured reviewer" {
		t.Fatalf("configured precedence = %#v", profiles)
	}
}

func TestLoadRejectsUnclosedFrontmatter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.md")
	if err := os.WriteFile(path, []byte("---\nname: reviewer\nInstructions without a closing delimiter.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := load(path, "test"); err == nil || !strings.Contains(err.Error(), "missing closing delimiter") {
		t.Fatalf("load error = %v, want missing closing delimiter", err)
	}
}

func TestLoadRejectsInvalidClosedSchemaMetadata(t *testing.T) {
	cases := []struct {
		name  string
		field string
	}{
		{name: "invalid-thinking", field: "thinking: extreme"},
		{name: "invalid-reasoning", field: "reasoning: extreme"},
		{name: "qualified-model-with-provider", field: "provider: openai\nmodel: openai-codex/gpt-5"},
		{name: "invalid-prompt-mode", field: "systemPromptMode: merge"},
		{name: "invalid-project-context", field: "inheritProjectContext: sometimes"},
		{name: "invalid-skills", field: "inheritSkills: sometimes"},
		{name: "invalid-fast-mode", field: "fastMode: sometimes"},
		{name: "invalid-budget-ratio", field: "budgetRatio: 1.2"},
		{name: "invalid-budget-tokens", field: "budgetTokens: 0"},
		{name: "conflicting-budget", field: "budgetRatio: 0.5\nbudgetTokens: 1000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "profile.md")
			writeProfile(t, path, fmt.Sprintf("---\nname: test\n%s\n---\nInstructions.\n", tc.field))
			if _, err := load(path, "test"); err == nil {
				t.Fatalf("load succeeded for invalid metadata %q", tc.field)
			}
		})
	}
}

func TestLoadProfileBudgetOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.md")
	writeProfile(t, path, "---\nname: reviewer\nbudgetRatio: 0.6\n---\nInstructions.\n")
	profile, err := load(path, "test")
	if err != nil {
		t.Fatal(err)
	}
	if profile.BudgetRatio == nil || *profile.BudgetRatio != 0.6 || profile.BudgetTokens != nil {
		t.Fatalf("profile budget = ratio %v tokens %v", profile.BudgetRatio, profile.BudgetTokens)
	}
}

func TestProfileModelSelection(t *testing.T) {
	profile := &Profile{Model: "openrouter/anthropic/claude-sonnet"}
	provider, model := profile.ModelSelection()
	if provider != "openrouter" || model != "anthropic/claude-sonnet" {
		t.Fatalf("selection = (%q, %q)", provider, model)
	}

	profile = &Profile{Provider: "openai", Model: "gpt-5"}
	provider, model = profile.ModelSelection()
	if provider != "openai" || model != "gpt-5" {
		t.Fatalf("explicit selection = (%q, %q)", provider, model)
	}
}

func TestProfileToolsPreserveDeclarationAndResolve(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, filepath.Join(dir, "absent.md"), "---\nname: absent\n---\nInstructions.\n")
	writeProfile(t, filepath.Join(dir, "empty.md"), "---\nname: empty\ntools: []\n---\nInstructions.\n")
	writeProfile(t, filepath.Join(dir, "listed.md"), "---\nname: listed\ntools: [read, bash]\n---\nInstructions.\n")

	for _, tc := range []struct {
		name     string
		declared bool
		want     []string
	}{
		{name: "absent", declared: false, want: []string{"read", "bash", "lsp"}},
		{name: "empty", declared: true, want: []string{}},
		{name: "listed", declared: true, want: []string{"read", "bash"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := load(filepath.Join(dir, tc.name+".md"), "test")
			if err != nil {
				t.Fatal(err)
			}
			if profile.ToolsDeclared != tc.declared {
				t.Fatalf("ToolsDeclared = %v, want %v", profile.ToolsDeclared, tc.declared)
			}
			got, err := ResolveProfileTools(profile, []string{"read", "bash", "lsp"}, func(name string) bool { return name != "lsp" })
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("resolved tools = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestResolveProfileToolsIgnoresUnknownAndDeniedNames(t *testing.T) {
	profile := &Profile{Name: "reviewer", ToolsDeclared: true, Tools: []string{"read", "grep", "bash", "tasked_phases"}}
	got, err := ResolveProfileTools(profile, []string{"read", "bash"}, func(name string) bool { return name != "bash" })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"read"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("resolved tools = %#v, want %#v", got, want)
	}
}

func TestSystemPromptAddendumIsCompactAndDoesNotExposeBodyOrPath(t *testing.T) {
	profiles := []*Profile{{
		Name:         "reviewer",
		Description:  "Read-only\nreviewer",
		SystemPrompt: "secret instructions that belong only to the child",
		Model:        "openai-codex/gpt-5",
		Thinking:     "max",
		FastMode:     boolPtr(false),
		Tools:        []string{"read", "bash"},
		Path:         "/private/profile.md",
	}}
	got := SystemPromptAddendum(profiles)
	for _, want := range []string{"[subagents_list]", "[/subagents_list]", "reviewer", "Read-only reviewer", "model=openai-codex/gpt-5", "thinking=max", "fastMode=false"} {
		if !strings.Contains(got, want) {
			t.Fatalf("addendum missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"secret instructions", "/private/profile.md", "\nreviewer"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("addendum contains %q:\n%s", unwanted, got)
		}
	}
}

func boolPtr(value bool) *bool { return &value }

func writeProfile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
