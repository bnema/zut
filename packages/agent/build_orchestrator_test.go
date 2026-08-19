package agent

import (
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
)

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
