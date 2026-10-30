package daemonproto_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"reflect"
	"strings"
	"testing"
	"time"
)

func eventCases() []core.AgentEvent {
	content := []provider.Content{provider.TextBlock{Text: "hi", ThoughtSignature: "sig", Phase: "commentary"}, provider.ImageBlock{MimeType: "image/png", Data: []byte{1, 2}}, provider.ReasoningBlock{}, provider.ReasoningBlock{ID: "r", Encrypted: "opaque"}, provider.ToolCallBlock{ID: "c", Name: "read", Arguments: json.RawMessage(`{"x":1}`), Origin: "host"}, provider.ToolResultBlock{CallID: "c", Content: []provider.Content{provider.TextBlock{Text: "nested", ThoughtSignature: "nested-sig"}}}}
	msg := provider.Message{Role: provider.RoleAssistant, Content: content, Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Meta: map[string]string{"k": "v"}, AddedToolNames: []string{"read"}}
	return []core.AgentEvent{
		core.EvTurnStart{Step: 2, TurnID: "turn"}, core.EvUserMessage{Message: msg}, core.EvAssistantStart{}, core.EvRequestStarted{Provider: "p", Model: "m", Scope: core.RetryScopeAgent, Attempt: 2, MaxAttempts: 3}, core.EvCacheDiagnostics{Eligible: true, Mode: "m", Transport: "t", Continuation: "c"}, core.EvRetryScheduled{Scope: core.RetryScopeProvider, Attempt: 1, MaxAttempts: 2, Delay: 123456789 * time.Nanosecond}, core.EvTextDelta{Delta: "text"}, core.EvToolCall{ID: "c", Name: "read", Args: json.RawMessage(`{"x":1}`)}, core.EvToolExecutionStarted{ID: "c", Name: "read"}, core.EvToolUseStart{ID: "c", Name: "read"}, core.EvToolUseArgs{ID: "c", Delta: `{"x"`}, core.EvToolUseEnd{ID: "c"}, core.EvToolProgress{ID: "c", Text: "working"}, core.EvPlanUpdate{CallID: "c", Update: core.PlanUpdate{Plan: []core.PlanStep{{Step: "do", Status: core.PlanInProgress}}}}, core.EvToolResult{ID: "c", Result: core.ToolResult{Content: content, IsError: true, ActivateTools: []string{"read"}, Details: map[string]any{"detail": "value"}, Timing: &provider.ToolTiming{Duration: time.Second}}}, core.EvUsage{Usage: provider.Usage{InputTokens: 5, ReasoningTokensKnown: true, ReasoningTokens: 2}, Cumulative: provider.Usage{OutputTokens: 8}}, core.EvAssistantMessage{Message: msg}, core.EvTurnEnd{Stop: provider.StopReason("end"), Err: errors.New("synthetic")}, core.EvTurnRecovery{Reason: "missing_answer", Attempt: 1}, core.EvRepetitionGuard{Count: 3, Elapsed: 123456789 * time.Nanosecond}, core.EvDone{}, core.EvError{Err: errors.New("synthetic")}, core.EvTurnEnd{}, core.EvError{},
	}
}
func TestAgentEventRoundTripAndProjection(t *testing.T) {
	for _, ev := range eventCases() {
		t.Run(ev.Type(), func(t *testing.T) {
			kind, raw, err := daemonproto.EncodeAgentEvent(ev)
			if err != nil {
				t.Fatal(err)
			}
			got, err := daemonproto.DecodeAgentEvent(kind, raw)
			if err != nil {
				t.Fatal(err)
			}
			// Errors cross the boundary as messages, never concrete error objects.
			a, _ := json.Marshal(ev)
			b, _ := json.Marshal(got)
			if string(a) != string(b) {
				t.Fatalf("round trip\n%s\n%s", a, b)
			}
			if x, ok := ev.(core.EvTurnEnd); ok {
				y := got.(core.EvTurnEnd)
				if (x.Err == nil) != (y.Err == nil) || x.Err != nil && x.Err.Error() != y.Err.Error() {
					t.Fatal("turn error lost")
				}
			}
			if x, ok := ev.(core.EvError); ok {
				y := got.(core.EvError)
				if (x.Err == nil) != (y.Err == nil) || x.Err != nil && x.Err.Error() != y.Err.Error() {
					t.Fatal("error lost")
				}
			}
			var wire map[string]json.RawMessage
			if err = json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			for k, v := range modes.EventToJSON(ev) {
				expected, _ := json.Marshal(v)
				var x, y any
				_ = json.Unmarshal(expected, &x)
				_ = json.Unmarshal(wire[k], &y)
				if !reflect.DeepEqual(x, y) {
					t.Fatalf("projection field %s: %s != %s", k, wire[k], expected)
				}
			}
			// Re-encoding also checks concrete content hydration, host-only origin and metadata.
			_, again, err := daemonproto.EncodeAgentEvent(got)
			if err != nil || string(raw) != string(again) {
				t.Fatalf("unstable typed round trip: %s vs %s (%v)", raw, again, err)
			}
		})
	}
}
func TestSessionEventRoundTrip(t *testing.T) {
	payloads := []struct {
		k session.EventKind
		v any
	}{
		{session.EvInfo, session.InfoEvent{}},
		{session.EvQueue, session.QueueEvent{}},
		{session.EvPending, session.PendingEvent{}},
		{session.EvPendingResolved, session.PendingResolvedEvent{}},
		{session.EvDeferred, session.DeferredEvent{}},
		{session.EvNote, session.NoteEvent{}},
		{session.EvExtChrome, session.ExtChromeEvent{}},
		{session.EvExtPanel, session.ExtPanelEvent{}},
		{session.EvExtAlert, session.ExtAlertEvent{}},
		{session.EvExtDisplay, session.ExtDisplayEvent{}},
		{session.EvExtInsert, session.ExtInsertEvent{}},
		{session.EvPlan, session.PlanEvent{}},
		{session.EvGoal, session.GoalEvent{}},
		{session.EvTitle, session.TitleEvent{}},
		{session.EvCompaction, session.CompactionEvent{}},
		{session.EvMessagesReplaced, session.MessagesReplacedEvent{}},
		{session.EvInterSession, session.InterSessionEvent{}},
		{session.EvAttach, session.AttachEvent{}},
		{session.EvClosed, session.ClosedEvent{}},
	}
	for _, p := range payloads {
		t.Run(string(p.k), func(t *testing.T) {
			e := session.Event{Session: "s", Seq: 42, Kind: p.k, Data: p.v}
			n, err := daemonproto.EncodeEvent(e)
			if err != nil {
				t.Fatal(err)
			}
			got, err := daemonproto.DecodeEvent(n)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(e, got) {
				t.Fatalf("%#v != %#v", e, got)
			}
		})
	}
}
func TestCodecRejectsUnknown(t *testing.T) {
	if _, err := daemonproto.DecodeAgentEvent("unknown", json.RawMessage(`{}`)); err == nil {
		t.Fatal("accepted unknown agent event")
	}
	if _, err := daemonproto.DecodeEvent(daemonproto.EventNotification{Kind: "unknown", Data: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("accepted unknown session event")
	}
}

func TestSnapshotResultCodec(t *testing.T) {
	want := session.Snapshot{Messages: []provider.Message{{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "c", Timing: &provider.ToolTiming{Duration: 1234567}, Content: []provider.Content{provider.ImageBlock{MimeType: "image/png", Data: []byte{1, 2}}, provider.ReasoningBlock{}}}}}}, Live: session.LiveTurn{Tools: []session.LiveTool{{Result: &core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "live"}}}}}}, Plan: map[string]core.PlanUpdate{"t": {Plan: []core.PlanStep{{Step: "next", Status: core.PlanPending}}}}}
	c, _ := servePair(t, daemonproto.Options{OnRequest: func(ctx context.Context, r daemonproto.Request) (any, error) { return want, nil }})
	var got session.Snapshot
	if err := c.Call(context.Background(), daemonproto.SnapshotRequest{Type: "snapshot", Session: "s"}, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("snapshot mismatch\n%#v\n%#v", want, got)
	}
}

func TestSnapshotResultCodecStreamingToolArgs(t *testing.T) {
	for name, args := range map[string]string{"empty": ``, "partial": `{"command":"ls`} {
		t.Run(name, func(t *testing.T) {
			raw := session.Snapshot{Live: session.LiveTurn{Tools: []session.LiveTool{{ID: "x", Name: "bash", State: "composing", Args: []byte(args)}}}}
			c, _ := servePair(t, daemonproto.Options{OnRequest: func(ctx context.Context, r daemonproto.Request) (any, error) { return raw, nil }})
			var got session.Snapshot
			err := c.Call(context.Background(), daemonproto.SnapshotRequest{Type: "snapshot", Session: "s"}, &got)
			if err == nil || !strings.Contains(err.Error(), "encode result data: ") {
				t.Fatalf("want diagnosable encode error, got %v", err)
			}
		})
	}
}

func TestToolResultDetailsExactLargeInteger(t *testing.T) {
	const number = "9007199254740993"
	ev := core.EvToolResult{ID: "call", Result: core.ToolResult{Details: map[string]any{"large": json.Number(number), "nested": []any{json.Number(number)}}}}
	kind, raw, err := daemonproto.EncodeAgentEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	got, err := daemonproto.DecodeAgentEvent(kind, raw)
	if err != nil {
		t.Fatal(err)
	}
	details := got.(core.EvToolResult).Result.Details.(map[string]any)
	if n, ok := details["large"].(json.Number); !ok || n.String() != number {
		t.Fatalf("integer rounded: %#v", details["large"])
	}
	if n, ok := details["nested"].([]any)[0].(json.Number); !ok || n.String() != number {
		t.Fatalf("nested integer rounded: %#v", details["nested"])
	}
	_, again, err := daemonproto.EncodeAgentEvent(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(again) {
		t.Fatalf("round trip changed number:\n%s\n%s", raw, again)
	}
}

func TestAgentCancellationIdentity(t *testing.T) {
	for _, ev := range []core.AgentEvent{core.EvTurnEnd{Stop: provider.StopError, Err: context.Canceled}, core.EvError{Err: context.Canceled}} {
		kind, raw, err := daemonproto.EncodeAgentEvent(ev)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := daemonproto.DecodeAgentEvent(kind, raw)
		if err != nil {
			t.Fatal(err)
		}
		var got error
		switch e := decoded.(type) {
		case core.EvTurnEnd:
			got = e.Err
		case core.EvError:
			got = e.Err
		}
		if !errors.Is(got, context.Canceled) || got.Error() != context.Canceled.Error() {
			t.Fatalf("decoded error = %v", got)
		}
	}
	// Old payloads remain valid and retain their text without inventing identity.
	ev, err := daemonproto.DecodeAgentEvent("turn_end", json.RawMessage(`{"type":"turn_end","value":{"Stop":"error","Error":"old error"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := ev.(core.EvTurnEnd).Err; got.Error() != "old error" || errors.Is(got, context.Canceled) {
		t.Fatalf("legacy error = %v", got)
	}
}
