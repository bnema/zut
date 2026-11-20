package tui

import (
	"strings"
	"testing"
)

func TestCodeChangeStatsCountsDashAndPlusContent(t *testing.T) {
	diff := "@@ -1,2 +1,2 @@\n--- sql\n+++i\n x\n"
	if got := stripANSI(codeChangeStats(Dark, modeDiff, diff)); got != "+1 -1" {
		t.Fatalf("stats = %q", got)
	}
	legacy := "--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n-a\n+b\n"
	if got := stripANSI(codeChangeStats(Dark, modeDiff, legacy)); got != "+1 -1" {
		t.Fatalf("legacy stats = %q", got)
	}
}

func TestAddedFileKeepsDotLines(t *testing.T) {
	src := "def f():\n    ...\n"
	if got := stripANSI(codeChangeStats(Dark, modeAddedFile, src)); got != "+2" {
		t.Fatalf("stats = %q", got)
	}
	v := &View{Theme: Dark}
	rows := v.renderAddedFile(src, "a.py", 80)
	if len(rows) != 2 {
		t.Fatalf("rows = %q", rows)
	}
	if got := strings.TrimSpace(stripANSI(rows[1])); !strings.HasPrefix(got, "2 +") || !strings.Contains(got, "...") {
		t.Fatalf("row = %q", got)
	}
	if !strings.Contains(rows[1], Dark.sgrBGColor(*Dark.DiffAddBG)) {
		t.Fatalf("row untinted: %q", rows[1])
	}
}

func TestSplitDiagnosticsTrailerRequiresHeader(t *testing.T) {
	text := "# Notes\n\nDiagnostics: see below\n"
	if body, trailer := splitDiagnosticsTrailer(text); body != text || trailer != "" {
		t.Fatalf("split: %q / %q", body, trailer)
	}
	real := "x\n\nDiagnostics: 1 error (deduplicated 1)\na.go:1: bad"
	if body, trailer := splitDiagnosticsTrailer(real); body != "x" || !strings.HasPrefix(trailer, "Diagnostics: 1 error") {
		t.Fatalf("split: %q / %q", body, trailer)
	}
}

func TestTruncatedHunkHeaderDiff(t *testing.T) {
	text := "@@ -1,1 +1,1 @@\n-a\n+b\n@@ -4\n... [truncated at 10 bytes]"
	if !looksLikeUnifiedDiff(text) {
		t.Fatal("rejected truncated diff")
	}
	v := &View{Theme: Dark}
	rows := v.renderUnifiedDiff(text, 80, "a.txt")
	var plain []string
	for _, r := range rows {
		plain = append(plain, strings.TrimSpace(stripANSI(r)))
	}
	if len(plain) != 4 || plain[2] != "@@ -4" || !strings.HasPrefix(plain[3], "... [truncated") {
		t.Fatalf("rows = %q", plain)
	}
	if looksLikeUnifiedDiff("@@ -4\n-a\n+b") {
		t.Fatal("accepted malformed header followed by content")
	}
}

func TestToolHeaderLineFitsWidth(t *testing.T) {
	stats := Dark.FGColor(Dark.Tool, "+1234") + " " + Dark.FGColor(Dark.Error, "-567")
	h := toolHeaderLine(Dark, "edit some/long/path.go", stats, 20)
	if w := visibleWidth(h); w > 20 {
		t.Fatalf("width = %d: %q", w, stripANSI(h))
	}
}

func TestRenderUnifiedDiffSkipsLegacyFileHeaders(t *testing.T) {
	v := &View{Theme: Dark}
	rows := v.renderUnifiedDiff("--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n-a\n+b\n", 80, "")
	if len(rows) != 2 {
		t.Fatalf("rows = %q", rows)
	}
	if got := strings.TrimSpace(stripANSI(rows[0])); got != "1 - a" {
		t.Fatalf("first row = %q", got)
	}
}
