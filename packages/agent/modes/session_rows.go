package modes

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/tui"
)

// clampViewTop returns a viewTop that keeps cursor visible in a
// window of the given size over a list of `total` rows. Leaves one
// row of padding above/below where possible so moving the cursor
// doesn't land right on the top/bottom edge — easier to see what
// direction you're moving.
func clampViewTop(viewTop, cursor, window, total int) int {
	if window <= 0 || total <= 0 {
		return 0
	}
	if window >= total {
		return 0
	}
	pad := 2
	if window < 6 {
		pad = 0
	}
	if cursor < viewTop+pad {
		viewTop = cursor - pad
	}
	if cursor >= viewTop+window-pad {
		viewTop = cursor - window + pad + 1
	}
	if viewTop < 0 {
		viewTop = 0
	}
	if viewTop+window > total {
		viewTop = total - window
	}
	return viewTop
}

func highlightSessionSearchExcerpt(th tui.Theme, excerpt string, indexes []int, maxWidth int) string {
	limit := 240
	if maxWidth > 0 && maxWidth < limit {
		limit = maxWidth
	}
	excerpt, indexes = windowSessionSearchExcerpt(excerpt, indexes, limit)
	if len(indexes) == 0 {
		return th.FGColor(th.Muted, excerpt)
	}
	matched := make(map[int]bool, len(indexes))
	for _, index := range indexes {
		matched[index] = true
	}
	var out strings.Builder
	for index := 0; index < len(excerpt); {
		r, size := utf8.DecodeRuneInString(excerpt[index:])
		part := string(r)
		if matched[index] {
			out.WriteString(th.FGColor(th.Accent, part))
		} else {
			out.WriteString(th.FGColor(th.Muted, part))
		}
		index += size
	}
	return out.String()
}

func windowSessionSearchExcerpt(text string, indexes []int, limit int) (string, []int) {
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text, indexes
	}
	matchRune := 0
	if len(indexes) > 0 {
		for runeIndex, byteIndex := range runeByteOffsets(text) {
			if byteIndex <= indexes[0] {
				matchRune = runeIndex
			} else {
				break
			}
		}
	}
	room := limit - 3
	if room < 1 {
		return string(runes[:limit]), nil
	}
	start := max(0, matchRune-room/2)
	if start+room > len(runes) {
		start = len(runes) - room
	}
	end := min(len(runes), start+room)
	prefix := ""
	if start > 0 {
		prefix = "..."
	}
	suffix := ""
	if end < len(runes) {
		suffix = "..."
	}
	base := len(string(runes[:start]))
	stop := len(string(runes[:end]))
	rebased := make([]int, 0, len(indexes))
	for _, index := range indexes {
		if index >= base && index < stop {
			rebased = append(rebased, len(prefix)+index-base)
		}
	}
	return prefix + string(runes[start:end]) + suffix, rebased
}

func runeByteOffsets(text string) []int {
	offsets := make([]int, 0, len(text))
	for index := range text {
		offsets = append(offsets, index)
	}
	return offsets
}

func formatRelative(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	default:
		return t.Local().Format("2006-01-02")
	}
}

// liveAttachLabel renders a session's attach state for the live section.
func liveAttachLabel(info session.Info, self session.ID) string {
	if session.ID(info.ID) == self {
		return "this terminal"
	}
	if info.Attach == session.AttachStateAttached {
		return "attached"
	}
	return "detached"
}

func sessionProject(repoRoot, cwd string) string {
	path := strings.TrimSpace(repoRoot)
	if path == "" {
		path = strings.TrimSpace(cwd)
	}
	if path == "" {
		return "-"
	}
	return sanitizeSessionTreeText(filepath.Base(path))
}

func liveRowColumns(info session.Info, self session.ID) []string {
	state := "○ idle"
	residents := false
	for _, r := range info.Residents {
		if r.State == "running" || r.State == "queued" {
			residents = true
		}
	}
	// Run includes resident work, so idle activity distinguishes waiting from
	// a foreground turn even when Run is running.
	if residents && (info.Run == session.RunStateIdle || info.Activity.Kind == "idle") {
		state = "◌ waiting agents"
	} else if info.Run == session.RunStateRunning || info.Run == session.RunStateCompacting {
		state = "● running"
		if tool := strings.TrimSpace(info.Activity.Tool); tool != "" {
			state += " · " + tool
		}
	}
	when := info.LastUserInputAt
	if when.IsZero() {
		when = info.UpdatedAt
	}
	model := info.Model
	if idx := strings.LastIndex(model, "/"); idx >= 0 {
		model = model[idx+1:]
	}
	model = strings.TrimPrefix(model, "claude-")
	if info.Reasoning != "" {
		model += ":" + info.Reasoning
	}
	title := strings.TrimSpace(info.Title)
	if title == "" && info.Goal != nil && strings.TrimSpace(info.Goal.Objective) != "" {
		title = "goal: " + info.Goal.Objective
	}
	if title == "" && info.PlanStep != nil {
		title = strings.TrimSpace(info.PlanStep.Text)
	}
	if title == "" {
		title = "(new session)"
	}
	columns := []string{liveAttachLabel(info, self), sessionProject(info.RepoRoot, info.CWD), state, formatRelative(when), model, fmt.Sprintf("$%.2f", info.Usage.CostUSD), title}
	for idx := range columns {
		columns[idx] = sanitizeSessionTreeText(columns[idx])
	}
	return columns
}

// liveColumns lists the fixed-width live columns (index into liveRowColumns)
// in display order with their drop priority; higher drops first when the
// dialog is too narrow to keep liveTitleMin cells for the title.
var liveColumns = []struct{ index, width, drop int }{
	{0, 13, 3}, {1, 12, 0}, {2, 18, 1}, {3, 11, 2}, {4, 16, 4}, {5, 7, 5},
}

const liveTitleMin = 24

// keptLiveColumns depends only on width so columns never shift between
// refreshes.
func keptLiveColumns(width int) []bool {
	keep := make([]bool, len(liveColumns))
	used := 2 + liveTitleMin
	for i := range keep {
		keep[i] = true
		used += liveColumns[i].width + 2
	}
	for drop := len(liveColumns) - 1; drop >= 1 && used > width; drop-- {
		for i, c := range liveColumns {
			if c.drop == drop && keep[i] {
				keep[i] = false
				used -= c.width + 2
			}
		}
	}
	return keep
}

func liveColumnWidths(width int) []int {
	keep := keptLiveColumns(width)
	var widths []int
	for i, c := range liveColumns {
		if keep[i] {
			widths = append(widths, c.width)
		}
	}
	return widths
}

func selectLiveColumns(all []string, width int) []string {
	keep := keptLiveColumns(width)
	var out []string
	for i, c := range liveColumns {
		if keep[i] {
			out = append(out, all[c.index])
		}
	}
	return append(out, all[len(all)-1])
}
