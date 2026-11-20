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
