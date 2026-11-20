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
		done, _, partial = v.liveHighlighted("w", "content", "go", src[:i])
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
	done, _, _ := v.liveHighlighted("e", "oldText#2", "go", "c\n")
	if len(done) != 1 || v.liveCode["e"].recomputes != 1 {
		t.Fatalf("state not reset: lines=%d recomputes=%d", len(done), v.liveCode["e"].recomputes)
	}
}

func TestLiveHighlightedWindowsLongBodies(t *testing.T) {
	v := &View{Theme: Dark}
	text := strings.Repeat("x := 1\n", liveCodeHighlightWindow+50)
	tail, complete, _ := v.liveHighlighted("w", "content", "go", text)
	if complete != liveCodeHighlightWindow+50 || len(tail) != liveCodeHighlightWindow {
		t.Fatalf("complete = %d tail = %d", complete, len(tail))
	}
	if !strings.Contains(tail[0], "\x1b[") {
		t.Fatalf("tail row not highlighted: %q", tail[0])
	}
}

func TestLiveHighlightedKeepsHighlightWithTrailingBlankLines(t *testing.T) {
	v := &View{Theme: Dark}
	src := "package main\n\n"
	for i := 1; i <= len(src); i++ {
		tail, _, _ := v.liveHighlighted("w", "content", "go", src[:i])
		if len(tail) > 0 && !strings.Contains(tail[0], "\x1b[") {
			t.Fatalf("prefix %q: first row lost highlight: %q", src[:i], tail[0])
		}
	}
}

func TestLiveHighlightedRecomputesOnSameLengthChange(t *testing.T) {
	v := &View{Theme: Dark}
	v.liveHighlighted("w", "content", "", "aaa\nb")
	tail, _, _ := v.liveHighlighted("w", "content", "", "zzz\nb")
	if v.liveCode["w"].recomputes != 2 || stripANSI(tail[0]) != "zzz" {
		t.Fatalf("stale cache: recomputes=%d tail=%q", v.liveCode["w"].recomputes, tail)
	}
}

func TestLiveHighlightedChangedLengthRecomputes(t *testing.T) {
	v := &View{Theme: Dark}
	v.liveHighlighted("w", "content", "", "aaa\nb")
	tail, _, _ := v.liveHighlighted("w", "content", "", "aaaa\nb")
	if stripANSI(tail[0]) != "aaaa" {
		t.Fatalf("stale cache: %q", tail)
	}
}

func TestAdoptRenderCacheKeepsLiveCode(t *testing.T) {
	v := &View{Theme: Dark}
	v.liveHighlighted("w", "content", "go", "a\n")
	snap := &View{Theme: Dark}
	v.AdoptRenderCacheFrom(snap)
	if v.liveCode["w"] == nil {
		t.Fatal("adopt dropped live code state")
	}
}

func TestRenderToolCallPrunesFinishedLiveCode(t *testing.T) {
	v := &View{Theme: Dark}
	v.liveHighlighted("w", "content", "go", "a\n")
	v.liveHighlighted("w"+liveCodeIDSep+"oldText", "oldText#1", "go", "a\n")
	v.liveHighlighted("other", "content", "go", "a\n")
	v.RenderToolCall(ToolCallView{ID: "w", Name: "write", Done: true, Result: "ok"}, 80)
	if len(v.liveCode) != 1 || v.liveCode["other"] == nil {
		t.Fatalf("liveCode = %v", v.liveCode)
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
