package provider

import (
	"encoding/json"
	"testing"
)

func TestReplayContentPreservesHistoryAndRestoresOrigin(t *testing.T) {
	original := Message{Role: RoleAssistant, Meta: map[string]string{"provider": "google", "model": "model-a"}, Content: []Content{
		ReasoningBlock{Encrypted: "opaque"}, TextBlock{Text: "answer", ThoughtSignature: "text-signature"}, ImageBlock{ThoughtSignature: "image-signature"}, ToolCallBlock{ID: "call_1", Name: "lookup", ThoughtSignature: "tool-signature"},
	}}
	before, _ := json.Marshal(original)
	foreign := replayContent(original, "google", "model-b")
	if len(foreign) != 3 || foreign[0].(TextBlock).ThoughtSignature != "" || foreign[1].(ImageBlock).ThoughtSignature != "" || foreign[2].(ToolCallBlock).ThoughtSignature != "" {
		t.Fatalf("foreign content = %#v", foreign)
	}
	restored := replayContent(original, "google", "model-a")
	if len(restored) != 4 || restored[0].(ReasoningBlock).Encrypted != "opaque" || restored[1].(TextBlock).ThoughtSignature != "text-signature" {
		t.Fatal("switch back lost private state")
	}
	after, _ := json.Marshal(original)
	if string(before) != string(after) {
		t.Fatal("history mutated")
	}
}

func TestResponsesContinuationAllowsHostOrigin(t *testing.T) {
	prior := Request{Model: "model-a", Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hello"}}}}}
	output := Message{Role: RoleAssistant, Content: []Content{TextBlock{Text: "answer"}}}
	session := responsesWebSocketSession{lastRequest: prior, lastOutput: output, lastResponseID: "resp_1", lastBaseline: "baseline"}
	next := prior
	next.Messages = append(append([]Message(nil), prior.Messages...), WithMessageOrigin(output, "openai-codex", "model-a"), Message{Role: RoleUser, Content: []Content{TextBlock{Text: "next"}}})
	incremental, id := session.incrementalRequest(next, "cache", func(Request, string) (string, error) { return "baseline", nil })
	if id != "resp_1" || len(incremental.Messages) != 1 {
		t.Fatalf("continuation = %q, %d messages", id, len(incremental.Messages))
	}
}

func TestResponsesReplayAcrossModels(t *testing.T) {
	c := newOpenAICodexClient("token", "account", "")
	for _, tt := range []struct {
		name, origin, model, id string
		want                    bool
	}{
		{"same model", "openai-codex", "gpt-5.2", "rs_native", true},
		{"other provider", "opencode", "gpt-5.2", "rs_first:rs_second", false},
		{"foreign valid ID", "opencode", "gpt-5.2", "rs_native", false},
		{"other model", "openai-codex", "gpt-5", "rs_native", false},
		{"unknown compound ID", "", "", "rs_first:rs_second", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			messages := []Message{{Role: RoleAssistant, Meta: map[string]string{"provider": tt.origin, "model": tt.model}, Content: []Content{
				ReasoningBlock{ID: tt.id, Encrypted: "opaque"}, TextBlock{Text: "answer"},
				ToolCallBlock{ID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{}`)},
			}}, {Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "call_1", Content: []Content{TextBlock{Text: "result"}}}}}}
			before, _ := json.Marshal(messages)
			wire, err := c.buildRequest(Request{Model: "gpt-5.2", Messages: messages})
			if err != nil {
				t.Fatal(err)
			}
			reasoning, text, call, result := 0, 0, 0, 0
			for _, item := range wire.Input {
				switch item.(type) {
				case codexReasoningItem:
					reasoning++
				case codexOutputMessage:
					text++
				case codexFunctionCall:
					call++
				case codexFunctionCallOutput:
					result++
				}
			}
			if (reasoning == 1) != tt.want || text != 1 || call != 1 || result != 1 {
				t.Fatalf("reasoning/text/call/result = %d/%d/%d/%d", reasoning, text, call, result)
			}
			after, _ := json.Marshal(messages)
			if string(before) != string(after) {
				t.Fatal("history mutated")
			}
		})
	}
}
