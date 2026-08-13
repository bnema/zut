package modes

import (
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/tui"
	"github.com/mattn/go-runewidth"
)

// renderResidentSubagentActivityLines renders the bounded, non-sensitive
// background work indicator shown directly below the main editor. A queued
// child is waiting for a resident scheduler slot; only running children use
// the animated glyph.
func renderResidentSubagentActivityLines(theme tui.Theme, spinnerGlyph string, snapshots []subagents.ResidentSnapshot, width int) []string {
	lines := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.State != subagents.ResidentQueued && snapshot.State != subagents.ResidentRunning {
			continue
		}
		name := sanitizeSessionTreeText(snapshot.Profile)
		if name == "" {
			name = sanitizeSessionTreeText(snapshot.ID)
		}
		if name == "" {
			name = "subagent"
		}
		glyph := "…"
		color := theme.Muted
		state := "queued"
		if snapshot.State == subagents.ResidentRunning {
			glyph = strings.TrimSpace(spinnerGlyph)
			if glyph == "" {
				glyph = "."
			}
			color = theme.Spinner
			state = "running"
		}
		plain := fmt.Sprintf("%s %s · %s", glyph, name, state)
		plain = truncateResidentSubagentIndicator(plain, width-2)
		if plain == "" {
			continue
		}
		lines = append(lines, "  "+theme.FGColor(color, plain))
	}
	return lines
}

func limitResidentSubagentActivityLines(theme tui.Theme, lines []string, hidden, maxRows, width int) []string {
	if maxRows <= 0 || (len(lines) == 0 && hidden == 0) {
		return nil
	}
	if hidden == 0 && len(lines) <= maxRows {
		return lines
	}
	if maxRows == 1 {
		return []string{residentSubagentActivityOverflowLine(theme, len(lines)+hidden, width)}
	}
	visible := len(lines)
	if visible > maxRows-1 {
		visible = maxRows - 1
	}
	out := append([]string(nil), lines[:visible]...)
	return append(out, residentSubagentActivityOverflowLine(theme, hidden+len(lines)-visible, width))
}

func residentSubagentActivityOverflowLine(theme tui.Theme, hidden, width int) string {
	text := fmt.Sprintf("  … %d more active subagents", hidden)
	return theme.FGColor(theme.Muted, truncateResidentSubagentIndicator(text, width))
}

func truncateResidentSubagentIndicator(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= width {
		return s
	}
	if width <= 3 {
		return strings.Repeat(".", width)
	}
	return runewidth.Truncate(s, width, "...")
}
