package agent

import (
	"encoding/json"
	"testing"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

// TestConfirmationKeyFor pins the confirmation grant key resolved at the tool
// call site: a multiplexed tool keys per action, a plain built-in keeps its
// name, and an unknown tool falls back to its raw name.
func TestConfirmationKeyFor(t *testing.T) {
	reg := core.Registry{
		tools.SubagentToolName: &tools.SubagentTool{},
		"read":                 &tools.GrepTool{},
	}
	cases := []struct {
		name string
		tool string
		args string
		want string
	}{
		{name: "facade action", tool: tools.SubagentToolName, args: `{"action":"spawn","task":"t"}`, want: "subagent:spawn"},
		{name: "facade plain action", tool: tools.SubagentToolName, args: `{"action":"resume","agent_id":"x","prompt":"p"}`, want: "subagent:resume"},
		{name: "built-in tool", tool: "read", args: `{"pattern":"x"}`, want: "read"},
		{name: "unknown tool", tool: "missing", args: `{}`, want: "missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := confirmationKeyFor(reg, tc.tool, json.RawMessage(tc.args)); got != tc.want {
				t.Fatalf("confirmationKeyFor(%q, %s) = %q, want %q", tc.tool, tc.args, got, tc.want)
			}
		})
	}
}
