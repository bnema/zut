package tui

import (
	"strings"
	"testing"
)

func TestTerminalSyntaxPreservesPaletteSlots(t *testing.T) {
	profile := TerminalProfile{Depth: ColorDepthIndexed256}
	th := TerminalTheme(profile)
	got := strings.Join(th.HighlightCode("func main() { return 1 }", "go"), "\n")
	for _, sequence := range []string{"\x1b[94m", "\x1b[93m", "\x1b[39m"} {
		if !strings.Contains(got, sequence) {
			t.Fatalf("highlight output missing %q: %q", sequence, got)
		}
	}
	if strings.Contains(got, "\x1b[38;5;196m") {
		t.Fatalf("terminal formatter remapped ANSI slots through xterm cube: %q", got)
	}
}

func TestTerminalSyntaxUsesReportedRGBPalette(t *testing.T) {
	profile := terminalTestProfile(ColorDepthTrueColor)
	th := TerminalTheme(profile)
	got := strings.Join(th.HighlightCode("func main() {}", "go"), "\n")
	if !strings.Contains(got, "\x1b[38;2;40;100;240m") {
		t.Fatalf("highlight output did not use reported bright-blue palette: %q", got)
	}
}

func TestTerminalSyntaxHonorsCustomOverrides(t *testing.T) {
	th := TerminalTheme(TerminalProfile{Depth: ColorDepthTrueColor})
	th.Syntax.Keyword = "#112233 bold"
	got := strings.Join(th.HighlightCode("func main() {}", "go"), "\n")
	if !strings.Contains(got, "\x1b[38;2;17;34;51m\x1b[1mfunc") {
		t.Fatalf("terminal formatter ignored custom keyword override: %q", got)
	}
}

func TestTerminalSyntaxHonorsCustomBaseStyle(t *testing.T) {
	th := TerminalTheme(TerminalProfile{Depth: ColorDepthTrueColor})
	th.SyntaxBaseStyle = "monokai"
	got := strings.Join(th.HighlightCode("func main() {}", "go"), "\n")
	if !strings.Contains(got, "\x1b[38;2;102;217;239m") {
		t.Fatalf("terminal formatter ignored custom syntax base style: %q", got)
	}
}

func TestTerminalSyntaxBalancesMultilineTokens(t *testing.T) {
	th := TerminalTheme(TerminalProfile{Depth: ColorDepthIndexed256})
	lines := th.HighlightCode("/* first\nsecond */", "go")
	if len(lines) != 2 {
		t.Fatalf("line count = %d, want 2: %#v", len(lines), lines)
	}
	for _, line := range lines {
		if !strings.HasSuffix(line, reset) {
			t.Fatalf("line does not reset terminal style: %q", line)
		}
	}
}
