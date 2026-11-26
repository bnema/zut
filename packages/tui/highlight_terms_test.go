package tui

import (
	"strings"
	"testing"
)

func highlightTestTheme() Theme {
	return Theme{
		FG:           Color256(252),
		SelectionFG:  Color256(16),
		SelectionBG:  Color256(117),
		UserBubbleBG: Color256(236),
		SearchLineBG: Color256(58),
	}
}

func TestHighlightTermsPlain(t *testing.T) {
	th := highlightTestTheme()
	hl := th.SelectionStyle()
	got, ok := HighlightTerms("start Refactoring now", []string{"refacto"}, th)
	if !ok {
		t.Fatal("expected match")
	}
	want := "start " + hl + "Refacto" + reset + "ring now"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestHighlightTermsPrefixOfWordOnly(t *testing.T) {
	th := highlightTestTheme()
	in := "unrefactored"
	got, ok := HighlightTerms(in, []string{"refacto"}, th)
	if ok || got != in {
		t.Fatalf("mid-word match should not highlight: %q %v", got, ok)
	}
}

func TestHighlightTermsNoMatch(t *testing.T) {
	th := highlightTestTheme()
	in := "\x1b[1mhello\x1b[22m world"
	got, ok := HighlightTerms(in, []string{"zzz", ""}, th)
	if ok || got != in {
		t.Fatalf("got %q %v", got, ok)
	}
}

func TestHighlightTermsUnicodeFolding(t *testing.T) {
	th := highlightTestTheme()
	got, ok := HighlightTerms("ÉCOLE", []string{"éco"}, th)
	if !ok || !strings.Contains(got, th.SelectionStyle()+"ÉCO"+reset) {
		t.Fatalf("got %q %v", got, ok)
	}
}

func TestHighlightTermsRestoresNestedStyle(t *testing.T) {
	th := highlightTestTheme()
	hl := th.SelectionStyle()
	bold := "\x1b[1m"
	red := "\x1b[38;5;196m"
	in := bold + "a " + red + "match here" + reset + " tail"
	got, ok := HighlightTerms(in, []string{"match"}, th)
	if !ok {
		t.Fatal("expected match")
	}
	want := bold + "a " + red + hl + "match" + reset + bold + red + " here" + reset + " tail"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if visibleWidth(got) != visibleWidth(in) {
		t.Fatal("width changed")
	}
}

func TestHighlightTermsAcrossStyleBoundary(t *testing.T) {
	th := highlightTestTheme()
	hl := th.SelectionStyle()
	green := "\x1b[32m"
	in := "ref" + green + "actor" + reset
	got, ok := HighlightTerms(in, []string{"refact"}, th)
	if !ok {
		t.Fatal("expected match across style boundary")
	}
	// The inner SGR is kept and the highlight re-applied right after it.
	want := hl + "ref" + green + hl + "act" + reset + green + "or" + reset
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestHighlightTermsWideRunesAndMultipleTerms(t *testing.T) {
	th := highlightTestTheme()
	in := "日本語 テスト 🚀 launch go"
	got, ok := HighlightTerms(in, []string{"日本", "🚀", "LAUN", "go"}, th)
	if !ok {
		t.Fatal("expected match")
	}
	if n := strings.Count(got, th.SelectionStyle()); n != 4 {
		t.Fatalf("highlights = %d, want 4: %q", n, got)
	}
	if visibleWidth(got) != visibleWidth(in) || stripANSI(got) != in {
		t.Fatalf("width or text changed: %q", got)
	}
}

func TestHighlightTermsLongestWins(t *testing.T) {
	th := highlightTestTheme()
	got, _ := HighlightTerms("session", []string{"se", "sess"}, th)
	want := th.SelectionStyle() + "sess" + reset + "ion"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestHighlightLinePreservesInnerStyles(t *testing.T) {
	th := highlightTestTheme()
	bg := th.bgPrefix(th.SearchLineBG)
	in := "\x1b[1mbold\x1b[0m plain \x1b[48;5;0mown\x1b[49m 日本"
	got := HighlightLine(in, th)
	want := bg + "\x1b[1mbold\x1b[0m" + bg + " plain \x1b[48;5;0mown\x1b[49m" + bg + " 日本" + reset
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if visibleWidth(got) != visibleWidth(in) {
		t.Fatal("width changed")
	}
}

func TestHighlightLineUsesSearchLineBG(t *testing.T) {
	th := highlightTestTheme()
	got := HighlightLine("x", th)
	if !strings.HasPrefix(got, sgrBGColor(Color256(58))) {
		t.Fatalf("expected SearchLineBG prefix: %q", got)
	}
	if strings.Contains(got, sgrBGColor(th.UserBubbleBG)) {
		t.Fatalf("must not use UserBubbleBG: %q", got)
	}
	for _, base := range []Theme{Dark, Light} {
		if base.SearchLineBG == base.UserBubbleBG || base.SearchLineBG == (TerminalColor{}) {
			t.Fatalf("built-in theme needs a distinct SearchLineBG: %+v", base.SearchLineBG)
		}
	}
}

func TestHighlightTermsKeepsStringEscapesWhole(t *testing.T) {
	th := Dark
	for _, seq := range []string{
		"\x1b_Ga=T,f=100;cmVmYWN0bw==refacto\x1b\\", // kitty APC with the term in the payload
		"\x1bPq#0refacto\x1b\\",                     // sixel DCS
		"\x1b^refacto\x1b\\",                        // PM
		"\x1bXrefacto\x07",                          // SOS ended by BEL
	} {
		toks := tokenizeANSI("a" + seq + "b")
		if len(toks) != 3 || !toks[1].isFn || toks[1].esc != seq {
			t.Fatalf("tokenizeANSI(%q) = %#v, want the sequence as one escape", seq, toks)
		}
	}
	image := "refacto \x1b_Ga=T;refacto\x1b\\"
	if got, ok := HighlightTerms(image, []string{"refacto"}, th); ok || got != image {
		t.Fatalf("image row was modified: %q", got)
	}
	pm := "refacto \x1b^refacto\x1b\\ end"
	got, ok := HighlightTerms(pm, []string{"refacto"}, th)
	if !ok || !strings.Contains(got, "\x1b^refacto\x1b\\") || strings.Count(got, th.SelectionStyle()) != 1 {
		t.Fatalf("PM payload was highlighted: %q", got)
	}
}
