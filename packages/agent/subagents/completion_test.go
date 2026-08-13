package subagents

import (
	"strings"
	"testing"
)

func TestFormatCompletionUpdateIncludesFinalSummary(t *testing.T) {
	got := FormatCompletionUpdate([]Completion{{AgentID: "child", Status: "completed", Task: "review", Summary: "found the regression"}}, "")
	if !strings.Contains(got, "final: found the regression") {
		t.Fatalf("completion update = %q", got)
	}
}
