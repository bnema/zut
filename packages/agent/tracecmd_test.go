package agent

import (
	"bytes"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
)

func TestRenderTraceInspectionUsesFactualOperations(t *testing.T) {
	now := time.Date(2026, 8, 12, 7, 0, 30, 0, time.UTC)
	views := subagents.ProjectTrace([]subagents.TraceEvent{{
		Type: "tool.started", AgentID: "agent-a", TurnID: "turn-1", Timestamp: now.Add(-30 * time.Second),
	}})
	var out bytes.Buffer
	if err := renderTraceInspection(&out, views, now); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "agent-a  tool open 30s\n"; got != want {
		t.Fatalf("inspection = %q, want %q", got, want)
	}
}
