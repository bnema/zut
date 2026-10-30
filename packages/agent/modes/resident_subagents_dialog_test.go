package modes

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
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
	usage := provider.Usage{InputTokens: 84_000, OutputTokens: 1_500, CacheReadTokens: 123_000, CacheMeasuredPromptTokens: 207_000, CacheMeasuredReadTokens: 123_000, CostUSD: 0.525}
	rows := make([]subagents.ResidentSnapshot, 2)
	for index := range rows {
		rows[index] = subagents.ResidentSnapshot{ID: fmt.Sprintf("usage-child-%d", index), State: subagents.ResidentIdle, Profile: "reviewer", Provider: "openai-codex", Model: "gpt-test", Usage: usage, ContextUsed: 207_000, ContextMax: 272_000, Subscription: true}
	}
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().ResidentList(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, offset, limit int) ([]subagents.ResidentSnapshot, int, error) {
		end := min(len(rows), offset+limit)
		if offset >= end {
			return nil, len(rows), nil
		}
		return rows[offset:end], len(rows), nil
	})
	dialog := newResidentSubagentsDialog()
	dialog.Open(port)
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

func TestResidentSubagentsDialogRefreshAllowsNilReceiver(t *testing.T) {
	var dialog *residentSubagentsDialog
	dialog.refresh(1)
}

func TestResidentSubagentsDialogListsFromPortAndSelects(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	rows := []subagents.ResidentSnapshot{{ID: "first", State: subagents.ResidentRunning, Model: "m"}, {ID: "second", State: subagents.ResidentIdle, Model: "m"}}
	port.EXPECT().ResidentList(mock.Anything, 0, 12).Return(rows, 2, nil).Once()
	dialog := newResidentSubagentsDialog()
	dialog.Open(port)
	dialog.HandleKey(tui.Key{Kind: tui.KeyDown})
	if got := dialog.HandleKey(tui.Key{Kind: tui.KeyEnter}); got != "second" {
		t.Fatalf("selected = %q, want second", got)
	}
}

func TestResidentSubagentsDialogShowsPortError(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().ResidentList(mock.Anything, mock.Anything, mock.Anything).Return(nil, 0, errors.New("daemon gone"))
	dialog := newResidentSubagentsDialog()
	dialog.Open(port)
	plain := strings.Join(plainResidentIndicatorLines(dialog.Render(tui.Dark, 100, 10)), "\n")
	if !strings.Contains(plain, "unavailable: daemon gone") || strings.Contains(plain, "No resident subagents") {
		t.Fatalf("dialog = %q, want port error", plain)
	}
}

func TestResidentSubagentsDialogCachesWindowBetweenRenders(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().ResidentList(mock.Anything, 0, 3).Return([]subagents.ResidentSnapshot{{ID: "a"}}, 1, nil).Once()
	dialog := newResidentSubagentsDialog()
	dialog.active, dialog.port = true, port
	for range 3 {
		dialog.Render(tui.Dark, 100, 9)
	}
}
