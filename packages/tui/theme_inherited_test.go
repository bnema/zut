package tui

import (
	"bytes"
	"strings"
	"testing"
)

func TestDetectTrueColor(t *testing.T) {
	tests := []struct {
		name      string
		term      string
		colorTerm string
		want      bool
	}{
		{name: "COLORTERM truecolor", term: "screen", colorTerm: "truecolor", want: true},
		{name: "COLORTERM 24bit", term: "screen", colorTerm: "24bit", want: true},
		{name: "direct TERM", term: "xterm-direct", want: true},
		{name: "256 color TERM", term: "xterm-256color", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectTrueColor(tt.term, tt.colorTerm); got != tt.want {
				t.Fatalf("DetectTrueColor(%q, %q) = %v, want %v", tt.term, tt.colorTerm, got, tt.want)
			}
		})
	}
}

func TestDetectColorDepth(t *testing.T) {
	for _, tt := range []struct {
		term, colorTerm string
		want            ColorDepth
	}{
		{"xterm", "", ColorDepthANSI16},
		{"xterm-256color", "", ColorDepthIndexed256},
		{"screen", "truecolor", ColorDepthTrueColor},
	} {
		if got := DetectColorDepth(tt.term, tt.colorTerm); got != tt.want {
			t.Fatalf("DetectColorDepth(%q, %q) = %v, want %v", tt.term, tt.colorTerm, got, tt.want)
		}
	}
}

func terminalTestProfile(depth ColorDepth) TerminalProfile {
	profile := TerminalProfile{
		Foreground:    ColorRGB(220, 210, 200),
		Background:    ColorRGB(12, 18, 24),
		HasForeground: true,
		HasBackground: true,
		Depth:         depth,
	}
	profile.Palette[10] = ColorRGB(20, 220, 80)
	profile.Palette[12] = ColorRGB(40, 100, 240)
	profile.Palette[13] = ColorRGB(220, 70, 210)
	profile.PaletteKnown = 1<<10 | 1<<12 | 1<<13
	return profile
}

func TestTerminalThemeUsesTerminalDefaultsAndPaletteSlots(t *testing.T) {
	th := TerminalTheme(terminalTestProfile(ColorDepthTrueColor))
	if th.Background != nil {
		t.Fatal("terminal theme painted a full-row background")
	}
	if th.FG != TerminalDefault() || th.Accent != TerminalPaletteSlot(12) {
		t.Fatalf("terminal roles = fg %#v accent %#v", th.FG, th.Accent)
	}
	if got := th.FGColor(th.FG, "x"); got != "\x1b[39mx\x1b[0m" {
		t.Fatalf("default foreground = %q", got)
	}
	if got := th.FGColor(th.Accent, "x"); !strings.Contains(got, "\x1b[38;2;40;100;240m") {
		t.Fatalf("accent did not use reported palette: %q", got)
	}
}

func TestTerminalThemeUnknownProfileUsesANSI(t *testing.T) {
	th := TerminalTheme(TerminalProfile{Depth: ColorDepthANSI16})
	if th.FG == Dark.FG || th.FG == Light.FG {
		t.Fatal("unknown auto profile selected a fixed built-in theme")
	}
	if got := th.FGColor(th.Accent, "x"); got != "\x1b[94mx\x1b[0m" {
		t.Fatalf("terminal palette fallback = %q", got)
	}
}

func TestTerminalPaletteSlotsDoNotAliasExplicitIndexedColors(t *testing.T) {
	profile := terminalTestProfile(ColorDepthTrueColor)
	th := TerminalTheme(profile)
	if got := th.FGColor(TerminalPaletteSlot(12), "x"); !strings.Contains(got, "40;100;240") {
		t.Fatalf("terminal slot = %q", got)
	}
	if got := th.FGColor(Color256(12), "x"); !strings.Contains(got, "0;0;255") {
		t.Fatalf("explicit xterm index adopted terminal palette: %q", got)
	}
}

func TestTerminalThemeRendererInvalidationDoesNotClearScrollback(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "")
	var buf bytes.Buffer
	r := NewRenderer(&buf)
	r.Resize(20, 2)
	buf.Reset()
	r.SetTheme(TerminalTheme(terminalTestProfile(ColorDepthTrueColor)))
	r.Invalidate()
	if strings.Contains(buf.String(), SeqClearScrollback) {
		t.Fatalf("theme invalidation cleared scrollback: %q", buf.String())
	}
}
