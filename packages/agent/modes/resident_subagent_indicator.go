package modes

import (
	"fmt"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/tui"
	"github.com/mattn/go-runewidth"
)

var residentProviderWaitingSpinner = tui.NewStringSpinner(
	[]string{"Zzzz", "zZzz", "zzZz", "zzzZ"},
	time.Second,
)

// renderResidentSubagentActivityLines renders the non-sensitive background
// work indicator shown directly below the main editor. A queued child is
// waiting for a resident scheduler slot; only running children use the
// animated glyph. Height limiting happens after this function so every active
// child in the provided page can be shown whenever the terminal has room.
func renderResidentSubagentActivityLines(theme tui.Theme, spinnerGlyph string, snapshots []subagents.ResidentSnapshot, width int, now time.Time) []string {
	lines := make([]string, 0, len(snapshots)*2)
	for _, snapshot := range snapshots {
		// Foreign journals remain discoverable through /subagents, but their
		// activity belongs to another zut host and must not occupy this
		// session's always-visible editor-adjacent indicator.
		if snapshot.OwnedElsewhere || (snapshot.State != subagents.ResidentQueued && snapshot.State != subagents.ResidentRunning) {
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
		activity := "queued"
		if snapshot.State == subagents.ResidentRunning {
			glyph = strings.TrimSpace(spinnerGlyph)
			if glyph == "" {
				glyph = "."
			}
			color = theme.Spinner
			if snapshot.WaitingForModel {
				activity = residentProviderWaitingSpinner.FrameAt(activityTime(snapshot), now)
			} else {
				activity = "running"
			}
		}
		plain := fmt.Sprintf("%s %s · %s · activity %s ago · %s", glyph, name, activity,
			formatResidentSubagentActivityAge(activityTime(snapshot), now),
			formatResidentSubagentActivityAge(turnStartTime(snapshot), now))
		lines = append(lines, renderResidentSubagentActivityGroup(theme, color, plain, residentUsageMetadata(snapshot), width)...)
	}
	return lines
}

func renderResidentSubagentActivityGroup(theme tui.Theme, color tui.TerminalColor, activity, metadata string, width int) []string {
	if width <= 0 {
		return nil
	}
	const activityIndent = "  "
	const metadataIndent = "    "
	activityWidth := runewidth.StringWidth(activity)
	metadataWidth := runewidth.StringWidth(metadata)
	if metadata != "" && runewidth.StringWidth(activityIndent)+activityWidth+1+metadataWidth <= width {
		gap := width - runewidth.StringWidth(activityIndent) - activityWidth - metadataWidth
		if gap < 1 {
			gap = 1
		}
		return []string{activityIndent + theme.FGColor(color, activity) + strings.Repeat(" ", gap) + theme.FGColor(theme.Muted, metadata)}
	}

	activity = truncateResidentSubagentIndicator(activity, width-runewidth.StringWidth(activityIndent))
	if activity == "" {
		return nil
	}
	lines := []string{activityIndent + theme.FGColor(color, activity)}
	if metadata == "" {
		return lines
	}
	metadataWidthLimit := width - runewidth.StringWidth(metadataIndent)
	if metadataWidthLimit <= 0 {
		return lines
	}
	for _, metadataLine := range tui.WrapANSILine(metadata, metadataWidthLimit) {
		metadataLine = truncateResidentSubagentIndicator(metadataLine, metadataWidthLimit)
		if metadataLine != "" {
			lines = append(lines, metadataIndent+theme.FGColor(theme.Muted, metadataLine))
		}
	}
	return lines
}

func residentUsageMetadata(snapshot subagents.ResidentSnapshot) string {
	parts := tui.UsageStatsParts(tui.UsageStatsParams{Usage: snapshot.Usage, Subscription: snapshot.Subscription})
	if context := tui.ContextUsageText(snapshot.ContextUsed, snapshot.ContextMax); context != "" {
		parts = append(parts, context)
	}
	if snapshot.Budget.Limit > 0 {
		parts = append(parts, fmt.Sprintf("budget:%s %d%%/%s", snapshot.Budget.State, snapshot.Budget.Percent, compactResidentTokens(snapshot.Budget.Limit)))
	}
	return strings.Join(parts, " ")
}

func compactResidentTokens(tokens int64) string {
	switch {
	case tokens >= 999_500:
		return fmt.Sprintf("%.1fM", float64(tokens)/1_000_000)
	case tokens >= 1_000:
		return fmt.Sprintf("%.0fk", float64(tokens)/1_000)
	default:
		return fmt.Sprintf("%d", tokens)
	}
}

func turnStartTime(snapshot subagents.ResidentSnapshot) time.Time {
	if !snapshot.TurnStartedAt.IsZero() {
		return snapshot.TurnStartedAt
	}
	return snapshot.UpdatedAt
}

func activityTime(snapshot subagents.ResidentSnapshot) time.Time {
	if !snapshot.ActivityUpdatedAt.IsZero() {
		return snapshot.ActivityUpdatedAt
	}
	return snapshot.UpdatedAt
}

func formatResidentSubagentActivityAge(started, now time.Time) string {
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

func renderResidentSubagentActivityPage(theme tui.Theme, manager *subagents.ResidentManager, spinnerGlyph string, snapshotLimit, width int, now time.Time) ([]string, int) {
	if manager == nil || snapshotLimit <= 0 {
		return nil, 0
	}
	snapshots, total := manager.ActiveSnapshotPage(snapshotLimit)
	hidden := total - len(snapshots)
	if hidden < 0 {
		hidden = 0
	}
	return renderResidentSubagentActivityLines(theme, spinnerGlyph, snapshots, width, now), hidden
}

func fitResidentSubagentActivityLines(theme tui.Theme, lines []string, hidden, maxBottomRows, width int, bottomRows func([]string) int) []string {
	if len(lines) == 0 && hidden == 0 {
		return nil
	}
	for maxRows := len(lines) + 1; maxRows >= 0; maxRows-- {
		candidate := limitResidentSubagentActivityLines(theme, lines, hidden, maxRows, width)
		if maxRows == 0 || bottomRows(candidate) <= maxBottomRows {
			return candidate
		}
	}
	return nil
}

func limitResidentSubagentActivityLines(theme tui.Theme, lines []string, hidden, maxRows, width int) []string {
	if maxRows <= 0 || (len(lines) == 0 && hidden == 0) {
		return nil
	}
	groups := residentSubagentActivityLineGroups(lines)
	if len(groups) == 0 {
		return []string{residentSubagentActivityOverflowLine(theme, hidden, width)}
	}
	if hidden == 0 && len(lines) <= maxRows {
		return lines
	}

	if maxRows == 1 {
		if hidden == 0 && len(groups) == 1 {
			return []string{groups[0][0]}
		}
		return []string{residentSubagentActivityOverflowLine(theme, hidden+len(groups), width)}
	}

	visibleGroups := len(groups)
	overflowHidden := hidden
	if hidden > 0 {
		if visibleGroups > maxRows-1 {
			visibleGroups = maxRows - 1
		}
		overflowHidden = hidden + len(groups) - visibleGroups
	} else if visibleGroups > maxRows {
		visibleGroups = maxRows - 1
		overflowHidden = len(groups) - visibleGroups
	}
	if visibleGroups < 0 {
		visibleGroups = 0
	}
	if visibleGroups > len(groups) {
		visibleGroups = len(groups)
	}

	out := make([]string, 0, maxRows)
	for index := 0; index < visibleGroups && len(out) < maxRows; index++ {
		group := groups[index]
		out = append(out, group[0])
		remainingActivityRows := visibleGroups - index - 1
		reservedRows := remainingActivityRows
		if overflowHidden > 0 {
			reservedRows++
		}
		metadataRows := maxRows - len(out) - reservedRows
		for _, metadataLine := range group[1:] {
			if metadataRows <= 0 || len(out) >= maxRows {
				break
			}
			out = append(out, metadataLine)
			metadataRows--
		}
	}
	if overflowHidden > 0 && len(out) < maxRows {
		out = append(out, residentSubagentActivityOverflowLine(theme, overflowHidden, width))
	}
	return out
}

func residentSubagentActivityLineGroups(lines []string) [][]string {
	groups := make([][]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, "    ") && len(groups) > 0 {
			groups[len(groups)-1] = append(groups[len(groups)-1], line)
			continue
		}
		groups = append(groups, []string{line})
	}
	return groups
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
