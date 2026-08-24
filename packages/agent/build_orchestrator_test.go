package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
)

func TestResolveSeparatesInteractiveProactiveAndHeadlessStrictPolicies(t *testing.T) {
	root := t.TempDir()
	zutHome := filepath.Join(root, "zut-home")
	project := filepath.Join(root, "project")
	t.Setenv("ZUT_HOME", zutHome)
	t.Setenv("HOME", filepath.Join(root, "home"))
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	profilesDir := filepath.Join(root, "home", ".agents", "agents")
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profilesDir, "reviewer.md"), []byte("---\nname: reviewer\ndescription: Review delegated work\n---\nReview the requested scope."), 0o600); err != nil {
		t.Fatal(err)
	}
	enabled := true
	if err := SaveConfig(Config{AutoSubagentsEnabled: &enabled}); err != nil {
		t.Fatal(err)
	}

	base := Args{
		Provider: "ollama",
		Model:    "any-local-model",
		CWD:      project,
		NoLSP:    true,
		NoSkill:  true,
	}
	interactive, err := Resolve(base, false)
	if err != nil {
		t.Fatalf("resolve interactive: %v", err)
	}
	if !strings.Contains(interactive.SystemPrompt, ProactiveSubagentsSystemAddendum) || strings.Contains(interactive.SystemPrompt, StrictOrchestratorSystemAddendum) || !strings.Contains(interactive.SystemPrompt, "[subagents_list]") {
		t.Fatalf("interactive prompt did not select proactive primary policy and profiles:\n%s", interactive.SystemPrompt)
	}

	childArgs := residentChildArgs(base, "ollama", subagents.ResidentChildSpec{Provider: "ollama", Model: "any-local-model"})
	child, err := Resolve(childArgs, false)
	if err != nil {
		t.Fatalf("resolve resident child: %v", err)
	}
	if strings.Contains(child.SystemPrompt, ProactiveSubagentsSystemAddendum) || strings.Contains(child.SystemPrompt, StrictOrchestratorSystemAddendum) || strings.Contains(child.SystemPrompt, "[subagents_list]") {
		t.Fatalf("resident child inherited primary delegation policy or profiles:\n%s", child.SystemPrompt)
	}

	headlessArgs := base
	headlessArgs.Mode = ModePrint
	headlessArgs.Orchestrate = true
	headless, err := Resolve(headlessArgs, false)
	if err != nil {
		t.Fatalf("resolve headless orchestrator: %v", err)
	}
	if !strings.Contains(headless.SystemPrompt, StrictOrchestratorSystemAddendum) || strings.Contains(headless.SystemPrompt, ProactiveSubagentsSystemAddendum) {
		t.Fatalf("headless prompt did not select strict policy:\n%s", headless.SystemPrompt)
	}
}

func TestOrchestratorAlwaysReceivesResearchTools(t *testing.T) {
	for _, tc := range []struct {
		name          string
		args          Args
		wantResearch  bool
		wantWeb       bool
		wantWriteTool bool
	}{
		{
			name:          "requested allowlist",
			args:          Args{Orchestrate: true, ToolsSet: true, Tools: []string{"subagent_spawn"}},
			wantResearch:  true,
			wantWeb:       true,
			wantWriteTool: false,
		},
		{
			name:          "web search policy disabled",
			args:          Args{Orchestrate: true, WebSearchPolicy: subagents.WebSearchDeny},
			wantResearch:  true,
			wantWeb:       true,
			wantWriteTool: true,
		},
		{
			name:          "permission ceiling",
			args:          Args{Orchestrate: true, PermissionSet: &tools.PermissionSet{}},
			wantResearch:  true,
			wantWeb:       false,
			wantWriteTool: true,
		},
		{
			name:          "tools disabled",
			args:          Args{Orchestrate: true, NoTools: true, ToolsSet: true, Tools: []string{"write"}},
			wantResearch:  false,
			wantWeb:       false,
			wantWriteTool: false,
		},
		{
			name:          "normal session",
			args:          Args{ToolsSet: true, Tools: []string{"write"}},
			wantResearch:  false,
			wantWeb:       false,
			wantWriteTool: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := buildToolRegistry(tc.args, t.TempDir(), nil, false, false, false)

			if got := orchestratorExplorationToolsAllowed(tc.args); got != tc.wantResearch {
				t.Fatalf("orchestratorExplorationToolsAllowed = %v, want %v", got, tc.wantResearch)
			}
			_, gotRead := registry["read"]
			if gotRead != tc.wantResearch {
				t.Fatalf("read present = %v, want %v", gotRead, tc.wantResearch)
			}
			if _, ok := registry["grep"].(*tools.GrepTool); ok != tc.wantResearch {
				t.Fatalf("grep = %T, want research tool present = %v", registry["grep"], tc.wantResearch)
			}
			for _, name := range tools.WebCapabilityNames {
				_, gotWebTool := registry[name]
				if gotWebTool != tc.wantWeb {
					t.Fatalf("%s present = %v, want web tool present = %v", name, gotWebTool, tc.wantWeb)
				}
			}
			_, gotWrite := registry["write"]
			if gotWrite != tc.wantWriteTool {
				t.Fatalf("write present = %v, want %v", gotWrite, tc.wantWriteTool)
			}
		})
	}
}
