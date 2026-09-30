package tui

import (
	"reflect"
	"strings"
	"testing"
)

func TestPinnedSkillsMatchesResourceSections(t *testing.T) {
	th := Theme{Accent: Color256(75), Muted: Color256(245)}
	got := RenderPinnedSkills(th, []string{"review", "vscode"}, 80)
	want := []string{"", th.FGColor(th.Accent, "[Pinned skills]"), th.FGColor(th.Muted, "  review, vscode")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %q, want %q", got, want)
	}
	if got := RenderPinnedSkills(th, nil, 80); len(got) != 0 {
		t.Fatalf("empty pins rendered rows: %q", got)
	}
}

func TestPinnedSkillsWrapNamesToTerminalWidth(t *testing.T) {
	for _, width := range []int{1, 4, 12, 30, 80} {
		rows := RenderPinnedSkills(Theme{}, []string{strings.Repeat("界", 20), "review", "vscode"}, width)
		for _, row := range rows {
			if got := visibleWidth(row); got > width {
				t.Fatalf("row width %d exceeds %d: %q", got, width, row)
			}
		}
		if width >= 4 {
			body := strings.Join(rows[2:], "")
			if !strings.Contains(body, "vscode") && width >= 12 {
				t.Fatalf("last skill was truncated: %q", rows)
			}
		}
	}
}
