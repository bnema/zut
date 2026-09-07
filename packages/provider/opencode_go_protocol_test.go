package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestAnthropicMessagesURLKeepsVersionedBasePath(t *testing.T) {
	server := httptest.NewServer(nil)
	defer server.Close()

	for _, test := range []struct {
		name string
		base string
		want string
	}{
		{name: "host", base: server.URL, want: server.URL + "/v1/messages"},
		{name: "v1", base: server.URL + "/v1", want: server.URL + "/v1/messages"},
		{name: "v2", base: server.URL + "/v2", want: server.URL + "/v2/messages"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := anthropicMessagesURL(test.base); got != test.want {
				t.Fatalf("anthropicMessagesURL(%q) = %q, want %q", test.base, got, test.want)
			}
		})
	}
}

func TestAnthropicCompatUsesScopedModelMetadata(t *testing.T) {
	const modelID = "scoped-anthropic-model"
	client := NewAnthropicCompat(ProviderOpenCodeGo, "key", "https://example.test/v1").(*anthropicClient)
	client.SetModelMetadata(Model{
		Provider:      ProviderOpenCodeGo,
		ID:            modelID,
		Reasoning:     true,
		MaxOutput:     4096,
		ContextWindow: 32768,
		ReasoningLevelMap: map[string]string{
			"high": "high",
		},
	})

	wire, err := client.buildRequest(Request{
		Model:     modelID,
		Reasoning: "high",
		Messages:  []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hello"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if wire.MaxTokens != 4096 || wire.Thinking == nil || wire.Thinking.Type != "enabled" {
		t.Fatalf("wire = %+v, want scoped output limit and reasoning", wire)
	}
}

func TestOpenCodeGoAdaptersUsePublishedProtocolsAndIdentity(t *testing.T) {
	const apiKey = "synthetic-opencode-key"
	const (
		responsesModel = "muse-spark-1.3-contributor"
		messagesModel  = "minimax-m2"
		qwenModel      = "qwen3-coder"
		chatModel      = "kimi-k2"
		glmModel       = "glm-5"
	)

	type requestRecord struct {
		path    string
		method  string
		headers http.Header
		body    map[string]any
	}
	var (
		mu      sync.Mutex
		calls   = make(map[string]int)
		records []requestRecord
	)
	expectedPathByModel := map[string]string{
		responsesModel: "/v1/responses",
		messagesModel:  "/v1/messages",
		qwenModel:      "/v1/messages",
		chatModel:      "/v1/chat/completions",
		glmModel:       "/v1/chat/completions",
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api.json":
			_, _ = io.WriteString(w, `{
				"opencode-go": {
					"npm": "@ai-sdk/openai-compatible",
					"models": {
						"muse-spark-1.3-contributor": {
							"name": "Muse Spark 1.3 Contributor",
							"reasoning": true,
							"reasoning_options": [{"type":"effort","values":["low","medium","high"]}],
							"limit": {"context":128000,"output":8192},
							"provider": {"npm":"@ai-sdk/openai"}
						},
						"minimax-m2": {
							"name": "MiniMax M2",
							"reasoning": true,
							"reasoning_options": [{"type":"effort","values":["low","medium","high"]}],
							"limit": {"context":128000,"output":8192},
							"provider": {"npm":"@ai-sdk/anthropic"}
						},
						"qwen3-coder": {
							"name": "Qwen 3 Coder",
							"reasoning": true,
							"reasoning_options": [{"type":"effort","values":["low","medium","high"]}],
							"limit": {"context":128000,"output":8192},
							"provider": {"npm":"@ai-sdk/anthropic"}
						},
						"kimi-k2": {
							"name": "Kimi K2",
							"reasoning": false
						},
						"glm-5": {
							"name": "GLM 5",
							"reasoning": false
						}
					}
				}
			}`)
			return
		case "/v1/models":
			if got := r.Header.Get("authorization"); got != "Bearer "+apiKey {
				t.Errorf("models authorization = %q, want bearer key", got)
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"muse-spark-1.3-contributor"},{"id":"minimax-m2"},{"id":"qwen3-coder"},{"id":"kimi-k2"},{"id":"glm-5"}]}`)
			return
		case "/v1/responses", "/v1/messages", "/v1/chat/completions":
		default:
			http.NotFound(w, r)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read %s request: %v", r.URL.Path, err)
			return
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Errorf("decode %s request: %v", r.URL.Path, err)
			return
		}
		model, _ := decoded["model"].(string)
		if expectedPathByModel[model] != r.URL.Path {
			http.Error(w, "model routed to the wrong protocol", http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/v1/responses" {
			if decoded["input"] == nil || decoded["messages"] != nil {
				http.Error(w, "Responses body has the wrong shape", http.StatusBadRequest)
				return
			}
		} else if decoded["messages"] == nil || decoded["input"] != nil {
			http.Error(w, "non-Responses body has the wrong shape", http.StatusBadRequest)
			return
		}
		mu.Lock()
		callKey := r.URL.Path + "\x00" + model
		calls[callKey]++
		call := calls[callKey]
		records = append(records, requestRecord{path: r.URL.Path, method: r.Method, headers: r.Header.Clone(), body: decoded})
		mu.Unlock()

		if r.Header.Get("x-opencode-client") != "zut" || r.Header.Get("user-agent") != "zut" {
			t.Errorf("%s identity headers = client %q user-agent %q", r.URL.Path, r.Header.Get("x-opencode-client"), r.Header.Get("user-agent"))
		}
		if r.Header.Get("x-opencode-session") == "" || r.Header.Get("x-opencode-request") == "" {
			t.Errorf("%s missing request identity: %v", r.URL.Path, r.Header)
		}
		switch r.URL.Path {
		case "/v1/chat/completions", "/v1/responses":
			if got := r.Header.Get("authorization"); got != "Bearer "+apiKey {
				t.Errorf("%s authorization = %q, want bearer key", r.URL.Path, got)
			}
			if got := r.Header.Get("x-api-key"); got != "" {
				t.Errorf("%s leaked x-api-key %q", r.URL.Path, got)
			}
		case "/v1/messages":
			if got := r.Header.Get("x-api-key"); got != apiKey {
				t.Errorf("messages x-api-key = %q, want %q", got, apiKey)
			}
			if got := r.Header.Get("authorization"); got != "" {
				t.Errorf("messages leaked authorization %q", got)
			}
		}
		if call == 1 {
			writeToolResponse(w, r.URL.Path)
		} else {
			writeTextResponse(w, r.URL.Path)
		}
	}))
	defer server.Close()

	models, err := discoverOpenCodeGo(context.Background(), apiKey, server.URL+"/v1", server.URL+"/api.json")
	if err != nil {
		t.Fatal(err)
	}
	client := NewOpenCodeGoWithModels(apiKey, server.URL+"/v1", models)

	for _, test := range []struct {
		name      string
		model     string
		thread    string
		firstWant string
	}{
		{name: "responses", model: responsesModel, thread: "thread-responses", firstWant: "/v1/responses"},
		{name: "messages minimax", model: messagesModel, thread: "thread-messages-minimax", firstWant: "/v1/messages"},
		{name: "messages qwen", model: qwenModel, thread: "thread-messages-qwen", firstWant: "/v1/messages"},
		{name: "chat completions kimi", model: chatModel, thread: "thread-chat-kimi", firstWant: "/v1/chat/completions"},
		{name: "chat completions glm", model: glmModel, thread: "thread-chat-glm", firstWant: "/v1/chat/completions"},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := Request{
				Model:     test.model,
				Context:   RequestContext{ThreadID: test.thread, CacheSessionID: "cache-" + test.name, TurnID: "turn-1"},
				Tools:     []Tool{{Name: "lookup", Schema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)}},
				Reasoning: "high",
				Messages:  []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "find x"}}}},
			}
			assistant, err := consumeToolStream(client, first)
			if err != nil {
				t.Fatal(err)
			}
			second := first
			second.Context.TurnID = "turn-2"
			second.Messages = append(second.Messages,
				Message{Role: RoleAssistant, Content: []Content{assistant}},
				Message{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: assistant.ID, Content: []Content{TextBlock{Text: "result"}}}}},
			)
			if _, err := consumeTextStream(client, second); err != nil {
				t.Fatal(err)
			}

			mu.Lock()
			var got []requestRecord
			for _, record := range records {
				if record.path == test.firstWant && record.body["model"] == test.model {
					got = append(got, record)
				}
			}
			mu.Unlock()
			if len(got) != 2 {
				t.Fatalf("%s requests = %d, want initial and continuation", test.firstWant, len(got))
			}
			if got[0].method != http.MethodPost || got[1].method != http.MethodPost {
				t.Fatalf("methods = %q, %q", got[0].method, got[1].method)
			}
			if got[0].headers.Get("x-opencode-session") != test.thread || got[1].headers.Get("x-opencode-session") != test.thread {
				t.Fatalf("session headers = %q, %q", got[0].headers.Get("x-opencode-session"), got[1].headers.Get("x-opencode-session"))
			}
			if got[0].headers.Get("x-opencode-request") != "turn-1" || got[1].headers.Get("x-opencode-request") != "turn-2" {
				t.Fatalf("request headers = %q, %q", got[0].headers.Get("x-opencode-request"), got[1].headers.Get("x-opencode-request"))
			}
			assertOpenCodeGoBodyShape(t, test.firstWant, got[0].body, false)
			assertOpenCodeGoBodyShape(t, test.firstWant, got[1].body, true)
		})
	}
}

func writeToolResponse(w http.ResponseWriter, path string) {
	w.Header().Set("content-type", "text/event-stream")
	switch path {
	case "/v1/chat/completions":
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	case "/v1/responses":
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call-1\",\"name\":\"lookup\"}}\n\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"q\\\":\\\"x\\\"}\"}\n\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	case "/v1/messages":
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-1\",\"name\":\"lookup\",\"input\":{}}}\n\nevent: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\\\"x\\\"}\"}}\n\nevent: content_block_stop\ndata: {\"index\":0}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":1}}\n\n\nevent: message_stop\ndata: {}\n\n")
	}
}

func writeTextResponse(w http.ResponseWriter, path string) {
	w.Header().Set("content-type", "text/event-stream")
	switch path {
	case "/v1/chat/completions":
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	case "/v1/responses":
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	case "/v1/messages":
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"index\":0}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {}\n\n")
	}
}

func consumeToolStream(client Client, req Request) (ToolCallBlock, error) {
	stream, err := client.Stream(context.Background(), req)
	if err != nil {
		return ToolCallBlock{}, err
	}
	var call ToolCallBlock
	for event := range stream {
		if done, ok := event.(EventDone); ok {
			if done.Err != nil {
				return ToolCallBlock{}, done.Err
			}
			for _, content := range done.Message.Content {
				if candidate, ok := content.(ToolCallBlock); ok {
					call = candidate
				}
			}
		}
	}
	if call.ID == "" {
		return ToolCallBlock{}, errors.New("stream did not produce a tool call")
	}
	return call, nil
}

func consumeTextStream(client Client, req Request) (string, error) {
	stream, err := client.Stream(context.Background(), req)
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for event := range stream {
		switch event := event.(type) {
		case EventTextDelta:
			text.WriteString(event.Delta)
		case EventDone:
			if event.Err != nil {
				return "", event.Err
			}
		}
	}
	return text.String(), nil
}

func assertOpenCodeGoBodyShape(t *testing.T, path string, body map[string]any, continuation bool) {
	t.Helper()
	if body["model"] == nil || body["stream"] != true {
		t.Fatalf("%s body = %#v, want model and stream", path, body)
	}
	switch path {
	case "/v1/responses":
		if body["input"] == nil || body["messages"] != nil {
			t.Fatalf("Responses body = %#v, want input without messages", body)
		}
		if reasoning, ok := body["reasoning"].(map[string]any); !ok || reasoning["effort"] != "high" {
			t.Fatalf("Responses reasoning = %#v, want effort=high", body["reasoning"])
		}
		if continuation && !containsResponseItemType(body["input"], "function_call_output") {
			t.Fatalf("Responses continuation = %#v, want function_call_output", body["input"])
		}
	case "/v1/messages":
		if body["messages"] == nil || body["input"] != nil {
			t.Fatalf("Messages body = %#v, want messages without input", body)
		}
		if thinking, ok := body["thinking"].(map[string]any); !ok || thinking["type"] != "enabled" {
			t.Fatalf("Messages thinking = %#v, want enabled thinking", body["thinking"])
		}
		if continuation && !containsAnthropicBlockType(body["messages"], "tool_result") {
			t.Fatalf("Messages continuation = %#v, want tool_result", body["messages"])
		}
	case "/v1/chat/completions":
		if body["messages"] == nil || body["input"] != nil {
			t.Fatalf("Chat Completions body = %#v, want messages without input", body)
		}
		if continuation && !containsChatRole(body["messages"], "tool") {
			t.Fatalf("Chat continuation = %#v, want tool message", body["messages"])
		}
	}
}

func containsResponseItemType(value any, want string) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if object, ok := item.(map[string]any); ok && object["type"] == want {
			return true
		}
	}
	return false
}

func containsAnthropicBlockType(value any, want string) bool {
	messages, ok := value.([]any)
	if !ok {
		return false
	}
	for _, message := range messages {
		object, ok := message.(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := object["content"].([]any)
		if !ok {
			continue
		}
		for _, block := range blocks {
			if object, ok := block.(map[string]any); ok && object["type"] == want {
				return true
			}
		}
	}
	return false
}

func containsChatRole(value any, want string) bool {
	messages, ok := value.([]any)
	if !ok {
		return false
	}
	for _, message := range messages {
		if object, ok := message.(map[string]any); ok && object["role"] == want {
			return true
		}
	}
	return false
}
