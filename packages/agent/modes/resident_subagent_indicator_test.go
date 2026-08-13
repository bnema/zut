package modes

import (
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/tui"
)

func TestRenderResidentSubagentActivityLinesShowsOnlyActiveChildren(t *testing.T) {
	lines := renderResidentSubagentActivityLines(tui.Dark, "/", []subagents.ResidentSnapshot{
		{ID: "running-id", Profile: "reviewer", State: subagents.ResidentRunning},
		{ID: "queued-id", Profile: "planner", State: subagents.ResidentQueued},
		{ID: "idle-id", Profile: "finished", State: subagents.ResidentIdle},
	}, 80)
	got := strings.Join(plainResidentIndicatorLines(lines), "\n")
	if !strings.Contains(got, "/ reviewer · running") || !strings.Contains(got, "… planner · queued") {
		t.Fatalf("activity lines = %q", got)
	}
	if strings.Contains(got, "finished") {
		t.Fatalf("activity lines include inactive child: %q", got)
	}
}

func TestLimitResidentSubagentActivityLinesSummarizesOverflow(t *testing.T) {
	got := plainResidentIndicatorLines(limitResidentSubagentActivityLines(tui.Dark, []string{"one", "two", "three"}, 0, 2, 80))
	want := []string{"one", "  … 2 more active subagents"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("limited lines = %#v, want %#v", got, want)
	}
}

func TestLimitResidentSubagentActivityLinesIncludesAlreadyHiddenChildren(t *testing.T) {
	got := plainResidentIndicatorLines(limitResidentSubagentActivityLines(tui.Dark, []string{"one", "two", "three"}, 4, 2, 80))
	want := []string{"one", "  … 6 more active subagents"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("limited lines = %#v, want %#v", got, want)
	}
}

func plainResidentIndicatorLines(lines []string) []string {
	plain := make([]string, len(lines))
	for index, line := range lines {
		plain[index] = stripANSIBytes(line)
	}
	return plain
}
