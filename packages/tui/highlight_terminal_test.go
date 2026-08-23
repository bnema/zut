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
