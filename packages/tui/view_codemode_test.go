package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestShortArgsPythonCode(t *testing.T) {
	for _, tool := range []string{"codemode", "python"} {
		for _, tt := range []struct{ raw, want string }{
			{`{"code":"import time\n\n# comment\nr = zut.tools.bash(command='pwd')\nprint(r)"}`, "r = zut.tools.bash(command='pwd') (5 lines)"},
			{`{"code":"from os import path\nprint(\"hi`, `print("hi (2 lines)`},
			{`{"code":"import os\n# only setup\n"}`, "Python code (2 lines)"},
			{`{"code":"`, ""},
			{`{"code" : "print(1)"}`, "print(1) (1 lines)"},
		} {
			if got := ShortArgs(tool, json.RawMessage(tt.raw)); got != tt.want {
				t.Errorf("%s %s: got %q want %q", tool, tt.raw, got, tt.want)
			}
		}
	}
	t.Setenv("ZUT_TOOL_ARG_WIDTH", "20")
	raw, _ := json.Marshal(map[string]string{"code": strings.Repeat("界", 100)})
	got := ShortArgs("codemode", raw)
	if visibleWidth(got) > 20 || !strings.HasSuffix(got, "(1 lines)") {
		t.Fatalf("bad width: %q", got)
	}
}

func TestCodemodeTranscriptRecap(t *testing.T) {
	v := &View{Theme: Dark, ToolRecaps: map[string]string{"p": "✓ ↳ bash pwd\n1 calls"}, Messages: []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "p", Name: "codemode", Arguments: json.RawMessage(`{"code":"print(1)"}`)}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "p", Content: []provider.Content{provider.TextBlock{Text: "raw output"}}}}},
	}}
	for _, flat := range []bool{false, true} {
		v.FlatTools = flat
		rendered := strings.Join(v.Build(80), "\n")
		if !strings.Contains(rendered, "✓ ↳ bash pwd") || strings.Index(rendered, "✓ ↳ bash pwd") > strings.Index(rendered, "raw output") {
			t.Fatalf("missing/late recap: %s", rendered)
		}
	}
	lines := v.renderNestedActivity("✓ ↳ bash "+strings.Repeat("界", 100), 20)
	if visibleWidth(lines[0]) > 20 {
		t.Fatalf("activity too wide: %q", lines[0])
	}
}
