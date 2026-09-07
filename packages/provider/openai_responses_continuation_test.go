package provider

import (
	"context"
	"strings"
	"testing"
)

// runResponsesCompletionFixture feeds scripted Responses SSE payloads
// through the shared completion parser and returns the terminal event
// plus the reported usage.
func runResponsesCompletionFixture(t *testing.T, payloads ...string) (EventDone, Usage) {
	t.Helper()
	c := newOpenAICodexClient("token", "acct", "")
	raw := make(chan sseEvent, len(payloads)+1)
	for _, payload := range payloads {
		raw <- sseEvent{Data: payload}
	}
	close(raw)
	out := make(chan Event, 32)
	go c.runResponseEventsWithFirst(context.Background(), Request{Model: "gpt-5.2"}, out, raw, nil, nil)
	var usage Usage
	for ev := range out {
		switch e := ev.(type) {
		case EventUsage:
			usage = e.Usage
		case EventDone:
			return e, usage
		}
	}
	t.Fatal("parser stream ended without EventDone")
	return EventDone{}, Usage{}
}

func completedPayload(event, response string) string {
	return `{"type":"` + event + `","response":{` + response + `}}`
}

func TestResponsesContinuation(t *testing.T) {
	textItem := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"part one"}`,
	}
	usage := `"id":"resp-1","usage":{"input_tokens":3,"output_tokens":4}`

	for _, tt := range []struct {
		name    string
		event   string
		endTurn string // raw end_turn field, empty when absent
		want    StopReason
	}{
		{name: "explicit false continues", event: "response.completed", endTurn: `"end_turn":false`, want: StopContinue},
		{name: "done alias continues", event: "response.done", endTurn: `"end_turn":false`, want: StopContinue},
		{name: "explicit true ends", event: "response.completed", endTurn: `"end_turn":true`, want: StopEnd},
		{name: "absent ends", event: "response.completed", endTurn: ``, want: StopEnd},
		{name: "null ends", event: "response.completed", endTurn: `"end_turn":null`, want: StopEnd},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := usage
			if tt.endTurn != "" {
				response += "," + tt.endTurn
			}
			payloads := append(append([]string{}, textItem...), completedPayload(tt.event, response))
			done, gotUsage := runResponsesCompletionFixture(t, payloads...)
			if done.Stop != tt.want {
				t.Fatalf("stop = %q, want %q", done.Stop, tt.want)
			}
			if done.Err != nil {
				t.Fatalf("err = %v, want nil", done.Err)
			}
			if gotUsage.InputTokens != 3 || gotUsage.OutputTokens != 4 {
				t.Fatalf("usage = %+v, want input 3 output 4", gotUsage)
			}
			var text strings.Builder
			for _, c := range done.Message.Content {
				if tb, ok := c.(TextBlock); ok {
					text.WriteString(tb.Text)
				}
			}
			if text.String() != "part one" {
				t.Fatalf("text = %q, want %q", text.String(), "part one")
			}
		})
	}
}

func TestResponsesContinuationReasoningOnly(t *testing.T) {
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"blob"}}`,
		completedPayload("response.completed", `"id":"resp-1","end_turn":false,"usage":{"input_tokens":1,"output_tokens":1}`),
	)
	if done.Stop != StopContinue {
		t.Fatalf("stop = %q, want %q", done.Stop, StopContinue)
	}
	var sawReasoning bool
	for _, c := range done.Message.Content {
		if rb, ok := c.(ReasoningBlock); ok && rb.ID == "rs_1" && rb.Encrypted == "blob" {
			sawReasoning = true
		}
	}
	if !sawReasoning {
		t.Fatalf("reasoning not preserved: %#v", done.Message.Content)
	}
}

func TestResponsesContinuationToolCallPrecedence(t *testing.T) {
	toolItem := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call-1","name":"echo"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call"}}`,
	}
	for _, endTurn := range []string{`"end_turn":false`, `"end_turn":true`, ``} {
		response := `"id":"resp-1","usage":{"input_tokens":1,"output_tokens":1}`
		if endTurn != "" {
			response += "," + endTurn
		}
		payloads := append(append([]string{}, toolItem...),
			completedPayload("response.completed", response))
		done, _ := runResponsesCompletionFixture(t, payloads...)
		if done.Stop != StopToolUse {
			t.Fatalf("end_turn %s with tool call: stop = %q, want %q", endTurn, done.Stop, StopToolUse)
		}
		var sawCall bool
		for _, c := range done.Message.Content {
			if tc, ok := c.(ToolCallBlock); ok && tc.ID == "call-1" && tc.Name == "echo" {
				sawCall = true
			}
		}
		if !sawCall {
			t.Fatalf("tool call not preserved: %#v", done.Message.Content)
		}
	}
}

func TestResponsesContinuationPreservesErrorPaths(t *testing.T) {
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.failed","response":{"error":{"message":"boom"}}}`,
	)
	if done.Stop != StopError || done.Err == nil {
		t.Fatalf("failed response: stop = %q err = %v, want error", done.Stop, done.Err)
	}
}
