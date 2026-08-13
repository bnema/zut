package modes

import (
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
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
