package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// liveBody renders a live call and returns its body rows (header dropped).
func liveBody(t *testing.T, v *View, tc ToolCallView) []string {
	t.Helper()
	rows := v.renderToolCall(tc, 80)
	if len(rows) < 2 {
		t.Fatalf("no live body: %q", rows)
	}
	return rows[1:]
}

func liveWriteCall(content string) ToolCallView {
	raw, _ := json.Marshal(map[string]string{"path": "a.go", "content": content})
	// Drop the closing `"}` so the content is still streaming.
	buf := strings.TrimSuffix(string(raw), `"}`)
	return ToolCallView{ID: "w", Name: "write", RawJSONBuf: buf, LivePath: "a.go"}
}

func TestLiveWriteTailWindow(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&b, "x%d := %d\n", i, i)
	}
	b.WriteString("y := 3")
	v := &View{Theme: Dark}
	body := liveBody(t, v, liveWriteCall(b.String()))
	if len(body) != liveCodeRows+1 {
		t.Fatalf("rows = %d, want %d", len(body), liveCodeRows+1)
	}
	if got := strings.TrimSpace(stripANSI(body[0])); got != "writing · 31 lines" {
		t.Fatalf("progress = %q", got)
	}
	if got := strings.TrimSpace(stripANSI(body[1])); !strings.HasPrefix(got, "20 + x20 := 20") {
		t.Fatalf("first code row = %q", got)
	}
	last := body[len(body)-1]
	if got := strings.TrimSpace(stripANSI(last)); !strings.HasPrefix(got, "31 + y := 3") {
		t.Fatalf("last row = %q", got)
	}
	if !strings.Contains(last, Dark.sgrBGColor(*Dark.DiffAddBG)) {
		t.Fatalf("partial row untinted: %q", last)
	}
}

func TestLiveWriteShortStreamIsFullHeight(t *testing.T) {
	v := &View{Theme: Dark}
	body := liveBody(t, v, liveWriteCall("a\nb"))
	if len(body) != liveCodeRows+1 {
		t.Fatalf("rows = %d, want %d", len(body), liveCodeRows+1)
	}
	v.ExpandAll = true
	v.liveRenderCache = nil
	if got := liveBody(t, v, liveWriteCall("a\nb")); len(got) != liveCodeRows+1 {
		t.Fatalf("ExpandAll rows = %d", len(got))
	}
}

func TestLiveEditShowsOldThenNew(t *testing.T) {
	v := &View{Theme: Dark}
	tc := ToolCallView{ID: "e", Name: "edit", LivePath: "a.txt",
		RawJSONBuf: `{"path":"a.txt","edits":[{"oldText":"a\nb","newText":"c`}
	body := liveBody(t, v, tc)
	if len(body) != liveCodeRows+1 {
		t.Fatalf("rows = %d", len(body))
	}
	if got := strings.TrimSpace(stripANSI(body[0])); got != "edit 1 · -2 +1" {
		t.Fatalf("progress = %q", got)
	}
	want := []string{"- a", "- b", "+ c"}
	for i, w := range want {
		if got := strings.TrimSpace(stripANSI(body[1+i])); got != w {
			t.Fatalf("row %d = %q, want %q", i+1, got, w)
		}
	}
	if !strings.Contains(body[1], Dark.sgrBGColor(*Dark.DiffDelBG)) || !strings.Contains(body[3], Dark.sgrBGColor(*Dark.DiffAddBG)) {
		t.Fatalf("edit rows not tinted: %q", body[1:4])
	}
}

func TestLiveCodeCacheSurvivesPruning(t *testing.T) {
	v := &View{Theme: Dark}
	tc := ToolCallView{ID: "e", Name: "edit", LivePath: "a.go",
		RawJSONBuf: `{"path":"a.go","edits":[{"oldText":"a\nb\n","newText":"c`}
	v.ToolCalls = []ToolCallView{tc}
	v.BuildLive(80)
	v.prepareLiveToolState()
	if len(v.liveCode) != 2 {
		t.Fatalf("live code states pruned while call active: %d", len(v.liveCode))
	}
}
