package modes

import (
	"fmt"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/tui"
	"github.com/mattn/go-runewidth"
)

// renderSubagentActivityLines renders only trace-observed open operations.
// A living process, generic lifecycle state, or heartbeat never starts a
// spinner because none proves that an operation is progressing.
func renderSubagentActivityLines(th tui.Theme, spinnerGlyph string, snapshots []subagents.AgentSnapshot, views map[string]subagents.AgentTraceView, width int, now time.Time) []string {
	lines := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		view := views[snapshot.ID]
		operation := view.PrimaryOperation
		if operation == nil && len(view.OpenOperations) != 0 {
			operation = &view.OpenOperations[0]
		}
		if operation == nil {
			continue
		}
		line := renderSubagentActivityLine(th, spinnerGlyph, snapshot, *operation, view.ObservationFor(*operation), width, now)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func renderSubagentActivityLine(th tui.Theme, spinnerGlyph string, snapshot subagents.AgentSnapshot, operation subagents.Operation, observation *subagents.LiveObservation, width int, now time.Time) string {
	name := sanitizeSubagentIndicatorText(snapshot.Subagent)
	if name == "" {
		name = sanitizeSubagentIndicatorText(snapshot.ID)
	}
	if name == "" {
		name = "subagent"
	}
	spinnerGlyph = strings.TrimSpace(spinnerGlyph)
	if spinnerGlyph == "" {
		spinnerGlyph = "."
	}
	activity := operation.Label()
	if observation != nil {
		activity += " · " + observation.Label() + " " + formatSubagentActivityAge(observation.At, now) + " ago"
	}
	age := formatSubagentActivityAge(operation.StartedAt, now)
	plain, layout := fitSubagentActivityLine(spinnerGlyph, name, activity, age, width-2)
	if plain == "" {
		return ""
	}

	switch layout {
	case subagentActivityFull:
		parts := strings.SplitN(plain, " · ", 3)
		if len(parts) != 3 {
			return "  " + th.FGColor(th.Muted, plain)
		}
		name = strings.TrimPrefix(parts[0], spinnerGlyph+" ")
		return "  " + th.FGColor(th.Spinner, spinnerGlyph) + " " +
			th.FGColor(th.Assistant, name) + th.FGColor(th.Muted, " · ") +
			th.FGColor(th.FG, parts[1]) + th.FGColor(th.Muted, " · "+parts[2])
	case subagentActivityNameAge:
		parts := strings.SplitN(plain, " · ", 2)
		if len(parts) != 2 {
			return "  " + th.FGColor(th.Muted, plain)
		}
		name = strings.TrimPrefix(parts[0], spinnerGlyph+" ")
		return "  " + th.FGColor(th.Spinner, spinnerGlyph) + " " +
			th.FGColor(th.Assistant, name) + th.FGColor(th.Muted, " · "+parts[1])
	default:
		return "  " + th.FGColor(th.Muted, plain)
	}
}

type subagentActivityLayout uint8

const (
	subagentActivityFull subagentActivityLayout = iota
	subagentActivityNameAge
	subagentActivityCompact
)

func fitSubagentActivityLine(spinnerGlyph, name, activity, age string, width int) (string, subagentActivityLayout) {
	if width <= 0 {
		return "", subagentActivityCompact
	}
	full := spinnerGlyph + " " + name + " · " + activity + " · " + age
	if runewidth.StringWidth(full) <= width {
		return full, subagentActivityFull
	}
	nameAge := spinnerGlyph + " " + name + " · " + age
	if activityWidth := width - runewidth.StringWidth(nameAge) - runewidth.StringWidth(" · "); activityWidth >= 4 {
		return spinnerGlyph + " " + name + " · " + truncateSubagentIndicatorText(activity, activityWidth) + " · " + age, subagentActivityFull
	}
	if runewidth.StringWidth(nameAge) <= width {
		return nameAge, subagentActivityNameAge
	}
	return truncateSubagentIndicatorText(nameAge, width), subagentActivityCompact
}

func formatSubagentActivityAge(started, now time.Time) string {
	if started.IsZero() {
		return "-"
	}
	if now.IsZero() {
		now = time.Now()
	}
	elapsed := now.Sub(started)
	if elapsed < 0 {
		elapsed = 0
	}
	seconds := int(elapsed.Seconds())
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 60*60:
		return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	case seconds < 24*60*60:
		return fmt.Sprintf("%dh%02dm", seconds/(60*60), (seconds%(60*60))/60)
	default:
		return fmt.Sprintf("%dd%02dh", seconds/(24*60*60), (seconds%(24*60*60))/(60*60))
	}
}

func truncateSubagentIndicatorText(s string, width int) string {
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

func sanitizeSubagentIndicatorText(s string) string { return sanitizeSessionTreeText(s) }

func limitSubagentActivityLines(th tui.Theme, lines []string, maxRows, width int) []string {
	if maxRows <= 0 || len(lines) == 0 {
		return nil
	}
	if len(lines) <= maxRows {
		return lines
	}
	if maxRows == 1 {
		return []string{subagentActivityOverflowLine(th, len(lines), width)}
	}
	out := append([]string(nil), lines[:maxRows-1]...)
	return append(out, subagentActivityOverflowLine(th, len(lines)-len(out), width))
}

func subagentActivityOverflowLine(th tui.Theme, hidden, width int) string {
	text := fmt.Sprintf("  … %d more open subagent operations", hidden)
	return th.FGColor(th.Muted, truncateSubagentIndicatorText(text, width))
}

func (i *Interactive) activeSubagentActivitySnapshots() ([]subagents.AgentSnapshot, map[string]subagents.AgentTraceView) {
	supervisor := i.cfg.Supervisor
	if supervisor == nil {
		return nil, nil
	}
	views := supervisor.TraceViews()
	activeSession := supervisor.ActiveSession()
	agents := supervisor.List()
	out := make([]subagents.AgentSnapshot, 0, len(agents))
	for _, agent := range agents {
		if activeSession != "" && agent.SessionID != "" && agent.SessionID != activeSession {
			continue
		}
		view := views[agent.ID]
		if view.PrimaryOperation == nil && len(view.OpenOperations) == 0 {
			continue
		}
		out = append(out, subagents.AgentSnapshot{ID: agent.ID, Started: agent.Started, Subagent: agent.Subagent})
	}
	return out, views
}
