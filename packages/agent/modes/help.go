package modes

import (
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/tui"
	"github.com/mattn/go-runewidth"
)

// helpKeyRows is the list of keybindings shown by /help.
var helpKeyRows = [][2]string{
	{"enter", "submit the current input"},
	{"shift+enter / alt+enter", "insert a newline"},
	{"tab", "complete the highlighted slash command"},
	{"esc", "cancel the current turn (while busy) - clear idle input; two bare Esc presses within 500ms open /session tree"},
	{"ctrl+c", "exit (while idle) - cancel the current turn (while busy)"},
	{"ctrl+w", "delete previous word"},
	{"alt+backspace", "delete previous word (same as ctrl+w)"},
	{"ctrl+u / ctrl+k", "delete to start / end of line"},
	{"ctrl+a / ctrl+e", "jump to start / end of line"},
	{"alt+← / alt+→", "jump one word back / forward"},
	{"ctrl+l", "redraw the screen"},
	{"ctrl+o", "expand / collapse long tool results"},
	{"ctrl+r", "cycle reasoning levels for the active model"},
	{"pgup / pgdn", "scroll the chat one page up / down"},
	{"up / down", "move in input, browse prompt history at top edge, scroll chat when empty"},
}

// renderHelpBlock builds the friendly /help view. Uses the shared
// frameHeader/frameRule helpers so the rules match every other block
// in the tui (tool results, code fences, dialogs) — full terminal width
// in the muted colour.
//
// Slash commands and keybindings share the same label column width so
// every description starts at the same x-position, regardless of which
// section it lives in. The width is computed from the longest label
// across BOTH lists, with a minimum of 14 cells so changes to either
// list don't compress the column visually.
func renderHelpBlock(th tui.Theme, width int, llamaConfigured bool) []string {
	if width < 20 {
		width = 20
	}

	// Label column width uses display cells, not byte length, so
	// single-cell multibyte runes (← → - ...) don't over-count and leave
	// a raggedy right edge. `len("alt+← / alt+→")` is 17 bytes but
	// only 13 cells; padding off byte length would either overshoot
	// (setting labelWidth too high and wasting space on every row)
	// or undershoot (never padding that row because len >= labelWidth
	// already, leaving its description mis-aligned).
	labelWidth := 14
	for _, c := range slashCatalog {
		if c.Name == "/llama" && !llamaConfigured {
			continue
		}
		if n := runewidth.StringWidth(c.Name); n > labelWidth {
			labelWidth = n
		}
	}
	for _, k := range helpKeyRows {
		if n := runewidth.StringWidth(k[0]); n > labelWidth {
			labelWidth = n
		}
	}

	pad := func(s string) string {
		w := runewidth.StringWidth(s)
		if w >= labelWidth {
			return s
		}
		return s + strings.Repeat(" ", labelWidth-w)
	}

	var out []string
	out = append(out, frameHeader(th, "zut help", width), "")

	// commands section
	out = append(out, tui.Bold("slash commands:"))
	for _, c := range slashCatalog {
		if c.Name == "/llama" && !llamaConfigured {
			continue
		}
		out = append(out, fmt.Sprintf("  %s  %s",
			th.FGColor(th.Accent, pad(c.Name)),
			th.FGColor(th.Muted, c.Desc)))
	}

	// keys section
	out = append(out, "", tui.Bold("keys:"))
	for _, k := range helpKeyRows {
		out = append(out, fmt.Sprintf("  %s  %s",
			th.FGColor(th.Accent, pad(k[0])),
			th.FGColor(th.Muted, k[1])))
	}

	out = append(out, "", frameRule(th, width), "")
	return out
}
