package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testAdvisorUseRaw      = `{"type":"server_tool_use","id":"srvtoolu_test1","name":"advisor","input":{}}`
	testAdvisorRedactedRaw = `{"type":"advisor_tool_result","tool_use_id":"srvtoolu_test1","content":{"type":"advisor_redacted_result","encrypted_content":"ZW5jcnlwdGVk","stop_reason":"end_turn"}}`
)

func testAdvisorUse(id string) ServerToolBlock {
	return ServerToolBlock{Provider: "anthropic", Kind: ServerToolKindUse, ID: id, Name: "advisor",
		Raw: json.RawMessage(strings.ReplaceAll(testAdvisorUseRaw, "srvtoolu_test1", id))}
}

func testAdvisorResult(id string) ServerToolBlock {
	return ServerToolBlock{Provider: "anthropic", Kind: ServerToolKindAdvisorResult, ID: id, Name: "advisor", Redacted: true,
		Raw: json.RawMessage(strings.ReplaceAll(testAdvisorRedactedRaw, "srvtoolu_test1", id))}
}

type capturedAnthRequest struct {
	beta string
	body map[string]any
}

// serveAnthSSE records the request and replies with the given SSE body.
func serveAnthSSE(t *testing.T, sse string, got *capturedAnthRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.beta = r.Header.Get("anthropic-beta")
		if err := json.Unmarshal(b, &got.body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const anthEndTurnSSE = "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {}\n\n"

func drainAnth(t *testing.T, client Client, req Request) ([]Event, EventDone) {
	t.Helper()
	ch, err := client.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	var done EventDone
	for ev := range ch {
		events = append(events, ev)
		if d, ok := ev.(EventDone); ok {
			done = d
		}
	}
	return events, done
}

func wireMessages(body map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range body["messages"].([]any) {
		out = append(out, m.(map[string]any))
	}
	return out
}

func wireBlockTypes(m map[string]any) []string {
	var out []string
	for _, b := range m["content"].([]any) {
		out = append(out, b.(map[string]any)["type"].(string))
	}
	return out
}

func TestAnthropicAdvisorRequestToolAndBeta(t *testing.T) {
	for _, oauth := range []bool{false, true} {
		t.Run(fmt.Sprintf("oauth=%t", oauth), func(t *testing.T) {
			var got capturedAnthRequest
			srv := serveAnthSSE(t, anthEndTurnSSE, &got)
			client := NewAnthropic("synthetic-key", srv.URL)
			if oauth {
				client = NewAnthropicOAuth("synthetic-token", srv.URL)
			}
			drainAnth(t, client, Request{
				Model:    "claude-sonnet-5-5",
				Advisor:  &NativeAdvisor{Model: "claude-opus-5-5", MaxTokens: 1024},
				Tools:    []Tool{{Name: "read", Schema: json.RawMessage(`{"type":"object"}`)}},
				Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
			})
			if !strings.Contains(got.beta, anthropicAdvisorBeta) {
				t.Errorf("anthropic-beta = %q, want advisor beta", got.beta)
			}
			if oauth && !strings.HasPrefix(got.beta, "claude-code-20250219,") {
				t.Errorf("oauth beta lost Claude Code values: %q", got.beta)
			}
			tools := got.body["tools"].([]any)
			if len(tools) != 2 {
				t.Fatalf("tools = %v", tools)
			}
			adv := tools[0].(map[string]any)
			if adv["type"] != "advisor_20260301" || adv["name"] != "advisor" || adv["model"] != "claude-opus-5-5" || adv["max_tokens"] != float64(1024) {
				t.Errorf("advisor tool = %v", adv)
			}
			if _, ok := adv["cache_control"]; ok {
				t.Error("advisor tool must not carry the cache breakpoint")
			}
			if _, ok := adv["input_schema"]; ok {
				t.Error("advisor tool must not carry input_schema")
			}
			if _, ok := tools[1].(map[string]any)["cache_control"]; !ok {
				t.Error("last client tool lost cache_control")
			}
		})
	}
}

func TestAnthropicNoAdvisorNoBeta(t *testing.T) {
	var got capturedAnthRequest
	srv := serveAnthSSE(t, anthEndTurnSSE, &got)
	drainAnth(t, NewAnthropic("synthetic-key", srv.URL), Request{
		Model:    "claude-sonnet-5-5",
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if got.beta != "" {
		t.Errorf("anthropic-beta = %q, want none", got.beta)
	}
}

func TestAnthropicAdvisorStreamParsing(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start` + "\n" + `data: {"message":{"usage":{"input_tokens":100,"output_tokens":1}}}`,
		`event: content_block_start` + "\n" + `data: {"index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"index":0,"delta":{"type":"text_delta","text":"Let me ask."}}`,
		`event: content_block_stop` + "\n" + `data: {"index":0}`,
		`event: content_block_start` + "\n" + `data: {"index":1,"content_block":` + testAdvisorUseRaw + `}`,
		`event: content_block_delta` + "\n" + `data: {"index":1,"delta":{"type":"input_json_delta","partial_json":""}}`,
		`event: content_block_stop` + "\n" + `data: {"index":1}`,
		`event: content_block_start` + "\n" + `data: {"index":2,"content_block":` + testAdvisorRedactedRaw + `}`,
		`event: content_block_stop` + "\n" + `data: {"index":2}`,
		`event: content_block_start` + "\n" + `data: {"index":3,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"index":3,"delta":{"type":"text_delta","text":"Done."}}`,
		`event: content_block_stop` + "\n" + `data: {"index":3}`,
		`event: message_delta` + "\n" + `data: {"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":50,"iterations":[{"type":"message","input_tokens":40,"output_tokens":10},{"type":"advisor_message","model":"claude-opus-5-5","input_tokens":1000,"output_tokens":200,"cache_read_input_tokens":0,"cache_creation_input_tokens":0},{"type":"message","input_tokens":60,"output_tokens":40}]}}`,
		`event: message_stop` + "\n" + `data: {}`,
	}, "\n\n") + "\n\n"
	var got capturedAnthRequest
	srv := serveAnthSSE(t, sse, &got)
	events, done := drainAnth(t, NewAnthropic("synthetic-key", srv.URL), Request{
		Model:    "claude-sonnet-5-5",
		Advisor:  &NativeAdvisor{Model: "claude-opus-5-5"},
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if done.Err != nil || done.Stop != StopEnd {
		t.Fatalf("done = %+v", done)
	}
	c := done.Message.Content
	if len(c) != 4 {
		t.Fatalf("content = %+v", c)
	}
	if tb, ok := c[0].(TextBlock); !ok || tb.Text != "Let me ask." {
		t.Errorf("c[0] = %+v", c[0])
	}
	use, ok := c[1].(ServerToolBlock)
	if !ok || use.Kind != ServerToolKindUse || use.ID != "srvtoolu_test1" || use.Provider != "anthropic" || string(use.Raw) != testAdvisorUseRaw {
		t.Errorf("c[1] = %+v", c[1])
	}
	res, ok := c[2].(ServerToolBlock)
	if !ok || res.Kind != ServerToolKindAdvisorResult || res.ID != "srvtoolu_test1" || !res.Redacted || res.AdvisorText != "" || string(res.Raw) != testAdvisorRedactedRaw {
		t.Errorf("c[2] = %+v", c[2])
	}
	if tb, ok := c[3].(TextBlock); !ok || tb.Text != "Done." {
		t.Errorf("c[3] = %+v", c[3])
	}

	var serverEvents []EventServerTool
	var usage Usage
	for _, ev := range events {
		switch e := ev.(type) {
		case EventServerTool:
			serverEvents = append(serverEvents, e)
		case EventToolStart, EventToolArgs, EventToolEnd:
			t.Errorf("server tool leaked as client tool event: %#v", e)
		case EventUsage:
			usage = e.Usage
		}
	}
	if len(serverEvents) != 2 ||
		serverEvents[0] != (EventServerTool{ID: "srvtoolu_test1", Name: "advisor", Phase: ServerToolPhaseStart}) ||
		serverEvents[1] != (EventServerTool{ID: "srvtoolu_test1", Name: "advisor", Phase: ServerToolPhaseResult, Redacted: true}) {
		t.Errorf("server events = %+v", serverEvents)
	}

	// Tokens stay executor-only: context comes from the last executor
	// iteration, output stays summed; advisor cost is added at Opus rates.
	if usage.InputTokens != 60 || usage.OutputTokens != 50 {
		t.Errorf("usage tokens = %+v", usage)
	}
	sonnet, _ := FindModel("anthropic", "claude-sonnet-5-5")
	opus, _ := FindModel("anthropic", "claude-opus-5-5")
	want := ComputeCost(sonnet, Usage{InputTokens: 100, OutputTokens: 50}) +
		ComputeCost(opus, Usage{InputTokens: 1000, OutputTokens: 200})
	if math.Abs(usage.CostUSD-want) > 1e-12 || usage.CostUSD <= ComputeCost(sonnet, Usage{InputTokens: 100, OutputTokens: 50}) {
		t.Errorf("cost = %v, want %v", usage.CostUSD, want)
	}
}

func TestAnthropicAdvisorPlaintextAndErrorResults(t *testing.T) {
	plain := `{"type":"advisor_tool_result","tool_use_id":"srvtoolu_p","content":{"type":"advisor_result","text":"Check the cache."}}`
	b, ev, ok := anthServerBlockStart(json.RawMessage(plain))
	if !ok || b.AdvisorText != "Check the cache." || b.Redacted || b.ErrorCode != "" || ev.Text != "Check the cache." {
		t.Errorf("plaintext = %+v %+v", b, ev)
	}
	failed := `{"type":"advisor_tool_result","tool_use_id":"srvtoolu_e","content":{"type":"advisor_tool_result_error","error_code":"overloaded"}}`
	b, ev, ok = anthServerBlockStart(json.RawMessage(failed))
	if !ok || b.ErrorCode != "overloaded" || ev.ErrorCode != "overloaded" {
		t.Errorf("error = %+v %+v", b, ev)
	}
}

func TestAnthropicPauseTurnContinues(t *testing.T) {
	sse := "event: content_block_start\ndata: {\"index\":0,\"content_block\":" + testAdvisorUseRaw + "}\n\n" +
		"event: content_block_stop\ndata: {\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"pause_turn\"}}\n\nevent: message_stop\ndata: {}\n\n"
	var got capturedAnthRequest
	srv := serveAnthSSE(t, sse, &got)
	_, done := drainAnth(t, NewAnthropic("synthetic-key", srv.URL), Request{
		Model:    "claude-sonnet-5-5",
		Advisor:  &NativeAdvisor{Model: "claude-opus-5-5"},
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if done.Stop != StopContinue || len(done.Message.Content) != 1 {
		t.Fatalf("done = %+v", done)
	}
}

func TestAnthropicAdvisorMixedTurnReplay(t *testing.T) {
	// Assistant N calls a client tool while the advisor is pending; the
	// result arrives at index 0 of assistant N+1.
	history := []Message{
		{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}},
		{Role: RoleAssistant, Content: []Content{testAdvisorUse("srvtoolu_test1"), ToolCallBlock{ID: "call1", Name: "read", Arguments: json.RawMessage(`{}`)}}},
		{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "call1", Content: []Content{TextBlock{Text: "ok"}}}}},
		{Role: RoleAssistant, Content: []Content{testAdvisorResult("srvtoolu_test1"), TextBlock{Text: "after"}}},
		{Role: RoleUser, Content: []Content{TextBlock{Text: "next"}}},
	}
	for _, withAdvisor := range []bool{true, false} {
		t.Run(fmt.Sprintf("advisor=%t", withAdvisor), func(t *testing.T) {
			var got capturedAnthRequest
			srv := serveAnthSSE(t, anthEndTurnSSE, &got)
			req := Request{Model: "claude-sonnet-5-5", Messages: history,
				Tools: []Tool{{Name: "read", Schema: json.RawMessage(`{"type":"object"}`)}}}
			if withAdvisor {
				req.Advisor = &NativeAdvisor{Model: "claude-opus-5-5"}
			}
			drainAnth(t, NewAnthropic("synthetic-key", srv.URL), req)
			msgs := wireMessages(got.body)
			if len(msgs) != 5 {
				t.Fatalf("messages = %v", msgs)
			}
			if got := strings.Join(wireBlockTypes(msgs[1]), ","); got != "server_tool_use,tool_use" {
				t.Errorf("assistant N blocks = %s", got)
			}
			if got := strings.Join(wireBlockTypes(msgs[3]), ","); got != "advisor_tool_result,text" {
				t.Errorf("assistant N+1 blocks = %s", got)
			}
			raw, _ := json.Marshal(msgs[3]["content"].([]any)[0])
			var a, b any
			_ = json.Unmarshal(raw, &a)
			_ = json.Unmarshal([]byte(testAdvisorRedactedRaw), &b)
			if fmt.Sprint(a) != fmt.Sprint(b) {
				t.Errorf("result not replayed verbatim: %s", raw)
			}
			// History with server blocks needs the beta even without the tool.
			if !strings.Contains(got.beta, anthropicAdvisorBeta) {
				t.Errorf("beta = %q", got.beta)
			}
		})
	}
}

func TestAnthropicAdvisorPendingInLastAssistant(t *testing.T) {
	history := []Message{
		{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}},
		{Role: RoleAssistant, Content: []Content{TextBlock{Text: "thinking"}, testAdvisorUse("srvtoolu_pending")}},
	}
	build := func(adv *NativeAdvisor) *anthRequest {
		t.Helper()
		wire, err := NewAnthropic("synthetic-key", "").(*anthropicClient).buildRequest(Request{Model: "claude-sonnet-5-5", Messages: history, Advisor: adv})
		if err != nil {
			t.Fatal(err)
		}
		return wire
	}
	wire := build(&NativeAdvisor{Model: "claude-opus-5-5"})
	if n := len(wire.Messages[1].Content); n != 2 || !wire.advisorBeta {
		t.Errorf("pending use not kept with advisor: %+v", wire.Messages[1].Content)
	}
	// Resuming a pending call without the tool definition is a 400, so the
	// block is dropped when the advisor is off.
	wire = build(nil)
	if n := len(wire.Messages[1].Content); n != 1 || wire.advisorBeta {
		t.Errorf("pending use kept without advisor: %+v beta=%t", wire.Messages[1].Content, wire.advisorBeta)
	}
}

func TestAnthropicServerBlockOrphans(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}},
		// Unmatched use not in the last assistant message: dropped, and the
		// emptied message disappears.
		{Role: RoleAssistant, Content: []Content{testAdvisorUse("srvtoolu_old")}},
		{Role: RoleUser, Content: []Content{TextBlock{Text: "again"}}},
		// Result without a prior use: dropped.
		{Role: RoleAssistant, Content: []Content{testAdvisorResult("srvtoolu_ghost"), TextBlock{Text: "a"}}},
		// Result before its use: both dropped.
		{Role: RoleAssistant, Content: []Content{testAdvisorResult("srvtoolu_rev"), testAdvisorUse("srvtoolu_rev"), TextBlock{Text: "b"}}},
		// Invalid raw and foreign provider: dropped.
		{Role: RoleAssistant, Content: []Content{
			ServerToolBlock{Provider: "anthropic", Kind: ServerToolKindUse, ID: "srvtoolu_bad", Raw: json.RawMessage(`{bad`)},
			ServerToolBlock{Provider: "other", Kind: ServerToolKindUse, ID: "srvtoolu_x", Raw: json.RawMessage(`{}`)},
			TextBlock{Text: "c"},
		}},
	}
	out, has := prepareAnthServerBlocks(msgs, true, true)
	if has {
		t.Errorf("hasServer = true, out = %+v", out)
	}
	if len(out) != 5 {
		t.Fatalf("messages = %d: %+v", len(out), out)
	}
	for _, m := range out {
		for _, c := range m.Content {
			if _, ok := c.(ServerToolBlock); ok {
				t.Errorf("orphan kept: %+v", c)
			}
		}
	}
	if len(msgs[1].Content) != 1 {
		t.Error("caller history mutated")
	}
}

func TestAnthropicCompatNeverSendsAdvisor(t *testing.T) {
	var got capturedAnthRequest
	srv := serveAnthSSE(t, anthEndTurnSSE, &got)
	client := NewAnthropicCompat("kimi", "synthetic-key", srv.URL)
	client.(*anthropicClient).SetModelMetadata(Model{Provider: "kimi", ID: "claude-sonnet-5-5", MaxOutput: 1000})
	drainAnth(t, client, Request{
		Model:   "claude-sonnet-5-5",
		Advisor: &NativeAdvisor{Model: "claude-opus-5-5"},
		Messages: []Message{
			{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}},
			{Role: RoleAssistant, Content: []Content{testAdvisorUse("srvtoolu_test1"), testAdvisorResult("srvtoolu_test1"), TextBlock{Text: "ok"}}},
			{Role: RoleUser, Content: []Content{TextBlock{Text: "next"}}},
		},
	})
	if got.beta != "" {
		t.Errorf("beta = %q", got.beta)
	}
	if _, ok := got.body["tools"]; ok {
		t.Errorf("tools sent: %v", got.body["tools"])
	}
	if raw, _ := json.Marshal(got.body); strings.Contains(string(raw), "srvtoolu_") {
		t.Errorf("server block leaked: %s", raw)
	}
}

func TestOpenAISkipsServerToolBlock(t *testing.T) {
	c := NewOpenAI("synthetic-key", "").(*openaiClient)
	wire, err := c.buildRequest(Request{Model: "gpt-4o", Messages: []Message{
		{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}},
		{Role: RoleAssistant, Content: []Content{testAdvisorUse("srvtoolu_test1"), testAdvisorResult("srvtoolu_test1"), TextBlock{Text: "ok"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(wire); strings.Contains(string(raw), "srvtoolu_") {
		t.Errorf("server block leaked: %s", raw)
	}
}

func TestAnthropicAdvisorIterationUsageContext(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start` + "\n" + `data: {"message":{"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
		`event: message_delta` + "\n" + `data: {"delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":2349,"output_tokens":90,"cache_read_input_tokens":3000,"cache_creation_input_tokens":500,"iterations":[{"type":"message","input_tokens":1085,"output_tokens":30,"cache_read_input_tokens":1000,"cache_creation_input_tokens":500},{"type":"advisor_message","model":"claude-opus-5-5","input_tokens":4000,"output_tokens":300},{"type":"message","input_tokens":1264,"output_tokens":60,"cache_read_input_tokens":2000,"cache_creation_input_tokens":0}]}}`,
		`event: message_stop` + "\n" + `data: {}`,
	}, "\n\n") + "\n\n"
	var got capturedAnthRequest
	srv := serveAnthSSE(t, sse, &got)
	events, _ := drainAnth(t, NewAnthropic("synthetic-key", srv.URL), Request{
		Model:    "claude-sonnet-5-5",
		Advisor:  &NativeAdvisor{Model: "claude-opus-5-5"},
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	var usage Usage
	for _, ev := range events {
		if u, ok := ev.(EventUsage); ok {
			usage = u.Usage
		}
	}
	if usage.InputTokens != 1264 || usage.CacheReadTokens != 2000 || usage.CacheWriteTokens != 0 || usage.OutputTokens != 90 ||
		usage.CacheMeasuredPromptTokens != 3264 || usage.CacheMeasuredReadTokens != 2000 {
		t.Errorf("context usage = %+v", usage)
	}
	sonnet, _ := FindModel("anthropic", "claude-sonnet-5-5")
	opus, _ := FindModel("anthropic", "claude-opus-5-5")
	want := ComputeCost(sonnet, Usage{InputTokens: 2349, OutputTokens: 90, CacheReadTokens: 3000, CacheWriteTokens: 500}) +
		ComputeCost(opus, Usage{InputTokens: 4000, OutputTokens: 300})
	if math.Abs(usage.CostUSD-want) > 1e-12 {
		t.Errorf("cost = %v, want %v", usage.CostUSD, want)
	}
}

func TestAnthropicPendingUseRequiresOnlyToolResultsAfter(t *testing.T) {
	build := func(history []Message) *anthRequest {
		t.Helper()
		wire, err := NewAnthropic("synthetic-key", "").(*anthropicClient).buildRequest(Request{
			Model: "claude-sonnet-5-5", Messages: history, Advisor: &NativeAdvisor{Model: "claude-opus-5-5"},
			Tools: []Tool{{Name: "read", Schema: json.RawMessage(`{"type":"object"}`)}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return wire
	}
	mixed := []Message{
		{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}},
		{Role: RoleAssistant, Content: []Content{testAdvisorUse("srvtoolu_p"), ToolCallBlock{ID: "call1", Name: "read", Arguments: json.RawMessage(`{}`)}}},
		{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "call1", Content: []Content{TextBlock{Text: "ok"}}}}},
	}
	if got := strings.Join(wireTypesOf(build(mixed).Messages[1]), ","); got != "server_tool_use,tool_use" {
		t.Errorf("mixed-turn pending use dropped: %s", got)
	}
	// A plain user message after the pending call is a 400; drop it.
	followed := append(append([]Message(nil), mixed...), Message{Role: RoleUser, Content: []Content{TextBlock{Text: "next"}}})
	wire := build(followed)
	if got := strings.Join(wireTypesOf(wire.Messages[1]), ","); got != "tool_use" {
		t.Errorf("pending use kept before user text: %s", got)
	}
	if wire.advisorBeta != true {
		t.Error("advisor beta missing while advisor tool is sent")
	}
}

func wireTypesOf(m anthMessage) []string {
	raw, _ := json.Marshal(m)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return wireBlockTypes(decoded)
}

func TestAnthropicClientAdvisorToolStripsServerBlocks(t *testing.T) {
	plain := ServerToolBlock{Provider: "anthropic", Kind: ServerToolKindAdvisorResult, ID: "srvtoolu_test1", Name: "advisor", AdvisorText: "Check the cache.",
		Raw: json.RawMessage(`{"type":"advisor_tool_result","tool_use_id":"srvtoolu_test1","content":{"type":"advisor_result","text":"Check the cache."}}`)}
	wire, err := NewAnthropic("synthetic-key", "").(*anthropicClient).buildRequest(Request{
		Model: "claude-sonnet-5-5",
		Tools: []Tool{{Name: "advisor", Schema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []Message{
			{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}},
			{Role: RoleAssistant, Content: []Content{testAdvisorUse("srvtoolu_test1"), plain, testAdvisorUse("srvtoolu_r"), testAdvisorResult("srvtoolu_r"), TextBlock{Text: "ok"}}},
			{Role: RoleUser, Content: []Content{TextBlock{Text: "next"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(wire)
	if strings.Contains(string(raw), "srvtoolu_") || wire.advisorBeta {
		t.Fatalf("server blocks replayed next to a client advisor tool: %s", raw)
	}
	if !strings.Contains(string(raw), "[advisor advice] Check the cache.") {
		t.Errorf("plaintext advice note missing: %s", raw)
	}
}

func TestAnthropicAdvisorMaxTokensClamp(t *testing.T) {
	for in, want := range map[int]int{0: 0, -5: 0, 1: 1024, 1023: 1024, 1024: 1024, 4096: 4096} {
		if got := anthAdvisorMaxTokens(in); got != want {
			t.Errorf("anthAdvisorMaxTokens(%d) = %d, want %d", in, got, want)
		}
	}
	wire, err := NewAnthropic("synthetic-key", "").(*anthropicClient).buildRequest(Request{
		Model: "claude-sonnet-5-5", Advisor: &NativeAdvisor{Model: "claude-opus-5-5", MaxTokens: 10},
		Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tool := wire.Tools[0].(anthAdvisorTool); tool.MaxTokens != 1024 {
		t.Errorf("wire max_tokens = %d", tool.MaxTokens)
	}
}

func TestAnthropicServerBlockWithoutIDRejected(t *testing.T) {
	for _, raw := range []string{
		`{"type":"server_tool_use","id":"","name":"advisor","input":{}}`,
		`{"type":"advisor_tool_result","content":{"type":"advisor_result","text":"x"}}`,
	} {
		if _, _, ok := anthServerBlockStart(json.RawMessage(raw)); ok {
			t.Errorf("accepted block without id: %s", raw)
		}
	}
}

func TestAnthropicPingEmitsKeepalive(t *testing.T) {
	ping := "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	use := "event: content_block_start\ndata: {\"index\":0,\"content_block\":" + testAdvisorUseRaw + "}\n\nevent: content_block_stop\ndata: {\"index\":0}\n\n"
	result := "event: content_block_start\ndata: {\"index\":1,\"content_block\":" + testAdvisorRedactedRaw + "}\n\nevent: content_block_stop\ndata: {\"index\":1}\n\n"
	resumed := []Message{
		{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}},
		{Role: RoleAssistant, Content: []Content{testAdvisorUse("srvtoolu_test1"), ToolCallBlock{ID: "c1", Name: "read", Arguments: json.RawMessage(`{}`)}}},
		{Role: RoleTool, Content: []Content{ToolResultBlock{CallID: "c1", Content: []Content{TextBlock{Text: "ok"}}}}},
	}
	plain := []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}}
	for _, tc := range []struct {
		name    string
		sse     string
		history []Message
		want    int
	}{
		{"no pending", ping + ping, plain, 0},
		{"pending in stream", ping + use + ping + result + ping, plain, 1},
		{"resumed from history", ping + result + ping, resumed, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got capturedAnthRequest
			srv := serveAnthSSE(t, tc.sse+anthEndTurnSSE, &got)
			events, _ := drainAnth(t, NewAnthropic("synthetic-key", srv.URL), Request{
				Model: "claude-sonnet-5-5", Advisor: &NativeAdvisor{Model: "claude-opus-5-5"}, Messages: tc.history,
				Tools: []Tool{{Name: "read", Schema: json.RawMessage(`{"type":"object"}`)}},
			})
			n := 0
			for _, ev := range events {
				if _, ok := ev.(EventKeepalive); ok {
					n++
				}
			}
			if n != tc.want {
				t.Fatalf("keepalives = %d, want %d", n, tc.want)
			}
		})
	}
}

func TestPrepareAnthServerBlocksFastPath(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}}
	out, has := prepareAnthServerBlocks(msgs, true, true)
	if has || len(out) != 1 || &out[0] != &msgs[0] {
		t.Fatal("fast path copied messages")
	}
}
