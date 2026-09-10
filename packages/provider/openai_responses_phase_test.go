package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// phaseTexts returns (text, phase) pairs for assistant text blocks.
func phaseTexts(t *testing.T, done EventDone) [][2]string {
	t.Helper()
	var out [][2]string
	for _, c := range done.Message.Content {
		if tb, ok := c.(TextBlock); ok {
			out = append(out, [2]string{tb.Text, tb.Phase})
		}
	}
	return out
}

func TestResponsesPhaseCommentaryThenFinal(t *testing.T) {
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","phase":"commentary"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"working on it"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","phase":"final_answer"}}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":"done"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"message"}}`,
		completedPayload("response.completed", `"id":"resp-1","usage":{"input_tokens":1,"output_tokens":1}`),
	)
	got := phaseTexts(t, done)
	want := [][2]string{{"working on it", "commentary"}, {"done", "final_answer"}}
	if len(got) != len(want) {
		t.Fatalf("blocks = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("blocks = %#v, want %#v", got, want)
		}
	}
}

func TestResponsesPhaseOnItemCompletion(t *testing.T) {
	// Phase first appears on item-done rather than item-added.
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"late phase"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","phase":"commentary"}}`,
		completedPayload("response.completed", `"id":"resp-1","usage":{"input_tokens":1,"output_tokens":1}`),
	)
	got := phaseTexts(t, done)
	if len(got) != 1 || got[0] != [2]string{"late phase", "commentary"} {
		t.Fatalf("blocks = %#v, want [{late phase commentary}]", got)
	}
}

func TestResponsesPhaseAddedNotErasedByDone(t *testing.T) {
	// Phase observed on item-added must survive a phaseless item-done.
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","phase":"commentary"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"keep me"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message"}}`,
		completedPayload("response.completed", `"id":"resp-1","usage":{"input_tokens":1,"output_tokens":1}`),
	)
	got := phaseTexts(t, done)
	if len(got) != 1 || got[0] != [2]string{"keep me", "commentary"} {
		t.Fatalf("blocks = %#v, want [{keep me commentary}]", got)
	}
}

func TestResponsesPhaseTerminalReconciliation(t *testing.T) {
	// Phase only present in the terminal response.output array.
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"terminal phase"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message"}}`,
		`{"type":"response.completed","response":{"id":"resp-1","usage":{"input_tokens":1,"output_tokens":1},"output":[{"type":"message","content":[{"type":"output_text","text":"terminal phase","phase":"final_answer"}]}]}}`,
	)
	got := phaseTexts(t, done)
	if len(got) != 1 || got[0] != [2]string{"terminal phase", "final_answer"} {
		t.Fatalf("blocks = %#v, want [{terminal phase final_answer}]", got)
	}
}

func TestResponsesPhaseAbsentAndUnknown(t *testing.T) {
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"plain"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","phase":"mystery"}}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":"weird"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"message"}}`,
		completedPayload("response.completed", `"id":"resp-1","usage":{"input_tokens":1,"output_tokens":1}`),
	)
	if done.Stop != StopEnd {
		t.Fatalf("stop = %q, want %q", done.Stop, StopEnd)
	}
	got := phaseTexts(t, done)
	want := [][2]string{{"plain", ""}, {"weird", "mystery"}}
	if len(got) != len(want) {
		t.Fatalf("blocks = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("blocks = %#v, want %#v", got, want)
		}
	}
	// Unknown phases are stored but not interpreted as recognized.
	for _, c := range done.Message.Content {
		if tb, ok := c.(TextBlock); ok && tb.Phase == "mystery" && IsRecognizedTextPhase(tb.Phase) {
			t.Fatalf("unknown phase treated as recognized: %q", tb.Phase)
		}
	}
}

func TestResponsesPhaseToolInterleaving(t *testing.T) {
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","phase":"commentary"}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"checking"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"message"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call-1","name":"echo"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call"}}`,
		completedPayload("response.completed", `"id":"resp-1","usage":{"input_tokens":1,"output_tokens":1}`),
	)
	if done.Stop != StopToolUse {
		t.Fatalf("stop = %q, want %q", done.Stop, StopToolUse)
	}
	if len(done.Message.Content) != 2 {
		t.Fatalf("content = %#v, want 2 blocks", done.Message.Content)
	}
	tb, ok := done.Message.Content[0].(TextBlock)
	if !ok || tb.Text != "checking" || tb.Phase != "commentary" {
		t.Fatalf("first block = %#v, want commentary text", done.Message.Content[0])
	}
	if tc, ok := done.Message.Content[1].(ToolCallBlock); !ok || tc.ID != "call-1" {
		t.Fatalf("second block = %#v, want tool call", done.Message.Content[1])
	}
}

func TestResponsesPhaseRequestReplay(t *testing.T) {
	c := newOpenAICodexClient("token", "acct", "")
	req := Request{
		Model: "gpt-5.2",
		Messages: []Message{
			{Role: RoleAssistant, Content: []Content{
				TextBlock{Text: "progress", Phase: "commentary"},
				TextBlock{Text: "result", Phase: "final_answer"},
				TextBlock{Text: "plain"},
				TextBlock{Text: "odd", Phase: "mystery"},
			}},
		},
	}
	wire, err := c.buildRequest(req)
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Input []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type  string `json:"type"`
				Text  string `json:"text"`
				Phase string `json:"phase"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var phases []string
	for _, item := range decoded.Input {
		if item.Role != "assistant" {
			continue
		}
		for _, part := range item.Content {
			phases = append(phases, part.Text+":"+part.Phase)
		}
	}
	want := []string{"progress:commentary", "result:final_answer", "plain:", "odd:"}
	if len(phases) != len(want) {
		t.Fatalf("replay = %v, want %v (raw %s)", phases, want, raw)
	}
	for i := range want {
		if phases[i] != want[i] {
			t.Fatalf("replay = %v, want %v (raw %s)", phases, want, raw)
		}
	}
	if strings.Contains(string(raw), "mystery") {
		t.Fatalf("unrecognized phase leaked to wire: %s", raw)
	}
}

func TestResponsesPhaseWebSocketSharedStateMachine(t *testing.T) {
	// The WebSocket transport feeds the same runResponseEvents parser, so
	// exercise it through the shared entry point used by the socket path.
	c := newOpenAICodexClient("token", "acct", "")
	first := sseEvent{Data: `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","phase":"commentary"}}`}
	raw := make(chan sseEvent, 4)
	raw <- sseEvent{Data: `{"type":"response.output_text.delta","output_index":0,"delta":"ws text"}`}
	raw <- sseEvent{Data: `{"type":"response.output_item.done","output_index":0,"item":{"type":"message"}}`}
	raw <- sseEvent{Data: completedPayload("response.completed", `"id":"resp-1","usage":{"input_tokens":1,"output_tokens":1}`)}
	close(raw)
	out := make(chan Event, 32)
	go c.runResponseEventsWithFirst(context.Background(), Request{Model: "gpt-5.2"}, out, raw, &first, nil)
	for ev := range out {
		if done, ok := ev.(EventDone); ok {
			got := phaseTexts(t, done)
			if len(got) != 1 || got[0] != [2]string{"ws text", "commentary"} {
				t.Fatalf("blocks = %#v, want [{ws text commentary}]", got)
			}
			return
		}
	}
	t.Fatal("stream ended without EventDone")
}
