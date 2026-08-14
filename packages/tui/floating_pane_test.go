package tui

import (
	"strings"
	"testing"
)

func TestFloatingPaneComposeCentersAndDimsLiveBackground(t *testing.T) {
	var pane FloatingPane
	frame, cursorRow, cursorCol := pane.Compose(Dark, "settings", "settings", []string{"live transcript", "draft input"}, []string{"settings body"}, 100, 30, -1, 0)

	if frame.Rect.Drawer {
		t.Fatal("100-column terminal unexpectedly used drawer layout")
	}
	if want := (100 - frame.Rect.Width) / 2; frame.Rect.X != want {
		t.Fatalf("pane x = %d, want %d", frame.Rect.X, want)
	}
	if !strings.Contains(frame.Background[len(frame.Background)-2], Dim("live transcript")) {
		t.Fatalf("live background was not dimmed: %q", frame.Background)
	}
	if got := stripANSI(frame.Lines[0]); !strings.HasPrefix(got, "╭") {
		t.Fatalf("pane is missing top border: %q", got)
	} else if title := strings.Index(got, "settings"); title < 0 {
		t.Fatalf("pane title is missing from its border: %q", got)
	} else {
		left := strings.Count(got[:title], "─")
		right := strings.Count(got[title+len("settings"):], "─")
		if left-right > 1 || right-left > 1 {
			t.Fatalf("pane title is not centered in its border: %q", got)
		}
	}
	if cursorRow != -1 || cursorCol != 0 {
		t.Fatalf("cursor = (%d, %d), want hidden", cursorRow, cursorCol)
	}
}

func TestFloatingPaneUsesBottomDrawerBelowEightyColumns(t *testing.T) {
	var pane FloatingPane
	frame, _, _ := pane.Compose(Dark, "settings", "settings", nil, []string{"settings body"}, 79, 24, -1, 0)

	if !frame.Rect.Drawer {
		t.Fatal("79-column terminal did not use drawer layout")
	}
	if frame.Rect.X != 0 || frame.Rect.Width != 79 || frame.Rect.Y+frame.Rect.Height != 24 {
		t.Fatalf("drawer geometry = %+v, want full-width bottom drawer", frame.Rect)
	}
	if got := stripANSI(frame.Lines[0]); !strings.Contains(got, "settings") {
		t.Fatalf("drawer title = %q, want centered settings title", got)
	}
}

func TestFloatingPaneRecomputesGeometryOnResize(t *testing.T) {
	var pane FloatingPane
	wide, _, _ := pane.Compose(Dark, "settings", "settings", nil, []string{"setting"}, 100, 30, -1, 0)
	narrow, _, _ := pane.Compose(Dark, "settings", "settings", nil, []string{"setting"}, 79, 24, -1, 0)

	if wide.Rect.Drawer {
		t.Fatal("wide terminal unexpectedly used a drawer")
	}
	if !narrow.Rect.Drawer || narrow.Rect.Width != 79 || narrow.Rect.Y+narrow.Rect.Height != 24 {
		t.Fatalf("resize did not recompute a bottom drawer: %+v", narrow.Rect)
	}
}

func TestFloatingPaneClampsCursorToContentAndHandlesTinyTerminals(t *testing.T) {
	var pane FloatingPane
	frame, _, col := pane.Compose(Dark, "search", "search", nil, []string{"query"}, 100, 20, 0, 1000)
	if want := frame.Rect.contentX() + frame.Rect.ContentWidth() - 1; col != want {
		t.Fatalf("cursor col = %d, want content edge %d", col, want)
	}

	tiny, row, tinyCol := pane.Compose(Dark, "search", "search", nil, []string{"x"}, 2, 2, 0, 10)
	if !tiny.Rect.Borderless {
		t.Fatalf("tiny terminal rect = %+v, want borderless", tiny.Rect)
	}
	if row != 0 || tinyCol != 1 {
		t.Fatalf("tiny cursor = (%d, %d), want (0, 1)", row, tinyCol)
	}
}

func TestFloatingPaneKeepsFocusedContentVisibleAndResetsForOtherView(t *testing.T) {
	content := make([]string, 30)
	for row := range content {
		content[row] = "row " + itoa(row)
	}
	var pane FloatingPane
	frame, cursorRow, _ := pane.Compose(Dark, "settings", "settings", nil, content, 100, 12, 25, 0)
	if cursorRow < frame.Rect.Y+1 || cursorRow >= frame.Rect.Y+frame.Rect.Height-1 {
		t.Fatalf("focused cursor row %d is outside pane %+v", cursorRow, frame.Rect)
	}
	if !strings.Contains(stripANSI(strings.Join(frame.Lines, "\n")), "row 25") {
		t.Fatal("focused row was not retained in the pane viewport")
	}

	other, _, _ := pane.Compose(Dark, "subagents", "subagents", nil, content, 100, 12, -1, 0)
	if strings.Contains(stripANSI(strings.Join(other.Lines, "\n")), "row 25") {
		t.Fatal("new view inherited the previous view's scroll position")
	}
}

func TestRendererDrawFloatingPaintsBackgroundThenPaneAtAbsolutePosition(t *testing.T) {
	var out strings.Builder
	r := NewRenderer(&out)
	r.Resize(100, 20)
	var pane FloatingPane
	frame, _, _ := pane.Compose(Dark, "settings", "settings", []string{"background"}, []string{"foreground"}, 100, 20, -1, 0)
	r.DrawFloating(frame, -1, 0)
	got := out.String()
	if !strings.Contains(got, Dim("background")) || !strings.Contains(got, "foreground") {
		t.Fatalf("floating render omitted a layer: %q", got)
	}
	if !strings.Contains(got, MoveTo(frame.Rect.Y+1, frame.Rect.X+1)) {
		t.Fatalf("floating pane was not painted at its geometry: %q", got)
	}

	out.Reset()
	r.Draw(frame.Background, -1, 0)
	if !strings.Contains(out.String(), MoveTo(1, 1)) {
		t.Fatalf("normal frame did not repaint after a floating pane: %q", out.String())
	}
}

func TestRendererDrawFloatingPreservesThemeBackgroundAndImageInvalidation(t *testing.T) {
	var out strings.Builder
	r := NewRenderer(&out)
	background := Color256(237)
	th := Dark
	th.Background = &background
	r.SetTheme(th)
	r.Resize(20, 4)

	pane := FloatingPaneFrame{
		Background: make([]string, 4),
		Lines:      []string{"\x1b_Ga=T,f=100;image\x1b\\"},
		Rect:       FloatingPaneRect{X: 2, Y: 1, Width: 10, Height: 1, Borderless: true},
	}
	r.DrawFloating(pane, -1, 0)
	if !strings.Contains(out.String(), th.BackgroundStyle()) {
		t.Fatalf("floating pane omitted theme background: %q", out.String())
	}
	if !r.prevHadImage || !r.prevHadKittyImage {
		t.Fatalf("floating image state = image:%t kitty:%t, want both true", r.prevHadImage, r.prevHadKittyImage)
	}

	out.Reset()
	r.Draw([]string{""}, -1, 0)
	if !strings.Contains(out.String(), SeqDeleteKittyImages) {
		t.Fatalf("stale kitty image was not deleted: %q", out.String())
	}
}
