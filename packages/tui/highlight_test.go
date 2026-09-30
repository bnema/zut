package tui

import (
	"regexp"
	"strings"
	"testing"
)

func TestStripANSIBackgroundsKeepsExtendedColors(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"256 foreground in background range", "\x1b[38;5;45mstring\x1b[0m", "\x1b[38;5;45mstring\x1b[0m"},
		{"256 foreground index 48", "\x1b[38;5;48mx\x1b[0m", "\x1b[38;5;48mx\x1b[0m"},
		{"256 foreground index 49", "\x1b[38;5;49mx\x1b[0m", "\x1b[38;5;49mx\x1b[0m"},
		{"256 foreground bright range", "\x1b[38;5;101mcomment\x1b[0m", "\x1b[38;5;101mcomment\x1b[0m"},
		{"256 foreground index 107", "\x1b[38;5;107mx\x1b[0m", "\x1b[38;5;107mx\x1b[0m"},
		{"bold plus foreground", "\x1b[1;38;5;45mstring\x1b[0m", "\x1b[1;38;5;45mstring\x1b[0m"},
		{"truecolor foreground", "\x1b[38;2;40;101;49mstring\x1b[0m", "\x1b[38;2;40;101;49mstring\x1b[0m"},
		{"plain text", "hello", "hello"},
		{"reset", "\x1b[0m", "\x1b[0m"},
		{"256 background dropped", "\x1b[48;5;45mbg\x1b[0m", "bg\x1b[0m"},
		{"separate background dropped", "\x1b[38;5;45m\x1b[48;5;0mtext", "\x1b[38;5;45mtext"},
		{"combined background dropped", "\x1b[38;5;45;48;5;0mtext", "\x1b[38;5;45mtext"},
		{"truecolor background dropped", "\x1b[48;2;9;0;21mbg\x1b[0m", "bg\x1b[0m"},
		{"default background dropped", "\x1b[49mbg", "bg"},
		{"ANSI background dropped", "\x1b[41mbg", "bg"},
		{"bright background dropped", "\x1b[101mbg", "bg"},
		{"bare foreground introducer dropped", "\x1b[38mtext", "text"},
		{"truncated indexed foreground", "\x1b[38;5mstring", "string"},
		{"truncated indexed background", "\x1b[48;5mstring", "string"},
		{"truncated truecolor foreground", "\x1b[38;2;1;2mtext", "text"},
		{"truncated truecolor background", "\x1b[48;2;1;2mtext", "text"},
		// An unsupported extended selector is not consumed as a color unit;
		// unrelated standalone attributes keep their ordinary SGR semantics.
		{"unknown selector preserves reverse", "\x1b[38;7mtext", "\x1b[7mtext"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripANSIBackgrounds(tc.in); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Chroma maps this custom type color to xterm index 45, which the old
// background filter removed from the foreground unit.
func TestHighlightCodeExtendedColorIsWellFormed(t *testing.T) {
	th := Dark
	th.Syntax = SyntaxTheme{KeywordType: "#00e5ff", NameBuiltin: "#00e5ff"}
	joined := strings.Join(th.HighlightCode("func splitFrontmatter(raw string) (string, string) {\n", "go"), "\n")
	if strings.Contains(joined, "\x1b[38;5m") || strings.Contains(joined, "\x1b[48;5m") {
		t.Fatalf("highlighting emitted a malformed extended color: %q", joined)
	}
	idx := strings.Index(joined, "string")
	if idx < 0 {
		t.Fatalf("type keyword missing: %q", joined)
	}
	prefix := joined[:idx]
	escape := strings.LastIndex(prefix, "\x1b[")
	if escape < 0 || !regexp.MustCompile(`^\x1b\[(?:[0-9]+;)*38;5;[0-9]+m$`).MatchString(prefix[escape:]) {
		t.Fatalf("type keyword lost its foreground color: %q", joined)
	}
}
