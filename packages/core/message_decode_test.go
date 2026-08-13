package core

import (
	"encoding/json"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestDecodeMessageJSONRestoresToolContent(t *testing.T) {
	raw, err := json.Marshal(provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{
		provider.TextBlock{Text: "checking"}, provider.ToolCallBlock{ID: "call-1", Name: "bash", Arguments: json.RawMessage(`{"command":"pwd"}`)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	message, err := DecodeMessageJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(message.Content) != 2 {
		t.Fatalf("message = %#v", message)
	}
	call, ok := message.Content[1].(provider.ToolCallBlock)
	if !ok || call.ID != "call-1" || string(call.Arguments) != `{"command":"pwd"}` {
		t.Fatalf("call = %#v", message.Content[1])
	}
}
