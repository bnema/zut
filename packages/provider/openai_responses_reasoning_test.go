package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func terminalPayload(event, id, usage, output string) string {
	response := `"id":"` + id + `"`
	if usage != "" {
		response += `,` + usage
	}
	if output != "" {
		response += `,"output":` + output
	}
	return `{"type":"` + event + `","response":{` + response + `}}`
}

func reasoningTerminalItem(id, encrypted, summary string) string {
	var sb strings.Builder
	sb.WriteString(`{"type":"reasoning"`)
	if id != "" {
		enc, _ := json.Marshal(id)
		sb.WriteString(`,"id":` + string(enc))
	}
	if encrypted != "" {
		enc, _ := json.Marshal(encrypted)
		sb.WriteString(`,"encrypted_content":` + string(enc))
	}
	sb.WriteString(`,"summary":`)
	if summary == "" {
		sb.WriteString(`[]`)
	} else {
		text, _ := json.Marshal(summary)
		sb.WriteString(`[{"type":"summary_text","text":` + string(text) + `}]`)
	}
	sb.WriteString(`}`)
	return sb.String()
}

func findReasoningBlocks(msg Message) []ReasoningBlock {
	var out []ReasoningBlock
	for _, c := range msg.Content {
		if rb, ok := c.(ReasoningBlock); ok {
			out = append(out, rb)
		}
	}
	return out
}

func TestResponsesReasoningTerminalBackfillCompleted(t *testing.T) {
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_test_a"}}`,
		terminalPayload("response.completed", "resp-1", `"usage":{"input_tokens":1,"output_tokens":1}`, `[`+reasoningTerminalItem("rs_test_a", "blob-terminal", "")+`]`),
	)
	blocks := findReasoningBlocks(done.Message)
	if len(blocks) != 1 {
		t.Fatalf("reasoning blocks = %#v, want 1", done.Message.Content)
	}
	if blocks[0].ID != "rs_test_a" || blocks[0].Encrypted != "blob-terminal" {
		t.Fatalf("reasoning = %+v, want id rs_test_a with terminal payload", blocks[0])
	}
}

func TestResponsesReasoningTerminalBackfillDone(t *testing.T) {
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_test_a"}}`,
		terminalPayload("response.done", "resp-1", `"usage":{"input_tokens":1,"output_tokens":1}`, `[`+reasoningTerminalItem("rs_test_a", "blob-terminal", "")+`]`),
	)
	blocks := findReasoningBlocks(done.Message)
	if len(blocks) != 1 {
		t.Fatalf("reasoning blocks = %#v, want 1", done.Message.Content)
	}
	if blocks[0].ID != "rs_test_a" || blocks[0].Encrypted != "blob-terminal" {
		t.Fatalf("reasoning = %+v, want id rs_test_a with terminal payload", blocks[0])
	}
}

func TestResponsesReasoningEarlierPayloadWins(t *testing.T) {
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_test_a","encrypted_content":"early"}}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_test_a","encrypted_content":"done-late","summary":[{"type":"summary_text","text":"done summary"}]}}`,
		terminalPayload("response.completed", "resp-1", `"usage":{"input_tokens":1,"output_tokens":1}`, `[`+reasoningTerminalItem("rs_test_a", "late", "terminal summary")+`]`),
	)
	blocks := findReasoningBlocks(done.Message)
	if len(blocks) != 1 || blocks[0].Encrypted != "early" {
		t.Fatalf("reasoning = %+v, want early payload to win", done.Message.Content)
	}
	if blocks[0].Summary != "done summary" {
		t.Fatalf("summary = %q, want done summary to fill missing added summary", blocks[0].Summary)
	}
}

func TestResponsesReasoningDoneDoesNotDuplicateStreamedSummary(t *testing.T) {
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_test_a"}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"hello"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_test_a","encrypted_content":"blob-done","summary":[{"type":"summary_text","text":"hello"}]}}`,
		terminalPayload("response.completed", "resp-1", `"usage":{"input_tokens":1,"output_tokens":1}`, `[`+reasoningTerminalItem("rs_test_a", "blob-terminal", "hello")+`]`),
	)
	blocks := findReasoningBlocks(done.Message)
	if len(blocks) != 1 {
		t.Fatalf("reasoning blocks = %#v, want 1", done.Message.Content)
	}
	if blocks[0].Summary != "hello" {
		t.Fatalf("summary = %q, want single hello without duplication", blocks[0].Summary)
	}
	if blocks[0].Encrypted != "blob-done" {
		t.Fatalf("encrypted = %q, want done payload to fill missing added payload", blocks[0].Encrypted)
	}
}

func TestResponsesReasoningTerminalIDMismatch(t *testing.T) {
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_test_a"}}`,
		terminalPayload("response.completed", "resp-1", `"usage":{"input_tokens":1,"output_tokens":1}`, `[`+reasoningTerminalItem("rs_other", "blob-other", "")+`]`),
	)
	blocks := findReasoningBlocks(done.Message)
	if len(blocks) != 1 {
		t.Fatalf("reasoning blocks = %#v, want 1", done.Message.Content)
	}
	if blocks[0].Encrypted != "" {
		t.Fatalf("mismatched terminal id transferred payload: %+v", blocks[0])
	}
}

func TestResponsesReasoningMultipleItemsPreserveIdentity(t *testing.T) {
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_a"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"reasoning","id":"rs_b"}}`,
		terminalPayload("response.completed", "resp-1", `"usage":{"input_tokens":1,"output_tokens":1}`,
			`[`+reasoningTerminalItem("rs_b", "blob-b", "")+`,`+reasoningTerminalItem("rs_a", "blob-a", "")+`]`),
	)
	blocks := findReasoningBlocks(done.Message)
	if len(blocks) != 2 {
		t.Fatalf("reasoning blocks = %#v, want 2", done.Message.Content)
	}
	if blocks[0].ID != "rs_a" || blocks[0].Encrypted != "blob-a" {
		t.Fatalf("first reasoning = %+v, want rs_a/blob-a", blocks[0])
	}
	if blocks[1].ID != "rs_b" || blocks[1].Encrypted != "blob-b" {
		t.Fatalf("second reasoning = %+v, want rs_b/blob-b", blocks[1])
	}
}

func TestResponsesReasoningMissingOutputKeepsIDOnly(t *testing.T) {
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_test_a"}}`,
		terminalPayload("response.completed", "resp-1", `"usage":{"input_tokens":1,"output_tokens":1}`, ``),
	)
	blocks := findReasoningBlocks(done.Message)
	if len(blocks) != 1 || blocks[0].ID != "rs_test_a" {
		t.Fatalf("reasoning blocks = %#v, want ID-only block", done.Message.Content)
	}
}

func TestResponsesReasoningNoDuplicateSummary(t *testing.T) {
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_test_a"}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"hello"}`,
		terminalPayload("response.completed", "resp-1", `"usage":{"input_tokens":1,"output_tokens":1}`, `[`+reasoningTerminalItem("rs_test_a", "blob-terminal", "hello")+`]`),
	)
	blocks := findReasoningBlocks(done.Message)
	if len(blocks) != 1 {
		t.Fatalf("reasoning blocks = %#v, want 1", done.Message.Content)
	}
	if blocks[0].Summary != "hello" {
		t.Fatalf("summary = %q, want single hello without duplication", blocks[0].Summary)
	}
	if blocks[0].Encrypted != "blob-terminal" {
		t.Fatalf("encrypted = %q, want terminal payload", blocks[0].Encrypted)
	}
}

func TestResponsesReasoningCompletionCallbackSeesBackfill(t *testing.T) {
	c := newOpenAICodexClient("token", "acct", "")
	raw := make(chan sseEvent, 4)
	raw <- sseEvent{Data: `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_test_a"}}`}
	raw <- sseEvent{Data: terminalPayload("response.completed", "resp-9", `"usage":{"input_tokens":1,"output_tokens":1}`, `[`+reasoningTerminalItem("rs_test_a", "blob-terminal", "")+`]`)}
	close(raw)
	out := make(chan Event, 16)
	var callbackMsg Message
	var callbackID string
	go c.runResponseEventsWithFirst(context.Background(), Request{Model: "gpt-5.2"}, out, raw, nil, func(id string, msg Message) {
		callbackID = id
		callbackMsg = msg
	})
	for ev := range out {
		if _, ok := ev.(EventDone); ok {
			break
		}
	}
	if callbackID != "resp-9" {
		t.Fatalf("callback id = %q, want resp-9", callbackID)
	}
	blocks := findReasoningBlocks(callbackMsg)
	if len(blocks) != 1 || blocks[0].Encrypted != "blob-terminal" {
		t.Fatalf("callback reasoning = %#v, want backfilled payload", callbackMsg.Content)
	}
}

func TestResponsesReasoningBackfillThenReplay(t *testing.T) {
	done, _ := runResponsesCompletionFixture(t,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_test_a"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"message"}}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":"hi"}`,
		terminalPayload("response.completed", "resp-1", `"usage":{"input_tokens":1,"output_tokens":1}`, `[`+reasoningTerminalItem("rs_test_a", "blob-terminal", "")+`]`),
	)
	c := newOpenAICodexClient("token", "acct", "")
	wire, err := c.buildRequest(Request{Model: "gpt-5.2", Messages: []Message{done.Message}})
	if err != nil {
		t.Fatal(err)
	}
	var sawReasoning bool
	for _, item := range wire.Input {
		if reasoning, ok := item.(codexReasoningItem); ok {
			sawReasoning = true
			if reasoning.EncryptedContent != "blob-terminal" || reasoning.ID != "rs_test_a" {
				t.Fatalf("replayed reasoning = %+v", reasoning)
			}
		}
	}
	if !sawReasoning {
		t.Fatalf("terminal payload did not reach next request: %#v", wire.Input)
	}
}

func TestResponsesReasoningRequestBuilderOmitsNonReplayable(t *testing.T) {
	c := newOpenAICodexClient("token", "acct", "")
	original := []Message{
		{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}},
		{Role: RoleAssistant, Content: []Content{
			ReasoningBlock{ID: "rs_id_only"},
			ReasoningBlock{Summary: "summary only"},
			ReasoningBlock{},
			ReasoningBlock{ID: "rs_full", Summary: "s", Encrypted: "blob-full"},
			ReasoningBlock{Encrypted: "blob-no-id"},
			TextBlock{Text: "hello"},
			ToolCallBlock{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)},
		}},
		{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "call-1", Content: []Content{TextBlock{Text: "ok"}}}}},
	}
	before, _ := json.Marshal(original)
	wire, err := c.buildRequest(Request{Model: "gpt-5.2", Messages: original})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(original)
	if string(before) != string(after) {
		t.Fatalf("buildRequest mutated input messages")
	}
	var reasoningItems []codexReasoningItem
	var sawText, sawCall, sawResult bool
	for _, item := range wire.Input {
		switch v := item.(type) {
		case codexReasoningItem:
			reasoningItems = append(reasoningItems, v)
		case codexOutputMessage:
			for _, part := range v.Content {
				if part.Text == "hello" {
					sawText = true
				}
			}
		case codexFunctionCall:
			if v.Name == "lookup" {
				sawCall = true
			}
		case codexFunctionCallOutput:
			sawResult = true
		}
	}
	if len(reasoningItems) != 2 {
		t.Fatalf("reasoning items = %#v, want only 2 encrypted blocks", reasoningItems)
	}
	if reasoningItems[0].ID != "rs_full" || reasoningItems[0].EncryptedContent != "blob-full" {
		t.Fatalf("first reasoning = %+v", reasoningItems[0])
	}
	if len(reasoningItems[0].Summary) != 1 || reasoningItems[0].Summary[0].Text != "s" {
		t.Fatalf("first summary = %+v", reasoningItems[0].Summary)
	}
	if reasoningItems[1].EncryptedContent != "blob-no-id" {
		t.Fatalf("second reasoning = %+v", reasoningItems[1])
	}
	if reasoningItems[1].Summary == nil || len(reasoningItems[1].Summary) != 0 {
		t.Fatalf("empty summary must encode as empty array, got %+v", reasoningItems[1].Summary)
	}
	if !sawText || !sawCall || !sawResult {
		t.Fatalf("text/call/result missing: text=%v call=%v result=%v input=%#v", sawText, sawCall, sawResult, wire.Input)
	}
	encoded, _ := json.Marshal(wire.Input)
	if strings.Contains(string(encoded), "rs_id_only") || strings.Contains(string(encoded), "summary only") {
		t.Fatalf("non-replayable reasoning leaked into input: %s", encoded)
	}
}

func TestNamedResponsesErrorIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
	}))
	defer server.Close()

	client := NewOpenAIResponsesNamed("synthetic-key", server.URL, "custom-responses")
	_, err := client.Stream(context.Background(), Request{
		Model:    "gpt-5",
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("want HTTP 400 error, got nil")
	}
	if !strings.Contains(err.Error(), "custom-responses") {
		t.Fatalf("error = %q, want provider identity custom-responses", err.Error())
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("error = %q, want retained HTTP status", err.Error())
	}
	if strings.Contains(err.Error(), "openai-codex") {
		t.Fatalf("error = %q, must not identify as openai-codex", err.Error())
	}
}
