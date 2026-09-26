package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
	"github.com/mattn/go-runewidth"
)

// FuzzViewRowsStayTerminalSafe feeds untrusted text through every entry
// point that reaches the chat view and checks the rendered rows. Any path
// that forgets to sanitize shows up as a control byte or a misaligned
// box edge.
func FuzzViewRowsStayTerminalSafe(f *testing.F) {
	for _, seed := range []string{
		"plain text",
		"old=\"\"\"\t\t\tdrainAllFrames(sends)\n\tif tt.saturated {",
		"progress 10%\rprogress 100%\r\n",
		"\x1b[2J\x1b[H\x1b]0;title\x07\x1bPdcs\x1b\\bell\a back\b",
		"--- a.go\n+++ a.go\n@@ -1,2 +1,2 @@\n-\told\n+\tnew\n \tctx",
		"```go\nfunc main() {\n\tprintln(1)\n}\n```",
		"wide 日本語\tcol\x7f\u0085\xff",
		"\u0604",      // invisible format character
		"00\n\r0\t",   // CR inside a numbered-file gutter
		"&0&0\u05a30", // zero-width mark in a truncated tool header
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		for _, width := range []int{40, 100} {
			assertTerminalSafeRows(t, transcriptView(text).Build(width), width)
			for _, v := range liveViews(text) {
				assertTerminalSafeRows(t, v.BuildLive(width), width)
			}
		}
	})
}

func transcriptView(text string) *View {
	callArgs := func(k, v string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{k: v})
		return b
	}
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: text}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.TextBlock{Text: text},
			provider.ToolCallBlock{ID: "t1", Name: "bash", Arguments: callArgs("command", text)},
			provider.ToolCallBlock{ID: "t2", Name: "read", Arguments: callArgs("path", "a.go")},
			provider.ToolCallBlock{ID: "t3", Name: "custom", Arguments: callArgs("q", text)},
		}},
	}
	for id, result := range map[string]string{
		"t1": "$ " + text + "\n" + text + "\n[exit 0]",
		"t2": text,
		"t3": text,
	} {
		msgs = append(msgs, provider.Message{Role: provider.RoleTool, Content: []provider.Content{
			provider.ToolResultBlock{CallID: id, Content: []provider.Content{provider.TextBlock{Text: result}}},
		}})
	}
	return &View{Theme: Dark, ExpandAll: true, Messages: msgs, Err: text}
}

func liveViews(text string) []*View {
	var out []*View
	for _, tc := range []struct{ name, field string }{
		{"bash", "command"},
		{"write", "content"},
	} {
		args, _ := json.Marshal(map[string]string{tc.field: text, "path": "a.go"})
		out = append(out, &View{Theme: Dark, ToolCalls: []ToolCallView{{
			ID: "live", Name: tc.name, Args: ShortArgs(tc.name, args), RawJSONBuf: string(args), LivePath: "a.go",
		}}})
	}
	edit, _ := json.Marshal(map[string]any{"path": "a.go", "edits": []map[string]string{{"oldText": "x", "newText": text}}})
	out = append(out, &View{Theme: Dark, ToolCalls: []ToolCallView{{
		ID: "edit", Name: "edit", RawJSONBuf: string(edit), LivePath: "a.go",
	}}})
	out = append(out, &View{Theme: Dark, StreamingActive: true, Streaming: "x" + text, ToolCalls: []ToolCallView{{
		ID: "res", Name: "bash", Args: "cmd", Result: text, Done: true,
	}}})
	return out
}

// assertTerminalSafeRows checks the invariant the renderer relies on:
// once SGR color codes are removed, every row contains only printable
// text, fits the terminal, and box side rows reach the right edge.
func assertTerminalSafeRows(t *testing.T, rows []string, width int) {
	t.Helper()
	for i, row := range rows {
		plain := stripSGR(row)
		for _, r := range plain {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
				t.Fatalf("row %d contains control %U: %q", i, r, row)
			}
		}
		w := runewidth.StringWidth(plain)
		if w > width {
			t.Fatalf("row %d is %d cols wide, terminal is %d: %q", i, w, width, plain)
		}
		trimmed := strings.TrimSpace(plain)
		if strings.HasPrefix(trimmed, "│") && w != width {
			t.Fatalf("box row %d is %d cols wide, want %d: %q", i, w, width, plain)
		}
	}
}

// stripSGR removes only CSI ... m color sequences. Any other escape is
// left in place so the control check above flags it.
func stripSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] >= 0x30 && s[j] <= 0x3f {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
