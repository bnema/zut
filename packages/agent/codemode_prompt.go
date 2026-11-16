package agent

import (
	"github.com/bnema/zut/packages/agent/mcp"
	"github.com/bnema/zut/packages/core"
)

// codemodeSystemAddendum stays conditional because /jail can change admission
// after this static prompt is assembled, just as it changes MCP deferral.
func codemodeSystemAddendum(reg core.Registry) string {
	if _, ok := reg["codemode"]; !ok {
		return ""
	}
	addendum := `[codemode]
When codemode is available (not jailed or permission-restricted), use it to batch 3+ dependent tool calls (grep → read matches, glob → filter → read, repeated bash) or post-process results in Python (filter, aggregate, diff). Return only a compact summary via zut.result(...)/print.
Use direct tools for a single read/edit/write or when you need to see output before deciding.
Workflow: zut.search(query) → zut.describe(name) → zut.call(name, **args) / zut.tools.<name>(**args). ToolResult has .text/.data/.is_error.
Nested calls follow normal confirmations; not callable inside: subagent, ask_user_question, plan, update_goal, schedule, sessions, codemode.
Tiny non-MCP example (Python; after discovering/describing grep and read):
hits = zut.tools.grep(pattern="TODO", path=".").data
paths = list(dict.fromkeys(hit["path"] for hit in hits))[:3]
texts = [zut.tools.read(path=path).text for path in paths]
zut.result({"files": paths, "todo_lines": [line for text in texts for line in text.splitlines() if "TODO" in line]})`
	for _, tool := range reg {
		if mcp.IsTool(tool) && mcp.DeferralConfigured(tool) {
			addendum += "\nWhen codemode is available (not jailed or permission-restricted), MCP tools are not directly callable; reach them only through zut.mcp.<server>.<method>(**args) / zut.call inside codemode. Otherwise MCP tools remain directly callable."
			break
		}
	}
	return addendum
}
