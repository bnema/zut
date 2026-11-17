package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

// TestServerAdvisorPauseTurnResumes drives the real Anthropic client: a
// pause_turn response holding a dangling server_tool_use makes core re-send
// with that assistant message replayed verbatim and the advisor tool kept.
func TestServerAdvisorPauseTurnResumes(t *testing.T) {
	const useRaw = `{"type":"server_tool_use","id":"srvtoolu_test1","name":"advisor","input":{}}`
	var (
		mu     sync.Mutex
		bodies []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, body)
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			fmt.Fprint(w, "event: content_block_start\ndata: {\"index\":0,\"content_block\":"+useRaw+"}\n\n"+
				"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\"}}\n\n"+
				"event: content_block_stop\ndata: {\"index\":0}\n\n"+
				"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"pause_turn\"}}\n\nevent: message_stop\ndata: {}\n\n")
			return
		}
		fmt.Fprint(w, "event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"advisor_tool_result\",\"tool_use_id\":\"srvtoolu_test1\",\"content\":{\"type\":\"advisor_redacted_result\",\"encrypted_content\":\"ZW5jcnlwdGVk\"}}}\n\n"+
			"event: content_block_stop\ndata: {\"index\":0}\n\n"+
			"event: content_block_start\ndata: {\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"event: content_block_delta\ndata: {\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\n"+
			"event: content_block_stop\ndata: {\"index\":1}\n\n"+
			"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {}\n\n")
	}))
	defer srv.Close()

	a := NewAgent(provider.NewAnthropic("synthetic-key", srv.URL), "claude-sonnet-5-5", "system", Registry{})
	a.MaxRetries = 0
	a.SetServerAdvisor(&provider.NativeAdvisor{Model: "claude-opus-5-5"})
	if err := a.Prompt(context.Background(), "hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2", len(bodies))
	}
	second := bodies[1]
	tools, _ := second["tools"].([]any)
	if len(tools) == 0 || tools[0].(map[string]any)["type"] != "advisor_20260301" {
		t.Fatalf("advisor tool missing on resume: %v", second["tools"])
	}
	msgs := second["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "assistant" {
		t.Fatalf("resume does not end with the paused assistant message: %v", last)
	}
	content := last["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("paused assistant content = %v", content)
	}
	var want any
	_ = json.Unmarshal([]byte(useRaw), &want)
	if !reflect.DeepEqual(content[0], want) {
		t.Fatalf("server_tool_use not replayed verbatim (cache_control injected?): %v", content[0])
	}
	final := a.Messages()
	if got := final[len(final)-1]; got.Role != provider.RoleAssistant || len(got.Content) != 2 {
		t.Fatalf("final assistant message = %+v", got)
	}
}
