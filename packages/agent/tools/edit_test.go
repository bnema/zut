package tools

import (
	"fmt"
	"strings"
	"testing"
)

func numberedLines(n int, override map[int]string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if l, ok := override[i]; ok {
			b.WriteString(l)
		} else {
			fmt.Fprintf(&b, "line%d", i)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func hunkHeaders(diff string) []string {
	var out []string
	for _, l := range strings.Split(diff, "\n") {
		if strings.HasPrefix(l, "@@") {
			out = append(out, l)
		}
	}
	return out
}

func TestUnifiedDiffHunkHeaderUsesRealLineNumbers(t *testing.T) {
	a := numberedLines(200, map[int]string{117: "old117"})
	b := numberedLines(200, map[int]string{117: "new117"})
	diff := unifiedDiff("f.go", a, b)
	if !strings.HasPrefix(diff, "@@ -114,7 +114,7 @@\n") {
		t.Fatalf("diff does not start with the expected hunk header:\n%s", diff)
	}
	if !strings.Contains(diff, "\n-old117\n") || !strings.Contains(diff, "\n+new117\n") {
		t.Fatalf("diff lost the changed rows:\n%s", diff)
	}
}

func TestUnifiedDiffSeparatesDistantChangesIntoHunks(t *testing.T) {
	a := numberedLines(120, map[int]string{20: "oldA", 70: "oldB"})
	b := numberedLines(120, map[int]string{20: "newA", 70: "newB"})
	diff := unifiedDiff("f.go", a, b)
	got := hunkHeaders(diff)
	want := []string{"@@ -17,7 +17,7 @@", "@@ -67,7 +67,7 @@"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("headers = %q, want %q\n%s", got, want, diff)
	}
	if strings.Contains(diff, "...") {
		t.Fatalf("diff still uses the legacy ... separator:\n%s", diff)
	}
}

func TestUnifiedDiffInsertionAtTop(t *testing.T) {
	a := numberedLines(5, nil)
	b := "new1\nnew2\n" + a
	diff := unifiedDiff("f.go", a, b)
	got := hunkHeaders(diff)
	// The insertion merges with the 3 following context lines.
	if len(got) != 1 || got[0] != "@@ -1,3 +1,5 @@" {
		t.Fatalf("headers = %q\n%s", got, diff)
	}
	if !strings.HasPrefix(diff, "@@ -1,3 +1,5 @@\n+new1\n+new2\n line1\n") {
		t.Fatalf("unexpected diff:\n%s", diff)
	}
}

func TestUnifiedDiffZeroCountUsesPrecedingLine(t *testing.T) {
	diff := unifiedDiff("f.go", "", "a\nb\n")
	if got := hunkHeaders(diff); len(got) != 1 || got[0] != "@@ -0,0 +1,2 @@" {
		t.Fatalf("headers = %q\n%s", got, diff)
	}
}
