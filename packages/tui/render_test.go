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

// TestDrawLogInaccessibleMutationAppendedBlankAdvancesViewport is a
// regression for the streaming-render corruption where an offscreen
// (already scrolled above the viewport) chat row changes while a new
// chat row is appended. The renderer must not replay the inaccessible
// row, but it must still emit the appended row and let the trailing
// bottom-margin blank scroll the viewport so the new tail is visible.
//
// Previously the rescan triggered by the inaccessible change reset
// firstChanged/lastChanged, and the appended-blank extension was only
// re-applied when the rescan found no visible change at all. Because the
// rescan did find the visible shift, lastChanged stayed short, the
// implicit blank bottom margin was never emitted, and the viewport failed
// to advance, leaving the frame shifted by one row.
func TestDrawLogInaccessibleMutationAppendedBlankAdvancesViewport(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "")
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(50, 7)

	bottom := []string{"STATUS", "editor"}
	chat := []string{"row-00", "row-01", "row-02", "row-03", "row-04", "row-05"}
	r.DrawLog(chat, bottom, 1, 2)
	if want := 2; r.logViewportTop != want {
		t.Fatalf("setup viewport top = %d, want %d", r.logViewportTop, want)
	}

	// Repeated growth exercises the fix more than once: each round mutates
	// a row that has already scrolled above the viewport and appends one
	// new chat row, which grows the logical buffer by exactly one row.
	appended := []string{"new-00", "new-01"}
	for round := 0; round < len(appended); round++ {
		buf.Reset()
		mutated := append([]string(nil), chat...)
		mutated[round] = mutated[round] + "-mutated"
		chat = append(mutated, appended[round])
		r.DrawLog(chat, bottom, 1, 2)
		got := buf.String()

		if strings.Contains(got, "-mutated") {
			t.Fatalf("round %d replayed an inaccessible mutated row: %q", round, got)
		}
		if strings.Contains(got, SeqClearScreenNoHome) || strings.Contains(got, SeqClearScrollback) {
			t.Fatalf("round %d cleared retained scrollback: %q", round, got)
		}
		if !strings.Contains(got, appended[round]) {
			t.Fatalf("round %d did not emit appended row %q: %q", round, appended[round], got)
		}
		if want := len(chat) + len(bottom) + 1 - r.rows; r.logViewportTop != want {
			t.Fatalf("round %d viewport top = %d, want %d (blank tail did not scroll)", round, r.logViewportTop, want)
		}
		if want := len(chat) + len(bottom) - 1; r.logHardwareRow != want {
			t.Fatalf("round %d hardware row = %d, want cursor row %d", round, r.logHardwareRow, want)
		}
	}
}

// TestDrawLogBottomShrinkRepaintsViewportWithoutDuplicatingScrollback is a
// regression for a bottom-band shrink while the transcript is taller than
// the viewport. The recovery path must repaint the cleared visible screen
// with only the new visible tail; replaying the full logical buffer pushes
// transcript rows that are already retained in scrollback back into it,
// duplicating history.
func TestDrawLogBottomShrinkRepaintsViewportWithoutDuplicatingScrollback(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(50, 4)

	chat := []string{"T1", "T2", "T3", "T4", "T5"}
	r.DrawLog(chat, []string{"b1", "b2", "b3", "b4", "b5"}, -1, 0)
	buf.Reset()

	r.DrawLog(chat, []string{"d1", "d2"}, -1, 0)
	got := buf.String()

	if strings.Contains(got, SeqClearScrollback) {
		t.Fatalf("bottom shrink purged retained scrollback: %q", got)
	}
	if !strings.Contains(got, SeqClearScreenNoHome) {
		t.Fatalf("bottom shrink did not repaint the visible viewport: %q", got)
	}
	for _, dup := range []string{"T1", "T2", "T3", "T4"} {
		if strings.Contains(got, dup) {
			t.Fatalf("bottom shrink replayed retained transcript row %q: %q", dup, got)
		}
	}
	for _, want := range []string{"T5", "d1", "d2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("bottom shrink repaint missing visible row %q: %q", want, got)
		}
	}
	if want := 4; r.logViewportTop != want {
		t.Fatalf("viewport top = %d, want %d", r.logViewportTop, want)
	}
	if want := 7; r.logHardwareRow != want {
		t.Fatalf("hardware row = %d, want %d", r.logHardwareRow, want)
	}
}

// TestDrawLogInvalidationGrowthKeepsHistory guards the scrollback-safe
// recovery trim. When the previous frame fit the viewport, nothing has
// scrolled into retained scrollback yet, so a following recovery repaint
// with a taller buffer must still emit the leading rows (letting them
// scroll into history) instead of dropping them.
func TestDrawLogInvalidationGrowthKeepsHistory(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "")
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(50, 4)

	r.DrawLog([]string{"T1", "T2"}, []string{"editor"}, -1, 0)
	if want := 0; r.logViewportTop != want {
		t.Fatalf("setup viewport top = %d, want %d", r.logViewportTop, want)
	}
	buf.Reset()

	r.Invalidate()
	r.DrawLog([]string{"T1", "T2", "T3", "T4", "T5", "T6"}, []string{"editor"}, -1, 0)
	got := buf.String()
	for _, want := range []string{"T1", "T2", "T3", "T4", "T5", "T6"} {
		if !strings.Contains(got, want) {
			t.Fatalf("recovery growth dropped history row %q: %q", want, got)
		}
	}
}

// TestDrawLogBottomShrinkKeepsRetainedOffscreenImage is a regression for the
// scrollback-safe recovery trim deleting a retained offscreen image. The
// clear step deletes Kitty images, so the repaint must not skip an image
// escape in the prefix without re-emitting it; image frames fall back to the
// full replay instead.
func TestDrawLogBottomShrinkKeepsRetainedOffscreenImage(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(50, 4)

	image := "\x1b_Ga=T;payload\x1b\\"
	r.DrawLog([]string{image, "a", "b", "c"}, []string{"d", "e"}, -1, 0)
	buf.Reset()

	r.DrawLog([]string{image, "a", "b", "c"}, []string{"d"}, -1, 0)
	got := buf.String()

	if !strings.Contains(got, SeqDeleteKittyImages) {
		t.Fatalf("bottom shrink did not repaint the visible viewport: %q", got)
	}
	if !strings.Contains(got, image) {
		t.Fatalf("bottom shrink deleted a retained offscreen image without re-emitting it: %q", got)
	}
	if !strings.Contains(got, "d") {
		t.Fatalf("bottom shrink repaint missing bottom row: %q", got)
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

// TestDrawLogPartialBottomShrinkRepaintsInaccessibleRows covers returning
// from a moderately long dialog to a short one: the shorter bottom frame
// starts above the currently addressable viewport, so relative cursor
// movement cannot repaint its prefix and a full repaint is required.
// Without the forced repaint, stale dialog rows stay on screen.
func TestDrawLogPartialBottomShrinkRepaintsInaccessibleRows(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(80, 4)

	r.DrawLog(nil, []string{"transcript 1", "transcript 2", "transcript 3", "transcript 4", "transcript 5"}, -1, 0)
	buf.Reset()

	r.DrawLog(nil, []string{"dashboard", "selected agent 1"}, -1, 0)
	got := buf.String()
	if !strings.Contains(got, SeqClearScreenNoHome) {
		t.Fatalf("partial bottom shrink did not repaint the screen: %q", got)
	}
	for _, want := range []string{"dashboard", "selected agent 1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("partial bottom repaint missing %q: %q", want, got)
		}
	}
}

// TestDrawLogChatShrinkDoesNotForceBottomRepaint pins the scope of the
// forced repaint: chat reflow has its own coordinate-rebasing path which
// preserves native terminal selections. Only a shrinking bottom frame
// forces the full repaint used when returning to a short dialog.
func TestDrawLogChatShrinkDoesNotForceBottomRepaint(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(80, 4)

	r.DrawLog([]string{"chat 1", "chat 2", "chat 3", "chat 4", "chat 5", "chat 6"}, []string{"input"}, 0, 0)
	buf.Reset()

	r.DrawLog([]string{"chat 1", "chat 2", "chat 3", "chat 4", "chat 5"}, []string{"input"}, 0, 0)
	if got := buf.String(); strings.Contains(got, SeqClearScreenNoHome) {
		t.Fatalf("chat shrink unnecessarily repainted the screen: %q", got)
	}
}

// TestDrawLogFullHeightShrinkRepaintsAndTracksViewport covers a dialog
// shrink that removes a full viewport of logical rows: the incremental
// clear-below path cannot address them, so the draw must repaint fully
// and the recomputed viewport state must survive for the next draw.
// A cursor move right after must update the visible selection instead of
// being discarded as inaccessible.
func TestDrawLogFullHeightShrinkRepaintsAndTracksViewport(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "ghostty")
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(80, 4)

	r.DrawLog(nil, []string{"transcript 1", "transcript 2", "transcript 3", "transcript 4", "transcript 5", "transcript 6"}, -1, 0)
	buf.Reset()

	r.DrawLog(nil, []string{"dashboard", "selected agent 1"}, -1, 0)
	if got := buf.String(); !strings.Contains(got, SeqClearScreenNoHome) {
		t.Fatalf("full-height shrink did not repaint the screen: %q", got)
	}
	buf.Reset()

	r.DrawLog(nil, []string{"dashboard", "selected agent 2"}, -1, 0)
	if got := buf.String(); !strings.Contains(got, "selected agent 2") {
		t.Fatalf("visible update after full-height shrink was suppressed: %q", got)
	}
}

// TestDrawLogShrinkRecoveryMatrix pins the Zut-specific renderer state
// across a forced full repaint: themed backgrounds still tint the frame,
// keepScrollback terminals never purge scrollback, Kitty images are
// cleaned, and a narrow follow-up draw stays coherent with no stale rows.
func TestDrawLogShrinkRecoveryMatrix(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "vscode")
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	background := Color256(237)
	th := Dark
	th.Background = &background
	r.SetTheme(th)
	r.Resize(80, 4)
	if !r.keepScrollback {
		t.Fatal("vscode TERM_PROGRAM did not enable keepScrollback")
	}

	image := "\x1b_Ga=T;payload\x1b\\"
	r.DrawLog(nil, []string{image, "transcript 1", "transcript 2", "transcript 3", "transcript 4", "transcript 5"}, -1, 0)
	buf.Reset()

	r.DrawLog(nil, []string{"dashboard", "selected agent 1"}, -1, 0)
	got := buf.String()
	if !strings.Contains(got, SeqClearToEnd) {
		t.Fatalf("themed shrink recovery did not repaint in place: %q", got)
	}
	if strings.Contains(got, SeqClearScreenNoHome) || strings.Contains(got, SeqClearScrollback) {
		t.Fatalf("keepScrollback repaint purged scrollback: %q", got)
	}
	if !strings.Contains(got, SeqDeleteKittyImages) {
		t.Fatalf("shrink recovery did not clean Kitty images: %q", got)
	}
	if !strings.Contains(got, th.BackgroundStyle()) {
		t.Fatalf("themed repaint lost the background tint: %q", got)
	}
	buf.Reset()

	// A narrow follow-up draw must stay coherent: no stale rows, no extra
	// full repaint, and the hardware cursor tracks the new viewport.
	r.Resize(20, 4)
	r.DrawLog(nil, []string{"dashboard", "selected agent 2"}, -1, 0)
	got = buf.String()
	if !strings.Contains(got, "selected agent 2") {
		t.Fatalf("narrow follow-up draw lost the selection: %q", got)
	}
	if strings.Contains(got, "transcript") {
		t.Fatalf("narrow follow-up draw left stale rows: %q", got)
	}
	for _, row := range strings.Split(got, "\r\n") {
		if strings.ContainsAny(row, "\r\n") {
			t.Fatalf("row embeds a newline: %q", row)
		}
	}
}
