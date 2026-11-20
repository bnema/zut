package tui

import (
	"strings"
	"testing"
)

func highlightCacheLen() int {
	highlightCache.mu.Lock()
	defer highlightCache.mu.Unlock()
	return len(highlightCache.order)
}

func TestLiveHighlightedRecomputesOnlyOnNewLines(t *testing.T) {
	src := "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\nvar x = 1"
	v := &View{Theme: Dark}
	before := highlightCacheLen()
	var done []string
	var partial string
	for i := 1; i <= len(src); i++ {
		done, partial = v.liveHighlighted("w", "content", "go", src[:i])
		if strings.Contains(partial, "\x1b[") {
			t.Fatalf("partial line carries SGR: %q", partial)
		}
	}
	st := v.liveCode["w"]
	if want := strings.Count(src, "\n"); st.recomputes != want || len(done) != want {
		t.Fatalf("recomputes=%d lines=%d, want %d", st.recomputes, len(done), want)
	}
	if partial != "var x = 1" {
		t.Fatalf("partial = %q", partial)
	}
	if after := highlightCacheLen(); after != before {
		t.Fatalf("highlight cache grew from %d to %d", before, after)
	}
}

func TestLiveHighlightedResetsOnKeyChange(t *testing.T) {
	v := &View{Theme: Dark}
	v.liveHighlighted("e", "oldText#1", "go", "a\nb\n")
	done, _ := v.liveHighlighted("e", "oldText#2", "go", "c\n")
	if len(done) != 1 || v.liveCode["e"].recomputes != 1 {
		t.Fatalf("state not reset: lines=%d recomputes=%d", len(done), v.liveCode["e"].recomputes)
	}
}

func TestLiveHighlightedWindowsLongBodies(t *testing.T) {
	v := &View{Theme: Dark}
	text := strings.Repeat("x := 1\n", liveCodeHighlightWindow+50)
	done, _ := v.liveHighlighted("w", "content", "go", text)
	if len(done) != liveCodeHighlightWindow+50 {
		t.Fatalf("lines = %d", len(done))
	}
	if done[0] != Dark.FGColor(Dark.Tool, "x := 1") {
		t.Fatalf("pre-window row not plain: %q", done[0])
	}
}

func TestLiveCodePrunedWithInactiveCalls(t *testing.T) {
	v := &View{Theme: Dark}
	v.liveHighlighted("gone", "content", "go", "a\n")
	v.prepareLiveToolState()
	if _, ok := v.liveCode["gone"]; ok {
		t.Fatal("liveCode kept an inactive call")
	}
	v.liveHighlighted("x", "content", "go", "a\n")
	v.InvalidateRenderCache()
	if v.liveCode != nil {
		t.Fatal("InvalidateRenderCache kept liveCode")
	}
}
