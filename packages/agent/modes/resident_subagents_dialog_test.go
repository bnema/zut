package modes

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func TestResidentSubagentListUsesHumanLabelTerminalStateAndRelativeTime(t *testing.T) {
	row := subagents.ResidentSnapshot{ID: "b6382ac7-ca36-45f1-9549-dd9ee4d195b7", Profile: "go-reviewer", Model: "gpt-5.6-luna", State: subagents.ResidentIdle}
	if got := residentDisplayName(row); got != "go-reviewer" {
		t.Fatalf("display name = %q", got)
	}
	if got := residentDisplayState(row.State); got != "completed" {
		t.Fatalf("display state = %q", got)
	}
	if got := shortResidentID(row.ID); got != "b6382ac7" {
		t.Fatalf("short ID = %q", got)
	}
	if got := formatResidentUpdatedAt(time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC), time.Date(2026, time.August, 13, 12, 2, 0, 0, time.UTC)); got != "2m ago" {
		t.Fatalf("relative update = %q", got)
	}
}

func TestResidentSubagentsDialogShowsUsageMetadata(t *testing.T) {
	completed := make(chan struct{}, 2)
	manager := subagents.NewResidentManager(t.TempDir(), func(_ subagents.ResidentChildSpec, journal *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		journal.ConfigureUsage(272_000, true)
		return func(context.Context, string) error {
			usage := provider.Usage{InputTokens: 84_000, OutputTokens: 1_500, CacheReadTokens: 123_000, CacheMeasuredPromptTokens: 207_000, CacheMeasuredReadTokens: 123_000, CostUSD: 0.525}
			return journal.RecordAgentEvent(core.EvUsage{Usage: usage, Cumulative: usage})
		}, nil
	})
	manager.SetCompletionObserver(func(subagents.ResidentCompletion) { completed <- struct{}{} })
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	for index := range 2 {
		id := fmt.Sprintf("usage-child-%d", index)
		if _, err := manager.Spawn(context.Background(), subagents.ResidentChildSpec{ID: id, SessionID: "session-" + id, Profile: "reviewer", Provider: "openai-codex", Model: "gpt-test"}, "task"); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		select {
		case <-completed:
		case <-time.After(time.Second):
			t.Fatal("resident child did not complete")
		}
	}
	dialog := newResidentSubagentsDialog()
	dialog.Open(manager)
	plain := strings.Join(plainResidentIndicatorLines(dialog.Render(tui.Dark, 100, 10)), "\n")
	if !strings.Contains(plain, "\n    ↑84k ↓1.5k R123k/ C59.4% $0.525 (sub) 76.1%/272k") {
		t.Fatalf("dialog = %q, want usage metadata line", plain)
	}
	for _, height := range []int{3, 4} {
		if lines := dialog.Render(tui.Dark, 100, height); len(lines) > height {
			t.Fatalf("height %d rendered %d lines: %q", height, len(lines), plainResidentIndicatorLines(lines))
		}
	}
	for width := 1; width <= 3; width++ {
		for _, line := range plainResidentIndicatorLines(dialog.Render(tui.Dark, width, 10)) {
			if line == "    " {
				t.Fatalf("width %d rendered an empty metadata row", width)
			}
		}
	}
}

func TestResidentSubagentsDialogBoundsEmptyStateByHeight(t *testing.T) {
	dialog := newResidentSubagentsDialog()
	dialog.Open(nil)
	tests := []struct {
		height int
		want   []string
	}{
		{height: 0, want: nil},
		{height: 1, want: []string{"  Resident subagents"}},
		{height: 2, want: []string{"  Resident subagents", "  Enter: open   Esc: close   /subagents new <task>: spawn"}},
		{height: 3, want: []string{"  Resident subagents", "  Enter: open   Esc: close   /subagents new <task>: spawn", "  No resident subagents."}},
	}
	for _, test := range tests {
		if got := plainResidentIndicatorLines(dialog.Render(tui.Dark, 100, test.height)); !slices.Equal(got, test.want) {
			t.Errorf("height %d rendered %q, want %q", test.height, got, test.want)
		}
	}
}

func TestResidentResultStatusIncludesFinalSummary(t *testing.T) {
	got := residentResultStatus("child", subagents.ResidentResult{State: subagents.ResidentIdle, Summary: "the child answer"})
	if got != "completed result child: the child answer" {
		t.Fatalf("result status = %q", got)
	}
}

func TestResidentSubagentsDialogRefreshAllowsNilReceiver(t *testing.T) {
	var dialog *residentSubagentsDialog
	dialog.refresh(1)
}
