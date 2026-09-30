package tui

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

// RenderPinnedSkills matches the startup resource sections, with an accent
// heading and indented, muted names. A leading blank separates it from chat.
func RenderPinnedSkills(th Theme, names []string, width int) []string {
	if len(names) == 0 || width < 4 {
		return nil
	}
	out := []string{"", th.FGColor(th.Accent, runewidth.Truncate("[Pinned skills]", width, "..."))}
	for _, line := range wrapLine(strings.Join(names, ", "), width-2, "") {
		out = append(out, th.FGColor(th.Muted, "  "+line))
	}
	return out
}
