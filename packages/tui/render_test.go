package tui

import (
	"bytes"
	"strings"
	"testing"
)

// TestDrawLogIdleNoOpEmitsNothing pins the cursor-blink fix: when
// DrawLog is called with the exact same buffer and cursor position
// as the previous call, it must emit ZERO bytes.
//
// The bug this regresses: at the 120ms animation tick the renderer
// used to always emit SeqHideCursor + cursor-position +
// SeqShowCursor, which resets the terminal's blink timer. Faster
// than the OS blink interval, so an idle dialog editor (e.g. a
// re-opened subagent transcript whose agent isn't producing output)
// rendered the caret as a solid non-blinking block.
//
// With the no-op fast path the renderer leaves the screen alone
// on idle frames, letting the terminal run its own blink cycle.
func TestDrawLogIdleNoOpEmitsNothing(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(80, 24)

	chat := []string{"hello", "world"}
	bottom := []string{"▌ "}
	// First draw populates the renderer's cached buffer.
	r.DrawLog(chat, bottom, 0, 2)
	first := buf.Len()
	if first == 0 {
		t.Fatal("first DrawLog wrote nothing; setup is broken")
	}
	buf.Reset()

	// Identical second draw: same content, same cursor placement.
	r.DrawLog(chat, bottom, 0, 2)
	if buf.Len() != 0 {
		t.Fatalf("idle re-draw emitted %d bytes; expected 0 so terminal blink keeps ticking\n%q",
			buf.Len(), buf.String())
	}
}

// TestDrawLogContentChangeBreaksFastPath proves the no-op fast path
// only fires when nothing changed. A buffer mutation must still
// produce output, otherwise streaming agent replies would freeze on
// screen.
func TestDrawLogContentChangeBreaksFastPath(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(80, 24)

	r.DrawLog([]string{"hello"}, []string{"▌ "}, 0, 2)
	buf.Reset()

	// New chat row lands.
	r.DrawLog([]string{"hello", "world"}, []string{"▌ "}, 0, 2)
	if buf.Len() == 0 {
		t.Fatal("content change suppressed by fast path; streaming output would freeze")
	}
}

// TestDrawLogCursorMoveBreaksFastPath proves a cursor-only change
// (no buffer change) still produces output. Without this, typing in
// the editor would visually move the caret but the terminal would
// keep drawing it at the old column.
func TestDrawLogCursorMoveBreaksFastPath(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(80, 24)

	r.DrawLog([]string{"hi"}, []string{"▌ "}, 0, 2)
	buf.Reset()

	// Same buffer, different cursor column.
	r.DrawLog([]string{"hi"}, []string{"▌ "}, 0, 3)
	if buf.Len() == 0 {
		t.Fatal("cursor-only change suppressed by fast path; caret would lag behind typing")
	}
	// And the emitted bytes must at least reposition the cursor.
	if !strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("cursor move emission missing CSI escapes: %q", buf.String())
	}
}

// TestDrawLogResizeForcesFullRedraw confirms a resize invalidates
// the cache so the next DrawLog with identical inputs still emits.
// Resize sets logInit=false; without that, a resize followed by an
// identical buffer would falsely no-op and leave a stale frame.
func TestDrawLogResizeForcesFullRedraw(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(80, 24)
	r.DrawLog([]string{"hi"}, []string{"▌ "}, 0, 2)
	buf.Reset()

	r.Resize(100, 30)
	r.DrawLog([]string{"hi"}, []string{"▌ "}, 0, 2)
	if buf.Len() == 0 {
		t.Fatal("post-resize redraw skipped; the new frame would never reach the terminal")
	}
}

// TestDrawLogInaccessibleChangePreservesScrollbackSelection covers long
// streaming output whose changing first row has already scrolled above the
// viewport. That row is immutable terminal history: clearing or replaying it
// either destroys a native mouse selection or duplicates stale tool frames.
func TestDrawLogInaccessibleChangePreservesScrollbackSelection(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "")
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(80, 3)
	r.DrawLog([]string{"selected partial", "line 2", "line 3", "line 4"}, []string{"input"}, 0, 0)
	buf.Reset()

	// Only the historical row changed. DrawLog must leave the terminal alone
	// instead of clearing and replaying the retained scrollback.
	r.DrawLog([]string{"selected partial response", "line 2", "line 3", "line 4"}, []string{"input"}, 0, 0)
	got := buf.String()
	if strings.Contains(got, SeqClearScreenNoHome) || strings.Contains(got, SeqClearScrollback) {
		t.Fatalf("inaccessible change cleared selected scrollback: %q", got)
	}
	if strings.Contains(got, "selected partial response") {
		t.Fatalf("inaccessible row was replayed into retained scrollback: %q", got)
	}
}

func TestDrawLogStructuralGrowthScrollsBeforeRepaintingVisibleTail(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(80, 3)
	r.DrawLog([]string{"old 0", "old 1"}, []string{"working 0s"}, -1, 0)
	buf.Reset()

	// Chat and the bottom band both grow while an old row is above the
	// viewport. Scroll by the logical growth before repainting the visible
	// tail. Homing and clearing the viewport here makes terminals visibly
	// jump during every streaming markdown reflow.
	chat := []string{"new row", "old 0", "old 1"}
	r.DrawLog(chat, []string{"working 0s", "editor"}, 1, 3)
	got := buf.String()
	if strings.Contains(got, SeqCursorHome+SeqClearToEnd) {
		t.Fatalf("structural growth reset the visible viewport: %q", got)
	}
	if strings.Contains(got, SeqClearScreenNoHome) || strings.Contains(got, SeqClearScrollback) {
		t.Fatalf("structural growth cleared retained scrollback: %q", got)
	}
	if !strings.Contains(got, "\r\n\r\n") {
		t.Fatalf("structural growth did not advance the viewport by two consecutive rows: %q", got)
	}
	if strings.Contains(got, "old 1") {
		t.Fatalf("viewport repaint included a row above the visible tail: %q", got)
	}
	for _, visible := range []string{"working 0s", "editor", SeqShowCursor, "\x1b[3C"} {
		if !strings.Contains(got, visible) {
			t.Fatalf("viewport repaint missing %q: %q", visible, got)
		}
	}
	if want := len(r.logLines) - r.rows; r.logViewportTop != want {
		t.Fatalf("viewport top = %d, want %d", r.logViewportTop, want)
	}
	if want := len(chat) + 1; r.logHardwareRow != want {
		t.Fatalf("hardware row = %d, want cursor row %d", r.logHardwareRow, want)
	}

	buf.Reset()
	r.DrawLog(chat, []string{"working 1s", "editor"}, 1, 3)
	if got := buf.String(); !strings.Contains(got, "working 1s") {
		t.Fatalf("status update after viewport repaint was lost: %q", got)
	}

	buf.Reset()
	r.DrawLog(append(chat, "next tool"), []string{"working 2s", "editor"}, 1, 3)
	if got := buf.String(); !strings.Contains(got, "next tool") {
		t.Fatalf("append after viewport repaint was lost: %q", got)
	}
}

func TestDrawLogStructuralShrinkRepaintsVisibleTailInPlace(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(80, 4)
	r.DrawLog([]string{"partial", "old 0", "old 1", "old 2"}, []string{"working", "editor"}, 1, 2)
	buf.Reset()

	chat := []string{"old 0", "old 1", "old 2"}
	r.DrawLog(chat, []string{"working", "editor"}, 1, 2)
	got := buf.String()
	if strings.Contains(got, SeqCursorHome+SeqClearToEnd) {
		t.Fatalf("structural shrink reset the visible viewport: %q", got)
	}
	for _, visible := range []string{"old 2", "working", "editor"} {
		if !strings.Contains(got, visible) {
			t.Fatalf("viewport repaint missing %q after shrink: %q", visible, got)
		}
	}
	if !strings.Contains(got, "\r\x1b[0m"+SeqClearLine+"old 2") {
		t.Fatalf("structural shrink did not return to column zero before repaint: %q", got)
	}
	if want := len(r.logLines) - r.rows; r.logViewportTop != want {
		t.Fatalf("viewport top = %d, want %d", r.logViewportTop, want)
	}
}

func TestDrawLogStructuralGrowthIgnoresImagesOutsideVisibleTail(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(80, 3)
	image := "\x1b_Ga=T;payload\x1b\\"
	r.DrawLog([]string{image, "line 1", "line 2", "line 3"}, []string{"input"}, -1, 0)
	buf.Reset()

	r.DrawLog([]string{"new row", image, "line 1", "line 2", "line 3"}, []string{"input"}, -1, 0)
	got := buf.String()
	if strings.Contains(got, SeqCursorHome+SeqClearToEnd) {
		t.Fatalf("image in retained scrollback forced a viewport reset: %q", got)
	}
	if !strings.Contains(got, "line 3") || !strings.Contains(got, "input") {
		t.Fatalf("visible tail was not repainted after growth: %q", got)
	}
}

func TestDrawLogStructuralReflowRepaintsVisibleImageFootprint(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(80, 4)
	image := "\x1b_Ga=T;payload\x1b\\"
	boxedBlank := "  │      │  "
	r.DrawLog([]string{image, boxedBlank, "caption"}, []string{"working 0s"}, -1, 0)
	buf.Reset()

	// The visible tail begins deep inside an image footprint spanning more
	// than one screen. Real footprint rows may retain their sentinel or be
	// wrapped in empty tool-box edges. A viewport reset deletes Kitty images,
	// so it must scan across both forms and replay the escape row above them.
	r.DrawLog([]string{
		"new row", image, boxedBlank, imageFootprintSentinel, boxedBlank,
		boxedBlank, boxedBlank, "caption",
	}, []string{"working 1s"}, -1, 0)
	got := buf.String()
	if !strings.Contains(got, SeqDeleteKittyImages) {
		t.Fatalf("viewport reset did not clear stale Kitty images: %q", got)
	}
	if !strings.Contains(got, image) {
		t.Fatalf("viewport reset did not repaint the visible image: %q", got)
	}
}

func TestDrawLogInaccessibleChangeStillAppendsNewOutput(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "")
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(80, 3)
	r.DrawLog([]string{"selected partial", "line 2", "line 3", "line 4"}, []string{"input"}, 0, 0)
	buf.Reset()

	// A streaming reflow changes inaccessible history while a tool result is
	// appended. Only the new suffix should be emitted, naturally scrolling the
	// selected old text upward without replaying the complete frame.
	r.DrawLog([]string{"selected partial response", "line 2", "line 3", "line 4", "tool output"}, []string{"input"}, 0, 0)
	got := buf.String()
	if strings.Contains(got, SeqClearScreenNoHome) || strings.Contains(got, SeqClearScrollback) {
		t.Fatalf("append after inaccessible change cleared selected scrollback: %q", got)
	}
	if strings.Contains(got, "selected partial response") || strings.Contains(got, "line 2") {
		t.Fatalf("append replayed historical rows: %q", got)
	}
	if !strings.Contains(got, "tool output") {
		t.Fatalf("new tool output was not appended: %q", got)
	}
}

// TestDrawLogInvalidationPreservesScrollbackSelection pins the same rule for
// cache invalidations, which can happen during an active turn independently
// of an inaccessible changed row.
func TestDrawLogInvalidationPreservesScrollbackSelection(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "")
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(80, 3)
	r.DrawLog([]string{"one", "two", "three", "four"}, []string{"input"}, 0, 0)
	buf.Reset()

	r.Invalidate()
	r.DrawLog([]string{"one", "two", "three", "four"}, []string{"input"}, 0, 0)
	if got := buf.String(); strings.Contains(got, SeqClearScrollback) {
		t.Fatalf("invalidation repaint erased scrollback and native selection: %q", got)
	}
}
