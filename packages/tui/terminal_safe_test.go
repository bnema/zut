package tui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

// assertTerminalSafeRows checks the invariant the renderer relies on:
// once SGR color codes are removed, every row contains only printable
// text, fits the terminal, and box side rows reach the right edge.
func assertTerminalSafeRows(t *testing.T, rows []string, width int) {
	t.Helper()
	for i, row := range rows {
		plain := stripSGR(row)
		for _, r := range plain {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
				t.Fatalf("row %d contains control %U: %q", i, r, row)
			}
		}
		w := runewidth.StringWidth(plain)
		if w > width {
			t.Fatalf("row %d is %d cols wide, terminal is %d: %q", i, w, width, plain)
		}
		trimmed := strings.TrimSpace(plain)
		if strings.HasPrefix(trimmed, "│") && w != width {
			t.Fatalf("box row %d is %d cols wide, want %d: %q", i, w, width, plain)
		}
	}
}

// stripSGR removes only CSI ... m color sequences. Any other escape is
// left in place so the control check above flags it.
func stripSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] >= 0x30 && s[j] <= 0x3f {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
