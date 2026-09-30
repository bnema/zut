package modes

import (
	"os"
	"strings"
)

// slideBackChordHint returns the keyboard chord the user must press
// to peel the most recently queued ("sliding in") message back into
// the editor. The chord itself is decoded the same way everywhere
// (see KeyUp with k.Alt in handleKey) — what changes is which
// physical combo actually delivers a modified-arrow escape sequence
// to the process:
//
//   - Ghostty, iTerm2 (with the "Esc+" / Meta=Option setting),
//     Terminal.app (with "Use Option as Meta key"), Alacritty,
//     Kitty: bare Option+Up sends \x1b[1;3A (mod=3 / Alt) which
//     our parser reads as KeyUp + Alt. Good.
//
//   - VS Code's integrated terminal (xterm.js) on macOS does NOT
//     forward Option+Up as an Alt-modified arrow by default — it
//     either swallows the Option as a compose modifier or emits a
//     character. But Option+Shift+Up reliably sends \x1b[1;4A
//     (mod=4 / Shift+Alt) because the Shift bit forces the
//     CSI-with-modifier path. Our parser already treats mod=4 as
//     alt=true, so the binding fires; we just need to tell the
//     user the right chord.
//
//   - Vev reserves Alt+Up for pane navigation. Alt+Shift+Up is
//     forwarded to the child process as \x1b[1;4A instead.
//
// Detection is by $TERM_PROGRAM; VS Code sets this to "vscode" on
// every platform it ships an integrated terminal on. The decoder
// accepts both chords on any terminal, so getting the env-var sniff
// wrong only ever shows a slightly-off hint string — never breaks
// the binding itself.
func slideBackChordHint() string {
	if strings.EqualFold(os.Getenv("TERM_PROGRAM"), "vev") {
		return "Alt+Shift+↑"
	}
	if strings.EqualFold(os.Getenv("TERM_PROGRAM"), "vscode") {
		return "Option+Shift+↑"
	}
	return "Option+↑"
}
