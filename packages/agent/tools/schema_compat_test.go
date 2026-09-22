package tools

import (
	"encoding/json"
	"testing"
)

// TestToolSchemasAnthropicCompatible guards the wire contract with the
// Claude API: recent models (Opus 5.5, Fable 5.1) reject composition
// keywords (oneOf/anyOf/allOf) at the root of a tool input_schema with a
// 400. Every built-in schema must keep a plain object root.
func TestToolSchemasAnthropicCompatible(t *testing.T) {
	schemas := map[string]json.RawMessage{
		"ast":                 (&ASTTool{}).Schema(),
		"bash":                (&BashTool{}).Schema(),
		"create_worktree":     (&CreateWorktreeTool{}).Schema(),
		"edit":                (&EditTool{}).Schema(),
		"glob":                (&GlobTool{}).Schema(),
		"grep":                (&GrepTool{}).Schema(),
		"lsp":                 (&LSPTool{}).Schema(),
		"manage_worktrees":    (&ManageWorktreesTool{}).Schema(),
		"plan":                (&PlanTool{}).Schema(),
		"python":              (&PythonTool{}).Schema(),
		"read":                (&ReadTool{}).Schema(),
		"schedule":            (&ScheduleTool{}).Schema(),
		"subagent":            (&SubagentTool{}).Schema(),
		"telegram_send_file":  (&TelegramSendFileTool{}).Schema(),
		"telegram_send_image": (&TelegramSendImageTool{}).Schema(),
		"update_goal":         (&UpdateGoalTool{}).Schema(),
		"web_click":           (&WebClickTool{}).Schema(),
		"web_find":            (&WebFindTool{}).Schema(),
		"web_open":            (&WebOpenTool{}).Schema(),
		"web_search":          (&WebSearchTool{}).Schema(),
		"worktree":            (&WorktreeTool{}).Schema(),
		"write":               (&WriteTool{}).Schema(),
	}
	for name, raw := range schemas {
		var schema map[string]any
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s: invalid JSON schema: %v", name, err)
		}
		for _, key := range []string{"oneOf", "anyOf", "allOf"} {
			if _, ok := schema[key]; ok {
				t.Errorf("%s: top-level %q rejected by Claude API", name, key)
			}
		}
		if schema["type"] != "object" {
			t.Errorf("%s: root type = %v, want object", name, schema["type"])
		}
	}
}

// TestWebOpenExactlyOneRefOrURL keeps the exactly-one-of contract after
// the oneOf keyword was removed from the web_open schema (Claude API
// rejects top-level composition). parseWebOpenArgs remains the enforcer.
func TestWebOpenExactlyOneRefOrURL(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   string
		want  bool
		refID string
		url   string
	}{
		{"ref only", `{"ref_id":"web-1"}`, true, "web-1", ""},
		{"url only", `{"url":"https://example.com"}`, true, "", "https://example.com"},
		{"both", `{"ref_id":"web-1","url":"https://example.com"}`, false, "", ""},
		{"neither", `{}`, false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, ok := parseWebOpenArgs(json.RawMessage(tc.raw))
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v", ok, tc.want)
			}
			if ok && (args.refID != tc.refID || args.url != tc.url) {
				t.Fatalf("args = %+v", args)
			}
		})
	}
}
