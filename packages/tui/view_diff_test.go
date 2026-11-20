package tui

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

// diffGutterNumbers returns the line numbers shown in the gutter of
// rendered diff rows, in order.
func diffGutterNumbers(rows []string) []int {
	var nums []int
	for _, r := range rows {
		fields := strings.Fields(stripANSI(r))
		if len(fields) > 0 && (fields[0] == "+" || fields[0] == "-") {
			fields = fields[1:]
		}
		if len(fields) == 0 {
			continue
		}
		if n, err := strconv.Atoi(fields[0]); err == nil {
			nums = append(nums, n)
		}
	}
	return nums
}

func TestRenderUnifiedDiffUsesHunkLineNumbers(t *testing.T) {
	v := &View{Theme: Dark}
	text := "@@ -114,3 +114,4 @@\n a\n b\n+c\n d\n"
	if !looksLikeUnifiedDiff(text) {
		t.Fatal("hunk-header diff not detected as a diff")
	}
	rows := v.renderUnifiedDiff(text, 80, "")
	got := diffGutterNumbers(rows)
	want := []int{114, 115, 116, 117}
	if len(got) != len(want) {
		t.Fatalf("numbers = %v, want %v\n%s", got, want, stripANSI(strings.Join(rows, "\n")))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("numbers = %v, want %v", got, want)
		}
	}
	if strings.Contains(stripANSI(strings.Join(rows, "\n")), "@@") {
		t.Fatal("hunk header should not be shown")
	}
}

func TestRenderUnifiedDiffSeparatesHunks(t *testing.T) {
	v := &View{Theme: Dark}
	text := "@@ -1,2 +1,2 @@\n a\n-b\n+B\n@@ -50,2 +50,2 @@\n-x\n+X\n y\n"
	rows := v.renderUnifiedDiff(text, 80, "")
	plain := stripANSI(strings.Join(rows, "\n"))
	if n := strings.Count(plain, "⋮"); n != 1 {
		t.Fatalf("separator count = %d, want 1\n%s", n, plain)
	}
	got := diffGutterNumbers(rows)
	want := []int{1, 2, 2, 50, 50, 51}
	if len(got) != len(want) {
		t.Fatalf("numbers = %v, want %v\n%s", got, want, plain)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("numbers = %v, want %v\n%s", got, want, plain)
		}
	}
}

func TestRenderUnifiedDiffLegacyEllipsis(t *testing.T) {
	v := &View{Theme: Dark}
	text := " a\n+b\n...\n c\n"
	if !looksLikeUnifiedDiff(text) {
		t.Fatal("legacy diff not detected")
	}
	plain := stripANSI(strings.Join(v.renderUnifiedDiff(text, 80, ""), "\n"))
	if !strings.Contains(plain, "...") || !strings.Contains(plain, "b") {
		t.Fatalf("legacy diff render lost rows:\n%s", plain)
	}
}

func TestRenderDiffRowTintsAddedRowAcrossResets(t *testing.T) {
	v := &View{Theme: Dark}
	const width = 60
	rows := v.renderDiffRow("+x := 1 // c", width, Dark.Tool, 3, '+', "a.go")
	if len(rows) != 1 {
		t.Fatalf("rows = %q", rows)
	}
	row := rows[0]
	bg := Dark.sgrBGColor(*Dark.DiffAddBG)
	if !strings.HasPrefix(row, "    "+bg) {
		t.Fatalf("indent tinted or row untinted: %q", row)
	}
	inner := strings.TrimSuffix(row, reset)
	if n := strings.Count(inner, reset); n == 0 || strings.Count(inner, reset+bg) != n {
		t.Fatalf("not every inner reset re-applies the tint: %q", row)
	}
	if got := visibleWidth(row); got != width {
		t.Fatalf("row width = %d, want %d", got, width)
	}
	del := v.renderDiffRow("-y", width, Dark.Error, 3, '-', "")
	if !strings.Contains(del[0], Dark.sgrBGColor(*Dark.DiffDelBG)) {
		t.Fatalf("removed row lacks the delete tint: %q", del[0])
	}
	ctx := v.renderDiffRow(" z", width, Dark.Muted, 3, ' ', "")
	if strings.Contains(ctx[0], "\x1b[48") {
		t.Fatalf("context row is tinted: %q", ctx[0])
	}
}

func TestRenderDiffRowNilTintsEmitNoBackground(t *testing.T) {
	th := Dark
	th.DiffAddBG, th.DiffDelBG = nil, nil
	v := &View{Theme: th}
	rows := v.renderUnifiedDiff("@@ -1,1 +1,1 @@\n-a := 1\n+a := 2\n", 60, "a.go")
	if joined := strings.Join(rows, "\n"); strings.Contains(joined, "\x1b[48") {
		t.Fatalf("nil tints still emit background SGR: %q", joined)
	}
}

func TestRenderDiffRowTintsWrappedRows(t *testing.T) {
	v := &View{Theme: Dark}
	const width = 30
	rows := v.renderDiffRow("+"+strings.Repeat("word ", 20), width, Dark.Tool, 1, '+', "")
	if len(rows) < 3 {
		t.Fatalf("long line did not wrap: %q", rows)
	}
	bg := Dark.sgrBGColor(*Dark.DiffAddBG)
	for i, r := range rows {
		if !strings.HasPrefix(r, "    "+bg) || visibleWidth(r) != width {
			t.Fatalf("wrapped row %d not fully tinted (width %d): %q", i, visibleWidth(r), r)
		}
	}
}

func TestClipToolRowKeepsTintReset(t *testing.T) {
	row := Dark.tintRow(Dark.FGColor(Dark.Tool, strings.Repeat("x", 40)), Dark.DiffAddBG, 40)
	clipped := clipToolRow(row, 20)
	if visibleWidth(clipped) != 20 || !strings.HasSuffix(clipped, reset) {
		t.Fatalf("clipped tinted row lost width or final reset: %q", clipped)
	}
}

// toolTranscript builds a finished call/result pair for transcript renders.
func toolTranscript(name, args, result string, isError bool) []provider.Message {
	return []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "c1", Name: name, Arguments: json.RawMessage(args)}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID: "c1", IsError: isError, Content: []provider.Content{provider.TextBlock{Text: result}},
		}}},
	}
}

func TestEditDiffNeverCollapses(t *testing.T) {
	var b strings.Builder
	b.WriteString("@@ -1,200 +1,200 @@\n")
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&b, "+row%d\n", i)
	}
	v := &View{Theme: Dark, Messages: toolTranscript("edit", `{"path":"a.txt"}`, b.String(), false)}
	plain := stripANSI(strings.Join(v.Build(100), "\n"))
	if strings.Contains(plain, "ctrl+o") {
		t.Fatalf("edit diff collapsed:\n%s", plain)
	}
	for _, want := range []string{"row1", "row100", "row200"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("missing %s", want)
		}
	}
}

func TestBashResultStillCollapses(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&b, "out %d\n", i)
	}
	v := &View{Theme: Dark, Messages: toolTranscript("bash", `{"command":"x"}`, b.String(), false)}
	if plain := stripANSI(strings.Join(v.Build(100), "\n")); !strings.Contains(plain, "ctrl+o to expand") {
		t.Fatalf("bash result did not collapse:\n%s", plain)
	}
}

func TestEditDiffDiagnosticsTrailerIsPlain(t *testing.T) {
	result := "@@ -1,1 +1,1 @@\n-a\n+b\n\nDiagnostics: 1 error (deduplicated 1)\n- a.go:1:1 boom\n"
	v := &View{Theme: Dark, Messages: toolTranscript("edit", `{"path":"a.go"}`, result, false)}
	rows := v.Build(80)
	addBG := Dark.sgrBGColor(*Dark.DiffAddBG)
	sawTint, sawDiag := false, false
	for _, r := range rows {
		plain := stripANSI(r)
		if strings.Contains(plain, "+ b") && strings.Contains(r, addBG) {
			sawTint = true
		}
		if strings.Contains(plain, "Diagnostics:") || strings.Contains(plain, "boom") {
			sawDiag = true
			if strings.Contains(r, "\x1b[48") {
				t.Fatalf("diagnostics row is tinted: %q", r)
			}
			if f := strings.Fields(plain); len(f) > 0 {
				if _, err := strconv.Atoi(f[0]); err == nil {
					t.Fatalf("diagnostics row is numbered: %q", plain)
				}
			}
		}
	}
	if !sawTint || !sawDiag {
		t.Fatalf("tinted=%v diagnostics=%v\n%s", sawTint, sawDiag, stripANSI(strings.Join(rows, "\n")))
	}
}

func TestWriteErrorRendersAsPlainText(t *testing.T) {
	v := &View{Theme: Dark, Messages: toolTranscript("write", `{"path":"a.go"}`, "permission denied\nsecond line", true)}
	rows := v.Build(80)
	joined := strings.Join(rows, "\n")
	if strings.Contains(stripANSI(joined), " + ") || strings.Contains(joined, "\x1b[48") {
		t.Fatalf("error write rendered as added rows:\n%q", joined)
	}
	if !strings.Contains(stripANSI(joined), "permission denied") {
		t.Fatalf("error text missing:\n%s", stripANSI(joined))
	}
}

func TestRenderAddedFileRows(t *testing.T) {
	v := &View{Theme: Dark}
	rows := v.renderAddedFile("package main\n\nfunc main() {}\n", "a.go", 80)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3:\n%s", len(rows), stripANSI(strings.Join(rows, "\n")))
	}
	if got := strings.TrimLeft(stripANSI(rows[0]), " "); !strings.HasPrefix(got, "1 + package main") {
		t.Fatalf("first row = %q", got)
	}
	addBG := Dark.sgrBGColor(*Dark.DiffAddBG)
	for i := 0; i < 3; i++ {
		if !strings.Contains(rows[i], addBG) {
			t.Fatalf("code row %d untinted: %q", i, rows[i])
		}
	}
}

func TestToolHeaderShowsDiffStats(t *testing.T) {
	result := "@@ -1,2 +1,4 @@\n a\n-b\n+c\n+d\n+e\n\nDiagnostics: 1 error (deduplicated 1)\n- x\n"
	v := &View{Theme: Dark, Messages: toolTranscript("edit", `{"path":"a.go"}`, result, false)}
	header := stripANSI(v.Build(80)[0])
	if !strings.HasSuffix(header, "+3 -1") {
		t.Fatalf("header = %q, want +3 -1 stats", header)
	}
}

func TestToolHeaderShowsWriteStats(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 412; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	v := &View{Theme: Dark, Messages: toolTranscript("write", `{"path":"a.txt"}`, b.String(), false)}
	if header := stripANSI(v.Build(80)[0]); !strings.HasSuffix(header, "+412") {
		t.Fatalf("header = %q, want +412", header)
	}
	live := &View{Theme: Dark}
	tc := ToolCallView{ID: "w", Name: "write", Args: "a.txt", Result: b.String(), Done: true}
	if header := stripANSI(live.renderToolCall(tc, 80)[0]); !strings.HasSuffix(header, "+412") {
		t.Fatalf("live header = %q, want +412", header)
	}
	tc.Error = true
	if header := stripANSI(live.renderToolCall(tc, 80)[0]); strings.Contains(header, "+412") {
		t.Fatalf("error header has stats: %q", header)
	}
}

func TestToolHeaderStatsSurviveNarrowWidth(t *testing.T) {
	stats := Dark.FGColor(Dark.Tool, "+3") + " " + Dark.FGColor(Dark.Error, "-1")
	header := stripANSI(toolHeaderLine(Dark, "edit some/very/long/path/to/a/file.go", stats, 30))
	if !strings.HasSuffix(header, "+3 -1") || !strings.Contains(header, "...") {
		t.Fatalf("header = %q", header)
	}
	if w := visibleWidth(header); w > 30 {
		t.Fatalf("header width %d > 30: %q", w, header)
	}
}

func TestASTDiffWithTruncationNoticeRendersAsDiff(t *testing.T) {
	text := "@@ -3,1 +3,1 @@\n-foo()\n+bar()\n\n... [truncated at 100 bytes]"
	if !looksLikeUnifiedDiff(text) {
		t.Fatal("truncated ast diff not detected as a diff")
	}
	v := &View{Theme: Dark, Messages: toolTranscript("ast", `{"path":".","rewrite":"bar()"}`, text, false)}
	out := stripANSI(strings.Join(v.Build(100), "\n"))
	if !strings.Contains(out, "+1 -1") {
		t.Fatalf("missing stats:\n%s", out)
	}
	if !strings.Contains(out, "3 + bar()") || strings.Contains(out, "@@") {
		t.Fatalf("not rendered as a diff:\n%s", out)
	}
	if !strings.Contains(out, "... [truncated at 100 bytes]") {
		t.Fatalf("truncation notice dropped:\n%s", out)
	}
}
