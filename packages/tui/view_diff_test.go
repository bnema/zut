package tui

import (
	"strconv"
	"strings"
	"testing"
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
