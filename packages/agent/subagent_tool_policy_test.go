package agent

import (
	"testing"

	"github.com/bnema/zut/packages/agent/tools"
)

// TestSubagentActionAllowedMatrix pins the launch-time gating contract: a bare
// "subagent" entry grants every action, "subagent:<action>" grants one action,
// and every pre-existing disable path still disables all of them.
func TestSubagentActionAllowedMatrix(t *testing.T) {
	cases := []struct {
		name      string
		args      Args
		spawn     bool
		status    bool
		stop      bool
		resume    bool
		interrupt bool
	}{
		{name: "default grants every action", args: Args{}, spawn: true, status: true, stop: true, resume: true, interrupt: true},
		{name: "unrelated allowlist denies all", args: Args{ToolsSet: true, Tools: []string{"read"}}},
		{name: "bare subagent grants every action", args: Args{ToolsSet: true, Tools: []string{"subagent"}}, spawn: true, status: true, stop: true, resume: true, interrupt: true},
		{name: "bare subagent wins beside a suffix", args: Args{ToolsSet: true, Tools: []string{"read", "subagent:resume", "subagent"}}, spawn: true, status: true, stop: true, resume: true, interrupt: true},
		{name: "one action suffix grants one action", args: Args{ToolsSet: true, Tools: []string{"subagent:resume"}}, resume: true},
		{name: "interrupt suffix grants interrupt only", args: Args{ToolsSet: true, Tools: []string{"subagent:interrupt"}}, interrupt: true},
		{name: "two action suffixes grant two actions", args: Args{ToolsSet: true, Tools: []string{"subagent:spawn", "subagent:stop"}}, spawn: true, stop: true},
		{name: "unknown action suffix denies all", args: Args{ToolsSet: true, Tools: []string{"subagent:bogus"}}},
		{name: "empty allowlist denies all", args: Args{ToolsSet: true, Tools: []string{}}},
		{name: "no tools denies all", args: Args{NoTools: true}},
		{name: "packaged permission set denies all", args: Args{PermissionSet: &tools.PermissionSet{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := [5]bool{
				autoSubagentsToolAllowed(tc.args),
				autoSubagentsStatusToolAllowed(tc.args),
				autoSubagentsStopToolAllowed(tc.args),
				autoSubagentsResumeToolAllowed(tc.args),
				autoSubagentsInterruptToolAllowed(tc.args),
			}
			want := [5]bool{tc.spawn, tc.status, tc.stop, tc.resume, tc.interrupt}
			if got != want {
				t.Fatalf("actions = %v, want %v (args %+v)", got, want, tc.args)
			}
			wantAny := tc.spawn || tc.status || tc.stop || tc.resume || tc.interrupt
			if any := autoSubagentsAnyToolAllowed(tc.args); any != wantAny {
				t.Fatalf("autoSubagentsAnyToolAllowed = %v, want %v (args %+v)", any, wantAny, tc.args)
			}
		})
	}
}
