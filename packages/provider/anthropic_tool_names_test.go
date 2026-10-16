package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnthropicMCPResourceToolNames(t *testing.T) {
	for _, helper := range []struct{ local, wire string }{
		{"mcp_list_resources", "ListMcpResourcesTool"},
		{"mcp_read_resource", "ReadMcpResourceTool"},
	} {
		for _, oauth := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/oauth=%t", helper.local, oauth), func(t *testing.T) {
				wireName := helper.local
				if oauth {
					wireName = helper.wire
				}
				tools := []Tool{
					{Name: helper.local, Schema: json.RawMessage(`{"type":"object","properties":{}}`)},
					{Name: "mcp__example__lookup", Schema: json.RawMessage(`{"type":"object","properties":{}}`)},
				}
				req := Request{
					Model: "claude-sonnet-5-5",
					Tools: tools,
					Messages: []Message{
						{Role: RoleUser, Content: []Content{TextBlock{Text: "List resources."}}},
						{Role: RoleAssistant, Content: []Content{ToolCallBlock{ID: "prior-call", Name: helper.local, Arguments: json.RawMessage(`{}`)}}},
						{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "prior-call", Content: []Content{TextBlock{Text: "No resources."}}}}},
					},
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						Tools    []anthTool `json:"tools"`
						Messages []struct {
							Content []struct {
								Type string `json:"type"`
								Name string `json:"name"`
							} `json:"content"`
						} `json:"messages"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if len(body.Tools) != 2 || body.Tools[0].Name != wireName || body.Tools[1].Name != "mcp__example__lookup" {
						t.Errorf("wire tools = %+v, want %q and unchanged server tool", body.Tools, wireName)
					}
					foundCall := false
					for _, message := range body.Messages {
						for _, block := range message.Content {
							if block.Type == "tool_use" {
								foundCall = true
								if block.Name != wireName {
									t.Errorf("replayed tool name = %q, want %q", block.Name, wireName)
								}
							}
						}
					}
					if !foundCall {
						t.Error("request lost replayed tool call")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"next-call\",\"name\":%q,\"input\":{}}}\n\n", wireName)
					fmt.Fprint(w, "event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n")
					fmt.Fprint(w, "event: content_block_stop\ndata: {\"index\":0}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"}}\n\nevent: message_stop\ndata: {}\n\n")
				}))
				defer srv.Close()
				client := NewAnthropic("synthetic-key", srv.URL)
				if oauth {
					client = NewAnthropicOAuth("synthetic-token", srv.URL)
				}
				events, err := client.Stream(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				started, completed := false, false
				for event := range events {
					switch e := event.(type) {
					case EventToolStart:
						started = true
						if e.Name != helper.local {
							t.Errorf("streamed tool name = %q, want %q", e.Name, helper.local)
						}
					case EventDone:
						completed = true
						if e.Err != nil || e.Stop != StopToolUse || len(e.Message.Content) != 1 {
							t.Errorf("unexpected completion: %+v", e)
							continue
						}
						call, ok := e.Message.Content[0].(ToolCallBlock)
						if !ok || call.Name != helper.local || call.ID != "next-call" || string(call.Arguments) != "{}" {
							t.Errorf("completed content = %+v, want local helper call", e.Message.Content)
						}
					}
				}
				if !started || !completed {
					t.Fatalf("missing events: started=%t completed=%t", started, completed)
				}
				if tools[0].Name != helper.local {
					t.Error("request mutated caller's tool definition")
				}
			})
		}
	}
}
