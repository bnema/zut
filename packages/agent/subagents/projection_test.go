package subagents

import (
	"testing"
	"time"
)

func TestProjectTraceReportsOpenOperationInsteadOfLifecycleState(t *testing.T) {
	started := time.Date(2026, 8, 12, 7, 0, 0, 0, time.UTC)
	views := ProjectTrace([]TraceEvent{
		{Seq: 1, Timestamp: started, Type: "turn.started", AgentID: "agent-1", TurnID: "turn-1"},
		{Seq: 2, Timestamp: started.Add(time.Second), Type: "tool.started", AgentID: "agent-1", TurnID: "turn-1"},
	})
	view := views["agent-1"]
	if view.Terminal != "" {
		t.Fatalf("terminal = %q, want empty", view.Terminal)
	}
	if len(view.OpenOperations) != 2 {
		t.Fatalf("open operations = %#v, want turn and tool", view.OpenOperations)
	}
	for _, operation := range view.OpenOperations {
		if !operation.Open() || operation.Duration(started.Add(time.Minute)) <= 0 {
			t.Fatalf("operation = %#v, want open operation with duration", operation)
		}
	}
}

func TestProjectTraceClosesPairedOperationsAndKeepsTerminalFact(t *testing.T) {
	started := time.Date(2026, 8, 12, 7, 0, 0, 0, time.UTC)
	views := ProjectTrace([]TraceEvent{
		{Seq: 1, Timestamp: started, Type: "turn.started", AgentID: "agent-1", TurnID: "turn-1"},
		{Seq: 2, Timestamp: started.Add(time.Second), Type: "turn.finished", AgentID: "agent-1", TurnID: "turn-1"},
		{Seq: 3, Timestamp: started.Add(2 * time.Second), Type: "tool.started", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "one"}},
		{Seq: 4, Timestamp: started.Add(3 * time.Second), Type: "tool.started", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "two"}},
		{Seq: 5, Timestamp: started.Add(4 * time.Second), Type: "tool.finished", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "one"}},
		{Seq: 6, Timestamp: started.Add(5 * time.Second), Type: "agent.finished", AgentID: "agent-1"},
	})
	view := views["agent-1"]
	if len(view.OpenOperations) != 0 {
		t.Fatalf("open operations = %#v, want none after terminal agent event", view.OpenOperations)
	}
	if view.Terminal != "completed" {
		t.Fatalf("terminal = %q, want completed", view.Terminal)
	}
}

func TestProjectTraceClosesFailedToolByCallID(t *testing.T) {
	started := time.Date(2026, 8, 12, 7, 0, 0, 0, time.UTC)
	view := ProjectTrace([]TraceEvent{
		{Timestamp: started, Type: "tool.started", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "one"}},
		{Timestamp: started, Type: "tool.started", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "two"}},
		{Timestamp: started.Add(time.Second), Type: "tool.failed", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "one"}},
	})["agent-1"]
	if len(view.OpenOperations) != 1 || view.OpenOperations[0].CallID != "two" {
		t.Fatalf("open operations = %#v, want only call two", view.OpenOperations)
	}
}

func TestProjectTraceSelectsSpecificActivityAndProjectsResultDelivery(t *testing.T) {
	started := time.Date(2026, 8, 12, 7, 0, 0, 0, time.UTC)
	view := ProjectTrace([]TraceEvent{
		{Timestamp: started, Type: "turn.started", AgentID: "agent-1", TurnID: "turn-1"},
		{Timestamp: started.Add(time.Second), Type: "tool.started", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "one", "name": "bash"}},
		{Timestamp: started.Add(2 * time.Second), Type: "assistant.stream.observed", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"source_event": "message.delta"}},
		{Timestamp: started.Add(3 * time.Second), Type: "result.available", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"ref": "subagent://agent-1/result"}},
		{Timestamp: started.Add(4 * time.Second), Type: "result.delivered", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"ref": "subagent://agent-1/result"}},
	})["agent-1"]
	if view.PrimaryOperation == nil || view.PrimaryOperation.Type != "tool.started" || view.PrimaryOperation.Name != "bash" {
		t.Fatalf("primary operation = %#v, want bash tool", view.PrimaryOperation)
	}
	if view.LastObservation == nil || view.LastObservation.Type != "assistant.stream.observed" || view.LastObservation.Label() != "assistant streaming" {
		t.Fatalf("last observation = %#v", view.LastObservation)
	}
	if view.Result == nil || !view.Result.Available || !view.Result.Delivered || view.Result.Ref != "subagent://agent-1/result" {
		t.Fatalf("result = %#v", view.Result)
	}
	if got, want := view.Summary(), "tool bash open · assistant streaming"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestProjectTraceOmitsPriorTurnObservation(t *testing.T) {
	started := time.Date(2026, 8, 12, 7, 0, 0, 0, time.UTC)
	view := ProjectTrace([]TraceEvent{
		{Timestamp: started, Type: "assistant.stream.observed", AgentID: "agent-1", TurnID: "turn-1"},
		{Timestamp: started.Add(time.Second), Type: "turn.started", AgentID: "agent-1", TurnID: "turn-2"},
	})["agent-1"]
	if view.LastObservation == nil {
		t.Fatal("last observation missing")
	}
	if view.PrimaryOperation == nil {
		t.Fatal("primary operation missing")
	}
	if observation := view.ObservationFor(*view.PrimaryOperation); observation != nil {
		t.Fatalf("prior turn observation = %#v, want omitted", observation)
	}
	if got, want := view.Summary(), "turn open"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestProjectTraceNewAvailableResultResetsDelivery(t *testing.T) {
	view := ProjectTrace([]TraceEvent{
		{Type: "result.delivered", AgentID: "agent-1", Data: map[string]any{"ref": "subagent://agent-1/result-1"}},
		{Type: "result.available", AgentID: "agent-1", Data: map[string]any{"ref": "subagent://agent-1/result-2"}},
	})["agent-1"]
	if view.Result == nil || !view.Result.Available || view.Result.Delivered || view.Result.Failed || view.Result.Ref != "subagent://agent-1/result-2" {
		t.Fatalf("result = %#v", view.Result)
	}
}

func TestProjectTraceFailedAgentClosesOpenOperations(t *testing.T) {
	view := ProjectTrace([]TraceEvent{
		{Type: "tool.started", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "one"}},
		{Type: "agent.failed", AgentID: "agent-1"},
	})["agent-1"]
	if len(view.OpenOperations) != 0 || view.Terminal != "failed" {
		t.Fatalf("view = %#v, want failed terminal with no open operations", view)
	}
}
