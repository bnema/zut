package tui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

func TestGhostRendersAfterPromptOnEmptyBuffer(t *testing.T) {
	e := NewEditor("> ")
	_, _, baseCol := e.Render(40)
	e.SetGhost("run the tests")
	lines, row, col := e.Render(40)
	if len(lines) != 1 || lines[0] != "> run the tests" {
		t.Fatalf("lines = %q", lines)
	}
	if row != 0 || col != baseCol {
		t.Fatalf("cursor = %d,%d want 0,%d", row, col, baseCol)
	}
}

func TestGhostStyleAndStyledPrompt(t *testing.T) {
	e := NewEditor("\x1b[31m>\x1b[0m ")
	e.GhostStyle = func(s string) string { return "\x1b[2m" + s + "\x1b[0m" }
	e.SetGhost("hello")
	lines, _, col := e.Render(40)
	if !strings.HasSuffix(lines[0], "\x1b[2mhello\x1b[0m") || col != 2 {
		t.Fatalf("lines = %q col=%d", lines, col)
	}
}

func TestGhostTruncatesWideRunes(t *testing.T) {
	for w := 1; w <= 12; w++ {
		e := NewEditor("> ")
		e.SetGhost("日本語のテキストです")
		lines, _, _ := e.Render(w)
		empty := NewEditor("> ")
		base, _, _ := empty.Render(w)
		if len(lines) != len(base) {
			t.Fatalf("width %d: rows %d want %d", w, len(lines), len(base))
		}
		for i, l := range lines {
			vw := runewidth.StringWidth(stripANSI(l))
			bw := runewidth.StringWidth(stripANSI(base[i]))
			if vw > w && vw != bw {
				t.Fatalf("width %d: row %q is %d cells", w, l, vw)
			}
		}
		if w >= 6 && len(base) == 1 {
			if vw := runewidth.StringWidth(lines[0]); vw > w-1 {
				t.Fatalf("width %d: no cursor column left: %q", w, lines[0])
			}
			if !strings.HasSuffix(lines[0], "…") {
				t.Fatalf("width %d: missing ellipsis: %q", w, lines[0])
			}
		}
	}
}

func TestGhostSanitized(t *testing.T) {
	e := NewEditor("> ")
	e.SetGhost("  \x1b[31mfix\x1b[0m\n\tthe\x07  bug\r\n ")
	if g := e.Ghost(); g != "fix the bug" {
		t.Fatalf("ghost = %q", g)
	}
	e.SetGhost("")
	if e.Ghost() != "" {
		t.Fatal("empty text should clear")
	}
}

func TestGhostHiddenWhenTypedOrMasked(t *testing.T) {
	e := NewEditor("> ")
	e.SetGhost("suggest")
	e.Mask = true
	if lines, _, _ := e.Render(40); strings.Contains(lines[0], "suggest") {
		t.Fatalf("masked render shows ghost: %q", lines)
	}
	e.Mask = false
	e.Lines = []string{"x"}
	e.CursorC = 1
	if e.Ghost() != "" {
		t.Fatal("Ghost should be empty with non-empty buffer")
	}
	if lines, _, _ := e.Render(40); strings.Contains(lines[0], "suggest") {
		t.Fatalf("render shows ghost: %q", lines)
	}
}

func TestGhostNeverInValue(t *testing.T) {
	e := NewEditor("> ")
	e.SetGhost("suggest")
	if e.Value() != "" || e.SubmitValue() != "" {
		t.Fatal("ghost leaked into value")
	}
	c := e.Clone()
	if c.Ghost() != "suggest" || c.Value() != "" {
		t.Fatalf("clone ghost=%q value=%q", c.Ghost(), c.Value())
	}
}

func TestAcceptGhost(t *testing.T) {
	e := NewEditor("> ")
	if e.AcceptGhost() {
		t.Fatal("accept without ghost")
	}
	e.SetGhost("do it")
	if !e.AcceptGhost() {
		t.Fatal("accept failed")
	}
	if e.Value() != "do it" || e.CursorC != 5 || e.Ghost() != "" || e.ghost != "" {
		t.Fatalf("value=%q cursor=%d ghost=%q", e.Value(), e.CursorC, e.ghost)
	}
	e.SetGhost("other")
	if e.AcceptGhost() {
		t.Fatal("accept with non-empty buffer")
	}
}

func TestGhostClearedByKeysAndEdits(t *testing.T) {
	e := NewEditor("> ")
	e.SetGhost("s")
	e.HandleKey(Key{Kind: KeyRune, Rune: 'a'})
	if e.ghost != "" || e.Value() != "a" {
		t.Fatalf("ghost=%q value=%q", e.ghost, e.Value())
	}
	e.Clear()
	e.SetGhost("s")
	// Enter reports submit; callers ignore it for an empty value.
	e.HandleKey(Key{Kind: KeyEnter})
	if e.ghost != "" || e.SubmitValue() != "" {
		t.Fatalf("ghost=%q submit=%q", e.ghost, e.SubmitValue())
	}
	for _, f := range []func(){func() { e.Insert("x") }, func() { e.SetValue("y") }, e.Clear} {
		e.SetValue("")
		e.SetGhost("s")
		f()
		if e.ghost != "" {
			t.Fatal("edit did not clear ghost")
		}
	}
}

func TestGhostStripsOSCAndBidiControls(t *testing.T) {
	cases := map[string]string{
		"osc8 bel":         "\x1b]8;;https://x.test\x07link\x1b]8;;\x07 text",
		"osc8 st":          "\x1b]8;;https://x.test\x1b\\link\x1b]8;;\x1b\\ text",
		"c1 osc":           "\u009d8;;https://x.test\u009clink\u009d8;;\u0007 text",
		"bidi override":    "\u202elink\u202c text",
		"bidi isolate":     "\u2066link\u2069 text",
		"unterminated osc": "link text\x1b]8;;https://x.test",
	}
	for name, in := range cases {
		e := NewEditor("> ")
		e.SetGhost(in)
		if got := e.Ghost(); got != "link text" {
			t.Errorf("%s: ghost = %q, want %q", name, got, "link text")
		}
	}
	e := NewEditor("> ")
	e.SetGhost("👩\u200d💻 code")
	if got := e.Ghost(); got != "👩\u200d💻 code" {
		t.Errorf("ZWJ sequence altered: %q", got)
	}
}
