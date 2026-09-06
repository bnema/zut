package tui

import (
	"fmt"
	"strings"
	"testing"
)

func TestReaderParsesAZERTYProfileKeys(t *testing.T) {
	for _, r := range "&é\"'(-è_ç" {
		for _, sequence := range []string{
			fmt.Sprintf("\x1b[%d;5u", r),
			fmt.Sprintf("\x1b[%d:49:49;5u", r),
			fmt.Sprintf("\x1b[27;5;%d~", r),
		} {
			k := readKey(t, sequence)
			if k.Kind != KeyRune || k.Rune != r || !k.Ctrl {
				t.Fatalf("%q: %+v", sequence, k)
			}
		}
		k := readKey(t, fmt.Sprintf("\x1b[%d;1u", r))
		if k.Kind != KeyRune || k.Rune != r || k.Ctrl {
			t.Fatalf("unmodified %c: %+v", r, k)
		}
	}
}

func TestStatusBarModelProfileIndicator(t *testing.T) {
	for _, cols := range []int{10, 25, 80, 160} {
		lines := StatusBar(StatusBarParams{Theme: Dark, Model: "gpt-5.6-sol", Reasoning: "max", ModelProfile: 9, Cols: cols})
		if !strings.Contains(stripANSI(strings.Join(lines, "\n")), "[9]") {
			t.Fatalf("missing profile at width %d: %q", cols, lines)
		}
		for _, line := range lines {
			if visibleWidth(line) > cols {
				t.Fatalf("overflow at width %d: %q", cols, line)
			}
		}
	}
}
