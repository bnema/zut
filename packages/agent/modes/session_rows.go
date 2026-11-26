package modes

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
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
