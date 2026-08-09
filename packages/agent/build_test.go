package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

type bundledSkillSource struct {
	bundled []*skills.Skill
}

func (s bundledSkillSource) Tools() []ExtensionToolInfo { return nil }
func (s bundledSkillSource) NewExtensionTool(ExtensionToolInfo) core.Tool {
	return nil
}
func (s bundledSkillSource) Skills() []*skills.Skill { return s.bundled }

type webSearchConflictSource struct{}

func (webSearchConflictSource) Tools() []ExtensionToolInfo {
	return []ExtensionToolInfo{{Extension: "test", Name: "web_search"}}
}
func (webSearchConflictSource) NewExtensionTool(ExtensionToolInfo) core.Tool {
	return &tools.ReadTool{}
}

type unorderedExtensionSource struct{}

func (unorderedExtensionSource) Tools() []ExtensionToolInfo {
	return []ExtensionToolInfo{
		{Extension: "test", Name: "zeta"},
		{Extension: "test", Name: "alpha"},
	}
}
func (unorderedExtensionSource) NewExtensionTool(ExtensionToolInfo) core.Tool {
	return &tools.ReadTool{}
}

func TestMergeExtensionToolsCreatesSkillToolWhenDiscoveryFoundNoSkills(t *testing.T) {
	r := Resolved{
		CWD:           t.TempDir(),
		ToolRegistry:  make(core.Registry),
		skillsEnabled: true,
	}
	bundled := &skills.Skill{Name: "bundled", Description: "bundled skill", Source: "extension tasked-phases"}

	r.MergeExtensionTools(bundledSkillSource{bundled: []*skills.Skill{bundled}})

	if r.SkillTool == nil {
		t.Fatal("MergeExtensionTools did not create a skill tool for bundled skills")
	}
	if _, ok := r.ToolRegistry[r.SkillTool.Name()]; !ok {
		t.Fatalf("skill tool %q was not registered", r.SkillTool.Name())
	}
	if got := r.SkillTool.Skills(); len(got) != 1 || got[0].Name != bundled.Name {
		t.Fatalf("merged bundled skills = %#v", got)
	}
}

func TestMergeExtensionToolsRespectsDisabledSkillDiscovery(t *testing.T) {
	r := Resolved{
		CWD:           t.TempDir(),
		ToolRegistry:  make(core.Registry),
		skillsEnabled: false,
	}
	bundled := &skills.Skill{Name: "bundled", Description: "bundled skill", Source: "extension tasked-phases"}

	r.MergeExtensionTools(bundledSkillSource{bundled: []*skills.Skill{bundled}})

	if r.SkillTool != nil {
		t.Fatal("disabled skill discovery created a skill tool")
	}
}

func TestMergeExtensionToolsReservesWebSearchName(t *testing.T) {
	r := Resolved{ToolRegistry: make(core.Registry)}
	r.MergeExtensionTools(webSearchConflictSource{})
	if _, ok := r.ToolRegistry["web_search"]; ok {
		t.Fatal("extension replaced reserved native web_search name")
	}
}

func TestMergeExtensionToolsKeepsDeterministicNativeSummaryOrder(t *testing.T) {
	r := Resolved{
		CWD:          t.TempDir(),
		ToolRegistry: buildToolRegistry(Args{}, t.TempDir(), nil, false, false, false),
	}
	r.MergeExtensionTools(unorderedExtensionSource{})

	var names []string
	for _, summary := range r.ToolSummary {
		names = append(names, summary.Name)
	}
	want := "read,write,edit,bash,create_worktree,web_search,alpha,zeta"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("merged tool summary order = %q, want %q", got, want)
	}

	r.MergeExtensionTools(unorderedExtensionSource{})
	names = names[:0]
	for _, summary := range r.ToolSummary {
		names = append(names, summary.Name)
	}
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("reloaded tool summary order = %q, want %q", got, want)
	}
}

func TestMergeExtensionToolsRetainsPonytailPromptAddendum(t *testing.T) {
	r := Resolved{
		CWD:           t.TempDir(),
		ToolRegistry:  make(core.Registry),
		skillsEnabled: true,
		systemAppend:  []string{PonytailSystemAddendum()},
	}
	bundled := &skills.Skill{Name: "bundled", Description: "bundled skill", Source: "extension tasked-phases"}

	r.MergeExtensionTools(bundledSkillSource{bundled: []*skills.Skill{bundled}})

	if !strings.Contains(r.SystemPrompt, PonytailSystemAddendum()) {
		t.Fatalf("extension prompt rebuild lost Ponytail addendum:\n%s", r.SystemPrompt)
	}
}

func TestBuildToolRegistryIncludesLSPAndWriteDiagnostics(t *testing.T) {
	root := t.TempDir()
	sandbox := tools.NewSandbox(root)
	registry := buildToolRegistry(Args{}, root, sandbox, true, true, false)
	lspTool, ok := registry["lsp"].(*tools.LSPTool)
	if !ok || lspTool.Manager == nil {
		t.Fatalf("lsp tool = %#v", registry["lsp"])
	}
	t.Cleanup(func() { _ = lspTool.Manager.Close() })
	if _, ok := registry["lsp"]; !ok {
		t.Fatal("default registry does not include lsp")
	}
	worktree, ok := registry["create_worktree"].(*tools.CreateWorktreeTool)
	if !ok || worktree.CWD != root || worktree.Sandbox != sandbox {
		t.Fatalf("default registry worktree tool = %#v", registry["create_worktree"])
	}
	if _, ok := registry["web_search"].(*tools.WebSearchTool); !ok {
		t.Fatalf("default registry web_search = %#v", registry["web_search"])
	}
	write, ok := registry["write"].(*tools.WriteTool)
	if !ok || write.LSP == nil || !write.LSPDiagnostics {
		t.Fatalf("write tool LSP wiring = %#v", write)
	}
	edit, ok := registry["edit"].(*tools.EditTool)
	if !ok || edit.LSP == nil || edit.LSPDiagnostics {
		t.Fatalf("edit tool LSP wiring = %#v", edit)
	}
	if disabled := buildToolRegistry(Args{}, root, tools.NewSandbox(root), false, true, true); len(disabled) != 6 {
		t.Fatalf("LSP-disabled registry has %d tools, want 6", len(disabled))
	}
	readOnly := buildToolRegistry(Args{Tools: []string{"read"}}, root, tools.NewSandbox(root), true, true, true)
	if read, ok := readOnly["read"].(*tools.ReadTool); !ok || read == nil {
		t.Fatalf("read-only registry = %#v", readOnly)
	}
	if _, ok := readOnly["web_search"]; ok {
		t.Fatalf("explicit non-matching tools list retained web search: %#v", readOnly)
	}
	webOnly := buildToolRegistry(Args{Tools: []string{"web_search"}}, root, tools.NewSandbox(root), true, true, true)
	if web, ok := webOnly["web_search"].(*tools.WebSearchTool); !ok || web == nil || len(webOnly) != 1 {
		t.Fatalf("web-search-only registry = %#v", webOnly)
	}
	workerMissingPolicy := buildToolRegistry(Args{Mode: ModeSubagentWorker}, root, tools.NewSandbox(root), false, false, false)
	if _, ok := workerMissingPolicy["web_search"]; ok {
		t.Fatal("worker registry enabled web_search without an explicit propagated allow")
	}
	workerAllowed := buildToolRegistry(Args{Mode: ModeSubagentWorker, WebSearchPolicy: subagents.WebSearchAllow}, root, tools.NewSandbox(root), false, false, false)
	if _, ok := workerAllowed["web_search"].(*tools.WebSearchTool); !ok {
		t.Fatalf("worker propagated allow registry = %#v", workerAllowed)
	}
	workerCapped := buildToolRegistry(Args{Mode: ModeSubagentWorker, ToolsSet: true, Tools: []string{"read"}, WebSearchPolicy: subagents.WebSearchAllow}, root, tools.NewSandbox(root), false, false, false)
	if _, ok := workerCapped["web_search"]; ok {
		t.Fatalf("worker tool list ceiling was bypassed: %#v", workerCapped)
	}
	worktreeOnly := buildToolRegistry(Args{Tools: []string{"create_worktree"}}, root, tools.NewSandbox(root), true, true, true)
	if worktree, ok := worktreeOnly["create_worktree"].(*tools.CreateWorktreeTool); !ok || worktree == nil || len(worktreeOnly) != 1 {
		t.Fatalf("worktree-only registry = %#v", worktreeOnly)
	}
}

func TestToolSummariesIncludeCreateWorktreeAndWebSearch(t *testing.T) {
	root := t.TempDir()
	registry := buildToolRegistry(Args{}, root, tools.NewSandbox(root), false, false, false)

	summaries := toolSummaries(registry, Args{})
	want := []string{"read", "write", "edit", "bash", "create_worktree", "web_search"}
	if len(summaries) != len(want) {
		t.Fatalf("tool summaries = %#v, want %v", summaries, want)
	}
	for index, summary := range summaries {
		if summary.Name != want[index] {
			t.Fatalf("tool summaries[%d] = %q, want %q", index, summary.Name, want[index])
		}
	}
}

func TestResolvedUseSandboxUpdatesCreateWorktreeTool(t *testing.T) {
	root := t.TempDir()
	previous := tools.NewSandbox(root)
	shared := tools.NewSandbox(root)
	worktree := &tools.CreateWorktreeTool{CWD: root, Sandbox: previous}
	resolved := &Resolved{
		Sandbox:      previous,
		ToolRegistry: core.NewRegistry(worktree),
	}

	resolved.UseSandbox(shared)

	if resolved.Sandbox != shared {
		t.Fatal("resolved sandbox was not replaced")
	}
	if worktree.Sandbox != shared {
		t.Fatal("worktree tool sandbox was not replaced")
	}
}

func TestAutoSubagentsToolPoliciesTrackEachTool(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tools      []string
		toolsSet   bool
		wantSpawn  bool
		wantStatus bool
		wantStop   bool
		wantResume bool
		wantAny    bool
	}{
		{name: "default", wantSpawn: true, wantStatus: true, wantStop: true, wantResume: true, wantAny: true},
		{name: "explicit empty", toolsSet: true},
		{name: "spawn", tools: []string{"subagent_spawn"}, wantSpawn: true, wantAny: true},
		{name: "status", tools: []string{"subagent_status"}, wantStatus: true, wantAny: true},
		{name: "stop", tools: []string{"subagent_stop"}, wantStop: true, wantAny: true},
		{name: "resume", tools: []string{"subagent_resume"}, wantResume: true, wantAny: true},
		{name: "other", tools: []string{"read"}},
		{name: "no tools", wantAny: false, tools: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := Args{Tools: tc.tools, ToolsSet: tc.toolsSet}
			if tc.name == "no tools" {
				args.NoTools = true
			}
			if got := autoSubagentsToolAllowed(args); got != tc.wantSpawn {
				t.Fatalf("spawn allowed = %v, want %v", got, tc.wantSpawn)
			}
			if got := autoSubagentsStatusToolAllowed(args); got != tc.wantStatus {
				t.Fatalf("status allowed = %v, want %v", got, tc.wantStatus)
			}
			if got := autoSubagentsStopToolAllowed(args); got != tc.wantStop {
				t.Fatalf("stop allowed = %v, want %v", got, tc.wantStop)
			}
			if got := autoSubagentsResumeToolAllowed(args); got != tc.wantResume {
				t.Fatalf("resume allowed = %v, want %v", got, tc.wantResume)
			}
			if got := autoSubagentsAnyToolAllowed(args); got != tc.wantAny {
				t.Fatalf("any allowed = %v, want %v", got, tc.wantAny)
			}
		})
	}
}

func TestReadAgentsContextLoadsGlobalAndAncestors(t *testing.T) {
	root := t.TempDir()
	zutHome := filepath.Join(root, "zut-home")
	project := filepath.Join(root, "repo")
	nested := filepath.Join(project, "packages", "app")
	if err := os.MkdirAll(zutHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zutHome, "AGENTS.md"), []byte("global rule"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "AGENTS.md"), []byte("repo rule"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "AGENTS.md"), []byte("app rule"), 0o644); err != nil {
		t.Fatal(err)
	}

	files := loadAgentsContext(nested, zutHome)
	if len(files) != 3 {
		t.Fatalf("loaded %d context files, want 3: %#v", len(files), files)
	}
	wantPaths := []string{
		filepath.Join(zutHome, "AGENTS.md"),
		filepath.Join(project, "AGENTS.md"),
		filepath.Join(nested, "AGENTS.md"),
	}
	for idx, want := range wantPaths {
		if files[idx].Path != want {
			t.Fatalf("context file %d path = %q, want %q", idx, files[idx].Path, want)
		}
	}

	got := formatAgentsContext(files)
	for _, want := range []string{"global rule", "repo rule", "app rule"} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatAgentsContext missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, "global rule") > strings.Index(got, "repo rule") || strings.Index(got, "repo rule") > strings.Index(got, "app rule") {
		t.Fatalf("AGENTS.md files loaded in wrong order:\n%s", got)
	}
}

func TestFindSubagentProfileReportsDiscoveryFailure(t *testing.T) {
	profileSource := filepath.Join(t.TempDir(), "profiles.md")
	if err := os.WriteFile(profileSource, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZUT_AGENT_PROFILES", profileSource)

	profile, err := findSubagentProfile("", "reviewer")
	if profile != nil {
		t.Fatalf("profile = %#v, want nil", profile)
	}
	if err == nil || !strings.Contains(err.Error(), "discover subagent profiles") {
		t.Fatalf("error = %v, want discovery failure", err)
	}
	if strings.Contains(err.Error(), profileSource) {
		t.Fatalf("error leaked profile path: %v", err)
	}
}

func TestResolveOrchestratedParentPromptScopesManifestAndContract(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	project := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(home, ".agents", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("ZUT_HOME", filepath.Join(root, "zut-home"))
	t.Setenv("ZUT_AGENT_PROFILES", filepath.Join(home, ".agents", "agents"))
	if err := os.WriteFile(filepath.Join(home, ".agents", "agents", "reviewer.md"), []byte("---\nname: reviewer\ndescription: Read-only reviewer\n---\nReview only.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	parent, err := Resolve(Args{CWD: project, Mode: ModePrint, Orchestrate: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(parent.SystemPrompt, "[subagents_list]"); got != 1 {
		t.Fatalf("parent profile manifest count = %d, want 1\n%s", got, parent.SystemPrompt)
	}
	if !strings.Contains(parent.SystemPrompt, "primary-agent orchestrator") {
		t.Fatalf("parent prompt omitted strict orchestrator contract:\n%s", parent.SystemPrompt)
	}

	child, err := Resolve(Args{CWD: project, Mode: ModePrint, Orchestrate: true, Subagent: "reviewer"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(child.SystemPrompt, "primary-agent orchestrator") || strings.Contains(child.SystemPrompt, "[subagents_list]") {
		t.Fatalf("selected child inherited parent orchestration instructions:\n%s", child.SystemPrompt)
	}
	if !strings.Contains(child.SystemPrompt, "Review only.") {
		t.Fatalf("selected child profile prompt missing:\n%s", child.SystemPrompt)
	}
}

func TestResolveIncludesNamedSubagentsListWhenAutoSubagentsIsEnabled(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	zutHome := filepath.Join(root, "zut-home")
	project := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(home, ".agents", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("ZUT_HOME", zutHome)
	t.Setenv("ZUT_AGENT_PROFILES", filepath.Join(home, ".agents", "agents"))
	enabled := true
	if err := SaveConfig(Config{Provider: "openai", Model: "gpt-5", AutoSubagentsEnabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	profile := `---
name: reviewer
description: Read-only code reviewer
thinking: high
---
Review without editing.
`
	if err := os.WriteFile(filepath.Join(home, ".agents", "agents", "reviewer.md"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{CWD: project}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[subagents_list]",
		"reviewer",
		"Read-only code reviewer",
		"[/subagents_list]",
		"primary-agent orchestrator",
		"Delegate all implementation, debugging/testing, and code-review work",
		"appropriately named subagent profile",
		"general worker",
		"Do not write or edit code yourself",
		"direct implementation tool calls",
		"inspect or review code",
		"apply worker patches",
		"Shared-worktree",
		"isolation:\"worktree\"",
		"Child workers cannot recursively spawn more sub-agents in v1",
	} {
		if !strings.Contains(r.SystemPrompt, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, r.SystemPrompt)
		}
	}
}

func TestResolveKeepsStrictContractButOmitsProfilesWhenSpawnIsUnavailable(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	zutHome := filepath.Join(root, "zut-home")
	project := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(home, ".agents", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("ZUT_HOME", zutHome)
	t.Setenv("ZUT_AGENT_PROFILES", filepath.Join(home, ".agents", "agents"))
	enabled := true
	if err := SaveConfig(Config{Provider: "openai", Model: "gpt-5", AutoSubagentsEnabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	profile := "---\nname: reviewer\ndescription: Read-only code reviewer\n---\nReview without editing.\n"
	if err := os.WriteFile(filepath.Join(home, ".agents", "agents", "reviewer.md"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args Args
	}{
		{name: "no tools", args: Args{CWD: project, NoTools: true}},
		{name: "read allowlist", args: Args{CWD: project, Tools: []string{"read"}, ToolsSet: true}},
		{name: "status allowlist", args: Args{CWD: project, Tools: []string{"subagent_status"}, ToolsSet: true}},
		{name: "permission set", args: Args{CWD: project, PermissionSet: &tools.PermissionSet{}}},
		{name: "headless orchestration", args: Args{CWD: project, Mode: ModePrint, Orchestrate: true, NoTools: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Resolve(tc.args, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"primary-agent orchestrator",
				"Do not write or edit code yourself",
				"subagent_spawn",
				"report this limitation to the user",
				"Completion is host-event-driven",
			} {
				if !strings.Contains(r.SystemPrompt, want) {
					t.Fatalf("system prompt missing %q:\n%s", want, r.SystemPrompt)
				}
			}
			for _, unwanted := range []string{"[subagents_list]", "reviewer", "Read-only code reviewer"} {
				if strings.Contains(r.SystemPrompt, unwanted) {
					t.Fatalf("system prompt advertises unusable profile %q:\n%s", unwanted, r.SystemPrompt)
				}
			}
		})
	}
}

func TestAutoSubagentsSystemAddendumRequiresHostCompletionUpdate(t *testing.T) {
	for _, want := range []string{
		"bash sleep",
		"watch",
		"tail -f",
		"polling loops",
		"repeated \"subagent_status\"",
		"dashboard, metadata, event-log, or file checks",
		"unrelated independent tasks",
		"end or yield your turn",
		"[auto-subagents update]",
		"Completion updates are the only completion signal",
		"user-requested commands, provider flows, extensions, or tests",
	} {
		if !strings.Contains(AutoSubagentsSystemAddendum, want) {
			t.Fatalf("auto-subagents contract missing %q:\n%s", want, AutoSubagentsSystemAddendum)
		}
	}
}

func TestResolveKeepsNamedSubagentsListWithoutOrchestratorContractWhenDisabled(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	zutHome := filepath.Join(root, "zut-home")
	project := filepath.Join(root, "repo")
	profilesDir := filepath.Join(home, ".agents", "agents")
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("ZUT_HOME", zutHome)
	t.Setenv("ZUT_AGENT_PROFILES", profilesDir)
	disabled := false
	if err := SaveConfig(Config{Provider: "openai", Model: "gpt-5", AutoSubagentsEnabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	profile := "---\nname: reviewer\ndescription: Read-only code reviewer\n---\nReview without editing.\n"
	if err := os.WriteFile(filepath.Join(profilesDir, "reviewer.md"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{CWD: project}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{
		AutoSubagentsSystemAddendum,
		"primary-agent orchestrator",
	} {
		if strings.Contains(r.SystemPrompt, unwanted) {
			t.Fatalf("disabled system prompt contains auto-subagents contract %q:\n%s", unwanted, r.SystemPrompt)
		}
	}
	for _, required := range []string{
		"[subagents_list]",
		"reviewer",
		"Read-only code reviewer",
		"[/subagents_list]",
		"user asks you to delegate",
		"active skill workflow requires delegation",
	} {
		if !strings.Contains(r.SystemPrompt, required) {
			t.Fatalf("disabled system prompt omits available delegation context %q:\n%s", required, r.SystemPrompt)
		}
	}
}

func TestResolveAppliesSelectedSubagentProfile(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	zutHome := filepath.Join(root, "zut-home")
	project := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(home, ".agents", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("ZUT_HOME", zutHome)
	t.Setenv("ZUT_AGENT_PROFILES", filepath.Join(home, ".agents", "agents"))
	if err := SaveConfig(Config{Provider: "openai", Model: "gpt-5", Reasoning: "high"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zutHome, "AGENTS.md"), []byte("global context"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := `---
name: reviewer
description: Read-only code reviewer
tools: read
thinking: off
systemPromptMode: replace
inheritProjectContext: false
inheritSkills: false
---
You are a read-only reviewer.
`
	if err := os.WriteFile(filepath.Join(home, ".agents", "agents", "reviewer.md"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{CWD: project, Subagent: "reviewer"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Reasoning != "" {
		t.Fatalf("profile thinking=off did not override config reasoning: %q", r.Reasoning)
	}
	if _, ok := r.ToolRegistry["read"]; !ok || len(r.ToolRegistry) != 1 {
		t.Fatalf("profile tools = %#v, want only read", r.ToolRegistry)
	}
	if len(r.ContextFiles) != 0 {
		t.Fatalf("profile inheritProjectContext=false loaded %#v", r.ContextFiles)
	}
	if !strings.Contains(r.SystemPrompt, "You are a read-only reviewer.") || strings.Contains(r.SystemPrompt, "global context") {
		t.Fatalf("profile system prompt inheritance is wrong:\n%s", r.SystemPrompt)
	}
	if !strings.Contains(r.SystemPrompt, PonytailSystemAddendum()) {
		t.Fatalf("replace-mode profile lost global Ponytail addendum:\n%s", r.SystemPrompt)
	}
}

func TestResolveSubagentFastModeProfileOverridesHostSetting(t *testing.T) {
	cases := []struct {
		name         string
		hostFastMode bool
		profileFast  *bool
		argFastMode  bool
		argFastSet   bool
		wantFastMode bool
	}{
		{name: "unset inherits enabled host", hostFastMode: true, wantFastMode: true},
		{name: "unset inherits disabled host", hostFastMode: false, wantFastMode: false},
		{name: "false disables enabled host", hostFastMode: true, profileFast: boolPtr(false), wantFastMode: false},
		{name: "true enables disabled host", hostFastMode: false, profileFast: boolPtr(true), wantFastMode: true},
		{name: "false stays disabled with disabled host", hostFastMode: false, profileFast: boolPtr(false), wantFastMode: false},
		{name: "profile false cannot be bypassed", hostFastMode: true, profileFast: boolPtr(false), argFastMode: true, argFastSet: true, wantFastMode: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			zutHome := filepath.Join(root, "zut-home")
			project := filepath.Join(root, "repo")
			profilesDir := filepath.Join(home, ".agents", "agents")
			if err := os.MkdirAll(profilesDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(project, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Setenv("ZUT_HOME", zutHome)
			t.Setenv("ZUT_AGENT_PROFILES", profilesDir)

			cfg := Config{Provider: "openai", Model: "gpt-5"}
			cfg.FastMode = &tc.hostFastMode
			if err := SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			profile := "---\nname: reviewer\n"
			if tc.profileFast != nil {
				profile += fmt.Sprintf("fastMode: %t\n", *tc.profileFast)
			}
			profile += "---\nReview the requested scope.\n"
			if err := os.WriteFile(filepath.Join(profilesDir, "reviewer.md"), []byte(profile), 0o600); err != nil {
				t.Fatal(err)
			}

			r, err := Resolve(Args{
				CWD:         project,
				Subagent:    "reviewer",
				FastMode:    tc.argFastMode,
				FastModeSet: tc.argFastSet,
			}, false)
			if err != nil {
				t.Fatal(err)
			}
			if r.FastMode != tc.wantFastMode {
				t.Fatalf("FastMode = %v, want %v", r.FastMode, tc.wantFastMode)
			}
		})
	}
}

func TestReadAgentsContextMissingFilesIsEmpty(t *testing.T) {
	files := loadAgentsContext(t.TempDir(), t.TempDir())
	if len(files) != 0 {
		t.Fatalf("expected no context files, got %#v", files)
	}
	if got := formatAgentsContext(files); got != "" {
		t.Fatalf("expected no context, got %q", got)
	}
}

// TestResolveFallsBackWhenConfiguredModelIsGone reproduces the
// startup failure caught by the user's screenshot: the persisted
// config.json points at a model id that's no longer in the active
// catalogue (because they edited models.json or zut's bundled
// catalogue changed). Resolve must NOT error — strands the user
// with no way to fix it from the TUI — and should repair the config
// so the next launch is silent.
func TestResolveFallsBackWhenConfiguredModelIsGone(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	// Persist a stale model id.
	stale := "gpt-5.5-pro-not-real"
	if err := SaveConfig(Config{Provider: "openai", Model: stale}); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{}, false)
	if err != nil {
		t.Fatalf("Resolve refused to launch with stale model: %v", err)
	}
	if r.Model == stale {
		t.Fatalf("Resolve kept stale model %q", r.Model)
	}
	if r.Provider != "openai" {
		t.Errorf("provider drifted: got %q; want openai", r.Provider)
	}

	// Config on disk should now hold the fallback so subsequent
	// launches don't repeat the warning.
	cfg, _ := LoadConfig()
	if cfg.Model == stale {
		t.Errorf("config.json still pins the stale model %q", cfg.Model)
	}
	if cfg.Model == "" {
		t.Errorf("config.json was emptied; expected the fallback model id")
	}
}

func TestResolveAppliesJailByDefault(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config Config
		want   bool
	}{
		{name: "missing defaults to unlocked", config: Config{Provider: "openai", Model: "gpt-5"}},
		{name: "enabled starts locked", config: Config{Provider: "openai", Model: "gpt-5", JailByDefault: boolPtr(true)}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ZUT_HOME", t.TempDir())
			t.Setenv("OPENAI_API_KEY", "test-key")
			if err := SaveConfig(tc.config); err != nil {
				t.Fatal(err)
			}

			r, err := Resolve(Args{}, false)
			if err != nil {
				t.Fatalf("Resolve failed: %v", err)
			}
			if got := r.Sandbox.Locked(); got != tc.want {
				t.Fatalf("sandbox locked = %v, want %v", got, tc.want)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

// TestResolveExplicitFlagStaleDoesNotRepairConfig confirms the
// repair-on-disk happens ONLY when the stale id came from the
// persisted config. If the user passed --model X explicitly and X is
// unknown, we still fall back, but we don't touch their config.
func TestResolveExplicitFlagStaleDoesNotRepairConfig(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	good := "gpt-5"
	if err := SaveConfig(Config{Provider: "openai", Model: good}); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{Model: "gpt-totally-fake"}, false)
	if err != nil {
		t.Fatalf("Resolve errored on unknown --model: %v", err)
	}
	if r.Model == "gpt-totally-fake" {
		t.Errorf("Resolve kept the bogus --model value")
	}
	cfg, _ := LoadConfig()
	if cfg.Model != good {
		t.Errorf("config.json was clobbered (was %q; now %q)", good, cfg.Model)
	}
}

func TestResolveOpenRouterPreservesSavedRoutedModelID(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	want := "deepseek/deepseek-v4-flash"
	if err := SaveConfig(Config{Provider: "openrouter", Model: want}); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{}, true)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.Provider != "openrouter" {
		t.Fatalf("provider = %q, want openrouter", r.Provider)
	}
	if r.Model != want {
		t.Fatalf("model = %q, want %q", r.Model, want)
	}
	if r.MaxOutput != 64000 {
		t.Fatalf("MaxOutput = %d, want synthetic gateway default 64000", r.MaxOutput)
	}
	if r.ContextWindow != 1_000_000 || r.NewAgent().ContextWindow != 1_000_000 {
		t.Fatalf("resolved/agent context window = %d/%d, want synthetic gateway metadata", r.ContextWindow, r.NewAgent().ContextWindow)
	}
}

func TestResolveGatewayPlainUnknownModelFallsBack(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	stale := "not-a-routed-model"
	if err := SaveConfig(Config{Provider: "openrouter", Model: stale}); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{}, true)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.Provider != "openrouter" {
		t.Fatalf("provider = %q, want openrouter", r.Provider)
	}
	if r.Model == stale || r.Model == "" {
		t.Fatalf("model = %q, want repaired gateway default", r.Model)
	}
}

// TestResolveEnvOnlyBedrockDiscoveredWithoutConfig reproduces issue
// #15: pointing ZUT_HOME at a fresh dir drops the persisted
// config.json (which pinned provider=amazon-bedrock). Resolve must
// still discover bedrock from the AWS env vars instead of falling back
// to anthropic and reporting "not logged in".
func TestResolveEnvOnlyBedrockDiscoveredWithoutConfig(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir()) // fresh home: no config.json
	// Disable the Kimi CLI token fallback so a developer machine with a
	// real Kimi CLI login doesn't pre-empt bedrock in the scan.
	if err := SetKimiCLIFallbackDisabled(true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test-bedrock-token")
	t.Setenv("AWS_REGION", "us-east-1")
	// Make sure no other provider's env credential pre-empts bedrock.
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "DEEPSEEK_API_KEY", "KIMI_API_KEY", "MOONSHOT_API_KEY", "XIAOMI_API_KEY", "OPENCODE_API_KEY"} {
		t.Setenv(k, "")
	}

	r, err := Resolve(Args{}, true)
	if err != nil {
		t.Fatalf("Resolve errored with env-only bedrock: %v", err)
	}
	if r.Provider != "amazon-bedrock" {
		t.Fatalf("provider = %q, want amazon-bedrock", r.Provider)
	}
	if !r.HasCredential() {
		t.Fatalf("bedrock credential not resolved from env")
	}
}

func TestResolveOllamaUsesModelBaseURLBeforeDefault(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	provider.SetLiveModels(nil)
	defer provider.SetLiveModels(nil)
	provider.SetUserModels([]provider.Model{{
		Provider:      "ollama",
		ID:            "qwen-local",
		DisplayName:   "Qwen Local",
		ContextWindow: 32768,
		MaxOutput:     8192,
		BaseURL:       "http://localhost:8000/v1",
	}})

	r, err := Resolve(Args{Provider: "ollama", Model: "qwen-local"}, false)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.BaseURL != "http://localhost:8000/v1" {
		t.Fatalf("BaseURL = %q, want models.json baseUrl", r.BaseURL)
	}
}

func TestResolveUsesInheritedSupervisorCredential(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "")

	r, err := Resolve(Args{
		Provider:            "openai",
		Model:               "gpt-5",
		inheritedCredential: "inherited-key",
		inheritedAuthMethod: "apikey",
		inheritedAccountID:  "account-id",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Credential != "inherited-key" || r.AuthMethod != "apikey" || r.AccountID != "account-id" {
		t.Fatalf("inherited credential was not preserved: %+v", r)
	}
}

func TestResolveLlamaCPPUsesRouterInferenceURL(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("LLAMA_BASE_URL", "http://127.0.0.1:8080/v1/")
	t.Setenv("LLAMA_API_KEY", "")
	provider.SetManagedModels(nil)
	t.Cleanup(func() { provider.SetManagedModels(nil) })

	r, err := Resolve(Args{Provider: "llama.cpp", Model: "local-model"}, true)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.BaseURL != "http://127.0.0.1:8080/v1" {
		t.Fatalf("BaseURL = %q", r.BaseURL)
	}
	if r.Credential != "local" || !r.HasCredential() {
		t.Fatalf("credential = %q", r.Credential)
	}
	if r.ContextWindow != 128000 || r.NewAgent().ContextWindow != 128000 {
		t.Fatalf("resolved/agent context window = %d/%d, want synthesized local metadata", r.ContextWindow, r.NewAgent().ContextWindow)
	}
	if got := r.NewClient().Name(); got != provider.LlamaCPPProviderID {
		t.Fatalf("client name = %q, want %q", got, provider.LlamaCPPProviderID)
	}
}

func TestResolveLlamaCPPUsesStoredLogin(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("LLAMA_BASE_URL", "")
	t.Setenv("LLAMA_API_KEY", "")
	if err := AuthStoreFor().SetEndpointCredential("llama.cpp", "http://localhost:9090", "stored-key"); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{Provider: "llama.cpp", Model: "stored-model"}, true)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.BaseURL != "http://localhost:9090/v1" || r.Credential != "stored-key" {
		t.Fatalf("resolved = base %q credential %q", r.BaseURL, r.Credential)
	}
}

func TestResolveCustomProviderModelBaseURLBeatsProviderBaseURL(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("MY_COMPANY_API_KEY", "test-key")
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{
		"providers": {
			"my-company": {
				"baseUrl": "https://provider.example.com/v1",
				"api": "openai",
				"models": [
					{"id": "fast", "baseUrl": "https://model.example.com/v1", "contextWindow": 65536}
				]
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	models, warnings := provider.LoadUserModelsWithWarnings(path)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	provider.SetLiveModels(nil)
	provider.SetUserModels(models)
	t.Cleanup(func() { provider.SetLiveModels(nil) })

	r, err := Resolve(Args{Provider: "my-company", Model: "fast"}, true)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.BaseURL != "https://model.example.com/v1" {
		t.Fatalf("BaseURL = %q, want model-level baseUrl", r.BaseURL)
	}
	if r.ContextWindow != 65536 || r.NewAgent().ContextWindow != 65536 {
		t.Fatalf("resolved/agent context window = %d/%d, want retained user-model metadata", r.ContextWindow, r.NewAgent().ContextWindow)
	}
}

func TestCustomProviderUsesOpenAIResponsesAPI(t *testing.T) {
	requestPath := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath <- r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{
		"providers": {
			"company-responses": {
				"baseUrl": "`+srv.URL+`/v1",
				"api": "openai-responses",
				"models": [{"id": "reasoning-model", "reasoning": true}]
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	models, warnings := provider.LoadUserModelsWithWarnings(path)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	provider.SetLiveModels(nil)
	provider.SetUserModels(models)
	t.Cleanup(func() { provider.SetLiveModels(nil) })

	r := Resolved{
		Provider:   "company-responses",
		Credential: "test-key",
		BaseURL:    srv.URL + "/v1",
	}
	events, err := r.NewClient().Stream(context.Background(), provider.Request{
		Model:    "reasoning-model",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "hello"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if got := <-requestPath; got != "/v1/responses" {
		t.Fatalf("request path = %q, want /v1/responses", got)
	}
}

func TestResolveCustomProviderInsecureFromModelsJSONBaseURL(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("LOCAL_PROXY_API_KEY", "test-key")
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{
		"providers": {
			"local-proxy": {
				"baseUrl": "https://proxy.example.com/v1",
				"api": "openai",
				"models": [{"id": "default"}]
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	models, warnings := provider.LoadUserModelsWithWarnings(path)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	provider.SetLiveModels(nil)
	provider.SetUserModels(models)
	t.Cleanup(func() { provider.SetLiveModels(nil) })

	r, err := Resolve(Args{Provider: "local-proxy", Model: "default", InsecureTLS: true}, true)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if !r.InsecureTLS {
		t.Fatal("InsecureTLS must be set for --insecure with models.json custom baseUrl")
	}
}

func TestResolveOllamaFallsBackToDefaultBaseURL(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	provider.SetLiveModels(nil)
	defer provider.SetLiveModels(nil)

	r, err := Resolve(Args{Provider: "ollama", Model: "any-local-model"}, false)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.BaseURL != "http://localhost:11434" {
		t.Fatalf("BaseURL = %q, want ollama default", r.BaseURL)
	}
}

func TestCanonicalProviderResolvesAliases(t *testing.T) {
	cases := map[string]string{
		"bedrock":         "amazon-bedrock",
		"AWS-Bedrock":     "amazon-bedrock",
		"  bedrock  ":     "amazon-bedrock",
		"vertex":          "google-vertex",
		"gemini":          "google",
		"azure":           "azure-openai-responses",
		"copilot":         "github-copilot",
		"codex":           "openai-codex",
		"moonshot":        "moonshotai",
		"vercel":          "vercel-ai-gateway",
		"hf":              "huggingface",
		"anthropic":       "anthropic",       // canonical passes through
		"amazon-bedrock":  "amazon-bedrock",  // already canonical
		"totally-unknown": "totally-unknown", // unknown returned unchanged (lowered)
		"Totally-UNKNOWN": "totally-unknown",
		"":                "",
	}
	for in, want := range cases {
		if got := canonicalProvider(in); got != want {
			t.Errorf("canonicalProvider(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalProviderAliasesAreKnown(t *testing.T) {
	for alias, canon := range providerAliases {
		if !isKnownProvider(canon) {
			t.Errorf("alias %q maps to %q which is not a known provider", alias, canon)
		}
	}
}

func TestResolveInsecureOnlyWithExplicitBaseURL(t *testing.T) {
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })

	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")

	resolved, err := Resolve(Args{Provider: "moonshotai", InsecureTLS: true}, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.InsecureTLS {
		t.Fatal("InsecureTLS must not be set for built-in provider base URLs")
	}
	assertDefaultTransportStillSecure(t)

	resolved, err = Resolve(Args{Provider: "openai", InsecureTLS: true, BaseURL: "https://my-llm.internal/v1"}, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !resolved.InsecureTLS {
		t.Fatal("InsecureTLS must be set with --insecure and explicit --base-url")
	}
	assertDefaultTransportStillSecure(t)
}

func TestResolveInsecureFromConfigRequiresExplicitBaseURL(t *testing.T) {
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })

	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	if err := SaveConfig(Config{Provider: "openai", Insecure: true}); err != nil {
		t.Fatal(err)
	}

	resolved, err := Resolve(Args{Provider: "openai"}, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.InsecureTLS {
		t.Fatal("InsecureTLS must not be set without a custom base URL")
	}
	assertDefaultTransportStillSecure(t)

	resolved, err = Resolve(Args{Provider: "openai", BaseURL: "https://my-llm.internal/v1"}, false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !resolved.InsecureTLS {
		t.Fatal("InsecureTLS must be set when config insecure=true and --base-url is provided")
	}
	assertDefaultTransportStillSecure(t)
}

func TestDefaultXAIModelIsGrok45(t *testing.T) {
	if got := defaultModelForProvider("xai"); got != "grok-4.5" {
		t.Fatalf("default xAI model = %q, want grok-4.5", got)
	}
}

func assertDefaultTransportStillSecure(t *testing.T) {
	t.Helper()
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return
	}
	if tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("http.DefaultTransport must not be made insecure")
	}
}

func TestResolveWebSearchRegistryRespectsConfigAndExplicitCLI(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	disabled := false
	if err := SaveConfig(Config{WebSearchEnabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	args := Args{CWD: t.TempDir(), Provider: "ollama", Model: "any-local-model"}
	resolved, err := Resolve(args, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resolved.ToolRegistry["web_search"]; ok {
		t.Fatal("persisted opt-out left web_search in the registry")
	}

	args.ToolsSet = true
	args.Tools = []string{"web_search"}
	resolved, err = Resolve(args, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resolved.ToolRegistry["web_search"]; !ok {
		t.Fatal("explicit --tools web_search did not override persisted opt-out")
	}

	args.PermissionSet = &tools.PermissionSet{}
	resolved, err = Resolve(args, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := resolved.ToolRegistry["web_search"]; ok {
		t.Fatal("packaged-agent PermissionSet received web_search")
	}
}

func TestResolveWebSearchPolicy(t *testing.T) {
	disabled := false
	cases := []struct {
		name    string
		args    Args
		cfg     Config
		cfgErr  error
		profile *subagents.Profile
		want    subagents.WebSearchPolicy
	}{
		{name: "legacy config", want: subagents.WebSearchAllow},
		{name: "persisted opt out", cfg: Config{WebSearchEnabled: &disabled}, want: subagents.WebSearchDeny},
		{name: "explicit CLI allow overrides opt out", args: Args{ToolsSet: true, Tools: []string{"web_search"}}, cfg: Config{WebSearchEnabled: &disabled}, want: subagents.WebSearchAllow},
		{name: "explicit empty CLI list denies", args: Args{ToolsSet: true}, want: subagents.WebSearchDeny},
		{name: "explicit nonmatching CLI list denies", args: Args{ToolsSet: true, Tools: []string{"read"}}, want: subagents.WebSearchDeny},
		{name: "no tools denies", args: Args{NoTools: true}, want: subagents.WebSearchDeny},
		{name: "packaged agent denies", args: Args{PermissionSet: &tools.PermissionSet{}}, want: subagents.WebSearchDeny},
		{name: "internal policy denies", args: Args{WebSearchPolicy: subagents.WebSearchDeny}, want: subagents.WebSearchDeny},
		{name: "internal policy allows", args: Args{WebSearchPolicy: subagents.WebSearchAllow}, cfg: Config{WebSearchEnabled: &disabled}, want: subagents.WebSearchAllow},
		{name: "normal explicit empty list narrows internal allow", args: Args{ToolsSet: true, WebSearchPolicy: subagents.WebSearchAllow}, want: subagents.WebSearchDeny},
		{name: "worker missing policy denies despite legacy config", args: Args{Mode: ModeSubagentWorker}, want: subagents.WebSearchDeny},
		{name: "worker explicit inherit denies", args: Args{Mode: ModeSubagentWorker, WebSearchPolicy: subagents.WebSearchInherit}, want: subagents.WebSearchDeny},
		{name: "worker invalid policy denies", args: Args{Mode: ModeSubagentWorker, WebSearchPolicy: subagents.WebSearchPolicy(99)}, want: subagents.WebSearchDeny},
		{name: "worker propagated deny remains denied", args: Args{Mode: ModeSubagentWorker, WebSearchPolicy: subagents.WebSearchDeny}, want: subagents.WebSearchDeny},
		{name: "worker propagated allow", args: Args{Mode: ModeSubagentWorker, WebSearchPolicy: subagents.WebSearchAllow}, cfg: Config{WebSearchEnabled: &disabled}, want: subagents.WebSearchAllow},
		{name: "worker explicit empty list caps allow", args: Args{Mode: ModeSubagentWorker, ToolsSet: true, WebSearchPolicy: subagents.WebSearchAllow}, want: subagents.WebSearchDeny},
		{name: "worker nonmatching list caps allow", args: Args{Mode: ModeSubagentWorker, ToolsSet: true, Tools: []string{"read"}, WebSearchPolicy: subagents.WebSearchAllow}, want: subagents.WebSearchDeny},
		{name: "worker matching list preserves allow", args: Args{Mode: ModeSubagentWorker, ToolsSet: true, Tools: []string{"read", "web_search"}, WebSearchPolicy: subagents.WebSearchAllow}, want: subagents.WebSearchAllow},
		{name: "worker no tools caps allow", args: Args{Mode: ModeSubagentWorker, NoTools: true, WebSearchPolicy: subagents.WebSearchAllow}, want: subagents.WebSearchDeny},
		{name: "worker permission set caps allow", args: Args{Mode: ModeSubagentWorker, PermissionSet: &tools.PermissionSet{}, WebSearchPolicy: subagents.WebSearchAllow}, want: subagents.WebSearchDeny},
		{name: "config load failure denies inherited policy", cfgErr: fmt.Errorf("broken config"), want: subagents.WebSearchDeny},
		{name: "named profile requires explicit tool", args: Args{WebSearchPolicy: subagents.WebSearchAllow}, profile: &subagents.Profile{Tools: []string{"read"}}, want: subagents.WebSearchDeny},
		{name: "named profile allows explicit tool", args: Args{WebSearchPolicy: subagents.WebSearchAllow}, profile: &subagents.Profile{Tools: []string{"read", "web_search"}}, want: subagents.WebSearchAllow},
		{name: "named profile cannot rescue worker inherit", args: Args{Mode: ModeSubagentWorker}, profile: &subagents.Profile{Tools: []string{"web_search"}}, want: subagents.WebSearchDeny},
		{name: "named profile cannot bypass worker tool list", args: Args{Mode: ModeSubagentWorker, ToolsSet: true, Tools: []string{"read"}, WebSearchPolicy: subagents.WebSearchAllow}, profile: &subagents.Profile{Tools: []string{"web_search"}}, want: subagents.WebSearchDeny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveWebSearchPolicy(tc.args, tc.cfg, tc.cfgErr, tc.profile); got != tc.want {
				t.Fatalf("policy = %v, want %v", got, tc.want)
			}
		})
	}
}
