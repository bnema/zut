package subagents

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func primaryOperation(t testing.TB, view AgentTraceView) *Operation {
	t.Helper()
	operation, ok := view.Primary()
	if !ok {
		t.Fatal("trace view has no primary operation")
	}
	return &operation
}

func TestProjectTraceRetainsJSONDecodedRequestAttempts(t *testing.T) {
	bundle := t.TempDir()
	if err := os.WriteFile(filepath.Join(bundle, "manifest.json"), []byte(`{"version":1,"trace_file":"trace.jsonl"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	line := `{"seq":1,"timestamp":"2026-08-12T07:00:00Z","type":"provider.request.started","agent_id":"agent-1","turn_id":"turn-1","data":{"attempt":2,"max_attempts":6}}` + "\n"
	if err := os.WriteFile(filepath.Join(bundle, "trace.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := ReadTrace(bundle)
	if err != nil {
		t.Fatal(err)
	}
	operation := primaryOperation(t, ProjectTrace(events)["agent-1"])
	if operation == nil || operation.Attempt != 2 || operation.MaxAttempts != 6 {
		t.Fatalf("projected operation = %#v, want attempt 2/6", operation)
	}
}

func TestProjectTraceIgnoresProtocolFactsAndShowsCompletedToolDuringNextProviderRequest(t *testing.T) {
	started := time.Date(2026, 8, 12, 7, 0, 0, 0, time.UTC)
	view := ProjectTrace([]TraceEvent{
		{Timestamp: started, Type: "turn.started", AgentID: "agent-1", TurnID: "turn-1"},
		{Timestamp: started.Add(time.Second), Type: "tool.started", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "call-1", "name": "read"}},
		{Timestamp: started.Add(2 * time.Second), Type: "tool.finished", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "call-1", "name": "read"}},
		{Timestamp: started.Add(2500 * time.Millisecond), Type: "worker.protocol.observed", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"source_event": "tool.use.args"}},
		{Timestamp: started.Add(3 * time.Second), Type: "provider.request.started", AgentID: "agent-1", TurnID: "turn-1"},
	})["agent-1"]
	if primaryOperation(t, view) == nil || primaryOperation(t, view).Type != "provider.request.started" {
		t.Fatalf("primary operation = %#v", primaryOperation(t, view))
	}
	observation := view.ObservationFor(*primaryOperation(t, view))
	if observation == nil || observation.Label() != "finished read" {
		t.Fatalf("observation = %#v, want finished read", observation)
	}
	if view.LastObservation != observation {
		t.Fatalf("protocol fact replaced user-facing observation: %#v", view.LastObservation)
	}
}

func TestProjectTraceSuppressesGenericUnmatchedToolFinished(t *testing.T) {
	view := ProjectTrace([]TraceEvent{
		{Type: "turn.started", AgentID: "agent-1", TurnID: "turn-1"},
		{Type: "tool.finished", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "unknown"}},
	})["agent-1"]
	if view.LastObservation != nil {
		t.Fatalf("generic tool-finished observation = %#v, want omitted", view.LastObservation)
	}
}

func TestProjectTraceKeepsShortCompletedToolVisibleBeforeNextRequest(t *testing.T) {
	started := time.Date(2026, 8, 12, 7, 0, 0, 0, time.UTC)
	view := ProjectTrace([]TraceEvent{
		{Timestamp: started, Type: "turn.started", AgentID: "agent-1", TurnID: "turn-1"},
		{Timestamp: started.Add(time.Second), Type: "tool.started", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "call-1", "name": "read"}},
		{Timestamp: started.Add(time.Second + time.Millisecond), Type: "tool.finished", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "call-1", "name": "read"}},
	})["agent-1"]
	if primaryOperation(t, view) == nil || primaryOperation(t, view).Type != "turn.started" {
		t.Fatalf("primary operation = %#v, want turn", primaryOperation(t, view))
	}
	if observation := view.ObservationFor(*primaryOperation(t, view)); observation == nil || observation.Label() != "finished read" {
		t.Fatalf("completed tool observation = %#v, want finished read", observation)
	}
}

func TestProjectTraceDoesNotDecorateToolWithProviderStream(t *testing.T) {
	started := time.Date(2026, 8, 12, 7, 0, 0, 0, time.UTC)
	view := ProjectTrace([]TraceEvent{
		{Timestamp: started, Type: "turn.started", AgentID: "agent-1", TurnID: "turn-1"},
		{Timestamp: started.Add(time.Second), Type: "tool.started", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "call-1", "name": "read"}},
		{Timestamp: started.Add(2 * time.Second), Type: "assistant.stream.observed", AgentID: "agent-1", TurnID: "turn-1"},
	})["agent-1"]
	if primaryOperation(t, view) == nil || primaryOperation(t, view).Type != "tool.started" {
		t.Fatalf("primary operation = %#v, want tool", primaryOperation(t, view))
	}
	if observation := view.ObservationFor(*primaryOperation(t, view)); observation != nil {
		t.Fatalf("provider stream decorated tool operation: %#v", observation)
	}
}

func TestAgentTraceViewTurnStartedAtMatchesOwningTurn(t *testing.T) {
	first := time.Date(2026, 8, 12, 7, 0, 0, 0, time.UTC)
	second := first.Add(time.Minute)
	view := AgentTraceView{OpenOperations: []Operation{
		{Type: "turn.started", TurnID: "turn-1", StartedAt: first},
		{Type: "turn.started", TurnID: "turn-2", StartedAt: second},
	}}
	operation := Operation{Type: "provider.request.started", TurnID: "turn-2", StartedAt: second.Add(time.Second)}
	if got := view.TurnStartedAt(operation); !got.Equal(second) {
		t.Fatalf("turn start = %v, want %v", got, second)
	}
}

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
	if primaryOperation(t, view) == nil || primaryOperation(t, view).Type != "tool.started" || primaryOperation(t, view).Name != "bash" {
		t.Fatalf("primary operation = %#v, want bash tool", primaryOperation(t, view))
	}
	if view.LastObservation == nil || view.LastObservation.Type != "assistant.stream.observed" || view.LastObservation.Label() != "assistant streaming" {
		t.Fatalf("last observation = %#v", view.LastObservation)
	}
	if view.Result == nil || !view.Result.Available || !view.Result.Delivered || view.Result.Ref != "subagent://agent-1/result" {
		t.Fatalf("result = %#v", view.Result)
	}
	if got, want := view.Summary(), "running bash"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestOperationLabelUsesHumanFacingActivity(t *testing.T) {
	cases := []struct {
		operation Operation
		want      string
	}{
		{operation: Operation{Type: "provider.request.started"}, want: "waiting for model response"},
		{operation: Operation{Type: "tool.started", Name: "bash"}, want: "running bash"},
		{operation: Operation{Type: "turn.started"}, want: "processing turn"},
	}
	for _, tc := range cases {
		if got := tc.operation.Label(); got != tc.want {
			t.Fatalf("Label(%q) = %q, want %q", tc.operation.Type, got, tc.want)
		}
	}
}

func TestProjectTraceRetainsSafeProviderAttemptDiagnostic(t *testing.T) {
	started := time.Date(2026, 8, 12, 7, 0, 0, 0, time.UTC)
	view := ProjectTrace([]TraceEvent{
		{Timestamp: started, Type: "provider.request.started", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"provider": "openai-codex", "model": "gpt-5.6", "attempt": 2, "max_attempts": 4}},
		{Timestamp: started.Add(time.Second), Type: "provider.request.failed", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"error_code": "deadline_exceeded"}},
	})["agent-1"]
	if len(view.OpenOperations) != 0 {
		t.Fatalf("open operations = %#v, want none", view.OpenOperations)
	}
	if got, want := view.LastRequest, (&RequestFact{TurnID: "turn-1", Provider: "openai-codex", Model: "gpt-5.6", Attempt: 2, MaxAttempts: 4, Outcome: "failed", ErrorCode: "deadline_exceeded", StartedAt: started, EndedAt: started.Add(time.Second)}); !reflect.DeepEqual(got, want) {
		t.Fatalf("request diagnostic = %#v, want %#v", got, want)
	}
}

func TestProjectTraceOmitsObservationForAnotherToolInSameTurn(t *testing.T) {
	started := time.Date(2026, 8, 12, 7, 0, 0, 0, time.UTC)
	view := ProjectTrace([]TraceEvent{
		{Timestamp: started, Type: "turn.started", AgentID: "agent-1", TurnID: "turn-1"},
		{Timestamp: started.Add(time.Second), Type: "tool.started", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "one", "name": "bash"}},
		{Timestamp: started.Add(2 * time.Second), Type: "tool.started", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "two", "name": "read"}},
		{Timestamp: started.Add(3 * time.Second), Type: "tool.output.observed", AgentID: "agent-1", TurnID: "turn-1", Data: map[string]any{"call_id": "two", "name": "read"}},
	})["agent-1"]
	if primaryOperation(t, view) == nil || primaryOperation(t, view).CallID != "one" {
		t.Fatalf("primary operation = %#v, want call one", primaryOperation(t, view))
	}
	if observation := view.ObservationFor(*primaryOperation(t, view)); observation != nil {
		t.Fatalf("another tool's observation = %#v, want omitted", observation)
	}
	if got, want := view.Summary(), "running bash"; got != want {
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
	if primaryOperation(t, view) == nil {
		t.Fatal("primary operation missing")
	}
	if observation := view.ObservationFor(*primaryOperation(t, view)); observation != nil {
		t.Fatalf("prior turn observation = %#v, want omitted", observation)
	}
	if got, want := view.Summary(), "processing turn"; got != want {
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

func TestProjectTraceDeliveryFailureClearsDelivered(t *testing.T) {
	view := ProjectTrace([]TraceEvent{
		{Type: "result.delivered", AgentID: "agent-1", Data: map[string]any{"ref": "subagent://agent-1/result"}},
		{Type: "result.delivery.failed", AgentID: "agent-1", Data: map[string]any{"ref": "subagent://agent-1/result"}},
	})["agent-1"]
	if view.Result == nil || !view.Result.Available || view.Result.Delivered || !view.Result.Failed {
		t.Fatalf("result = %#v, want available undelivered failure", view.Result)
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
