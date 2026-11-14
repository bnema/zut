package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"unicode/utf8"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

type bridgeTestTool struct{ name, description string }

func (t bridgeTestTool) Name() string            { return t.name }
func (t bridgeTestTool) Description() string     { return t.description }
func (t bridgeTestTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t bridgeTestTool) Deferred() bool          { return true }
func (t bridgeTestTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	panic("invoke through NestedInvoker")
}

type bridgeTestInvoker struct {
	tools  core.Registry
	invoke func(context.Context, string, json.RawMessage) core.NestedOutcome
}

func (i *bridgeTestInvoker) Tools() core.Registry { return i.tools }
func (i *bridgeTestInvoker) Invoke(ctx context.Context, n string, a json.RawMessage) core.NestedOutcome {
	return i.invoke(ctx, n, a)
}

type bridgeClient struct {
	w       *io.PipeWriter
	decoder *json.Decoder
}

func startBridge(t *testing.T, b *codemodeBridge) (*bridgeClient, <-chan error) {
	t.Helper()
	r, w := io.Pipe()
	rr, ww := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.serve(ctx, r, ww) }()
	t.Cleanup(func() { cancel(); r.Close(); w.Close(); rr.Close(); ww.Close() })
	return &bridgeClient{w, json.NewDecoder(rr)}, done
}
func (c *bridgeClient) raw(t *testing.T, request string) map[string]any {
	t.Helper()
	if _, err := io.WriteString(c.w, request+"\n"); err != nil {
		t.Fatal(err)
	}
	var reply map[string]any
	if err := c.decoder.Decode(&reply); err != nil {
		t.Fatal(err)
	}
	return reply
}
func (c *bridgeClient) request(t *testing.T, method string, params any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	return c.raw(t, string(raw))
}
func bridgeResult(t *testing.T, reply map[string]any) map[string]any {
	t.Helper()
	result, ok := reply["result"].(map[string]any)
	if !ok || reply["error"] != nil {
		t.Fatalf("expected result: %#v", reply)
	}
	return result
}
func bridgeError(t *testing.T, reply map[string]any, kind string) {
	t.Helper()
	e, ok := reply["error"].(map[string]any)
	if !ok || e["kind"] != kind || e["message"] == "" || reply["result"] != nil {
		t.Fatalf("want %s error: %#v", kind, reply)
	}
}
func bridgeWait(t *testing.T, done <-chan error) error {
	t.Helper()
	synctest.Wait()
	return <-done
}

func TestCodemodeBridgeMethods(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inv := &bridgeTestInvoker{tools: core.NewRegistry(bridgeTestTool{"echo", "Echo description\nmore"}, bridgeTestTool{"codemode", "hidden"})}
		inv.invoke = func(_ context.Context, name string, args json.RawMessage) core.NestedOutcome {
			if name != "echo" || string(args) != `{"x":1}` {
				t.Errorf("invoke %s %s", name, args)
			}
			return core.NestedOutcome{ID: "parent/1", Result: core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "hello"}, provider.TextBlock{Text: " world"}}, Data: map[string]int{"x": 1}}}
		}
		b := newCodemodeBridge(inv, "print('hi')")
		c, done := startBridge(t, b)
		if got := bridgeResult(t, c.request(t, "init", map[string]any{}))["code"]; got != "print('hi')" {
			t.Fatal(got)
		}
		desc := bridgeResult(t, c.request(t, "describe", map[string]any{"name": "echo"}))
		if desc["name"] != "echo" || desc["description"] != "Echo description\nmore" || desc["schema"].(map[string]any)["type"] != "object" {
			t.Fatal(desc)
		}
		search := bridgeResult(t, c.request(t, "search", map[string]any{"query": "DESCRIPTION"}))["results"].([]any)
		if len(search) != 1 || search[0].(map[string]any)["summary"] != "Echo description" {
			t.Fatal(search)
		}
		result := bridgeResult(t, c.request(t, "call", map[string]any{"name": "echo", "args": map[string]int{"x": 1}}))
		if result["id"] != "parent/1" || result["text"] != "hello world" || result["is_error"] != false || result["data"].(map[string]any)["x"] != float64(1) {
			t.Fatal(result)
		}
		bridgeResult(t, c.raw(t, `{"method":"result","params":{"value": { "a": [1, 2] }}}`))
		if err := c.w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := bridgeWait(t, done); err != nil {
			t.Fatal(err)
		}
		if string(b.Result()) != `{"a":[1,2]}` {
			t.Fatalf("result %s", b.Result())
		}
		if calls, errs, denied := b.Counts(); calls != 1 || errs != 0 || denied != 0 {
			t.Fatal(calls, errs, denied)
		}
	})
}

func TestCodemodeBridgeErrorsAndLimits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inv := &bridgeTestInvoker{tools: core.NewRegistry(bridgeTestTool{"echo", "echo"})}
		invoked := 0
		inv.invoke = func(context.Context, string, json.RawMessage) core.NestedOutcome {
			invoked++
			return core.NestedOutcome{Denied: invoked == 1, Result: core.ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: "refused"}}, Data: make(chan int)}}
		}
		b := newCodemodeBridge(inv, "")
		c, done := startBridge(t, b)
		for _, raw := range []string{`no JSON`, `{"method":"missing"}`, `{"method":"call","params":[]}`, `{"method":"call","params":null}`, `{"method":"call","params":{}}`, `{"method":"result","params":{}}`} {
			bridgeError(t, c.raw(t, raw), "bad_request")
		}
		for _, name := range []string{"codemode", "subagent", "ask_user_question", "plan", "update_goal", "schedule", "sessions"} {
			for _, method := range []string{"call", "describe"} {
				bridgeError(t, c.request(t, method, map[string]any{"name": name}), "not_allowed")
			}
		}
		for _, method := range []string{"call", "describe"} {
			bridgeError(t, c.request(t, method, map[string]any{"name": "absent"}), "unknown_tool")
		}
		denied := c.request(t, "call", map[string]any{"name": "echo"})
		bridgeError(t, denied, "denied")
		if denied["error"].(map[string]any)["message"] != "refused" {
			t.Fatal(denied)
		}
		for range 199 {
			r := bridgeResult(t, c.request(t, "call", map[string]any{"name": "echo"}))
			if r["is_error"] != true || r["data"] != nil {
				t.Fatal(r)
			}
		}
		bridgeError(t, c.request(t, "call", map[string]any{"name": "echo"}), "limit")
		bridgeError(t, c.request(t, "result", map[string]any{"value": strings.Repeat("x", 256*1024)}), "limit")
		bridgeResult(t, c.request(t, "result", map[string]any{"value": nil}))
		c.w.Close()
		if err := bridgeWait(t, done); err != nil {
			t.Fatal(err)
		}
		if invoked != 200 {
			t.Fatal(invoked)
		}
		if calls, errs, denied := b.Counts(); calls != 200 || errs != 199 || denied != 1 {
			t.Fatal(calls, errs, denied)
		}
	})
}

func TestCodemodeBridgeTruncationAndContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		context1 := provider.ToolContext{Reads: []provider.ResourceRef{{Key: "a", Variant: "1"}}, Discovers: []string{"a"}, Mutates: []string{"b"}}
		context2 := provider.ToolContext{Reads: []provider.ResourceRef{{Key: "a", Variant: "1"}, {Key: "a", Variant: "2"}}, Discovers: []string{"a", "c"}, Mutates: []string{"b", "d"}}
		inv := &bridgeTestInvoker{tools: core.NewRegistry(bridgeTestTool{"echo", "echo"})}
		count := 0
		inv.invoke = func(context.Context, string, json.RawMessage) core.NestedOutcome {
			count++
			ctx := context1
			if count > 1 {
				ctx = context2
			}
			return core.NestedOutcome{Result: core.ToolResult{Context: ctx, Content: []provider.Content{provider.TextBlock{Text: strings.Repeat("x", 1024*1024-1)}, provider.TextBlock{Text: "é!"}}}}
		}
		b := newCodemodeBridge(inv, "")
		c, done := startBridge(t, b)
		for range 2 {
			r := bridgeResult(t, c.request(t, "call", map[string]any{"name": "echo"}))
			text := r["text"].(string)
			if !utf8.ValidString(text) || text != strings.Repeat("x", 1024*1024-1)+"\n[codemode: output truncated at 1 MiB]" {
				t.Fatal("incorrect truncation")
			}
		}
		c.w.Close()
		if err := bridgeWait(t, done); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(b.Context(), context2) {
			t.Fatalf("context %#v", b.Context())
		}
		copy := b.Context()
		copy.Reads[0].Key = "changed"
		if b.Context().Reads[0].Key != "a" {
			t.Fatal("context aliases bridge")
		}
	})
}

func TestCodemodeBridgeSearchLimits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inv := &bridgeTestInvoker{tools: core.NewRegistry(bridgeTestTool{"codemode", "hidden"})}
		for i := 59; i >= 0; i-- {
			name := fmt.Sprintf("tool%02d", i)
			inv.tools[name] = bridgeTestTool{name, strings.Repeat("é", 81) + "\nsecond"}
		}
		b := newCodemodeBridge(inv, "")
		c, done := startBridge(t, b)
		for _, tc := range []struct{ limit, want int }{{0, 10}, {100, 50}, {3, 3}} {
			results := bridgeResult(t, c.request(t, "search", map[string]any{"limit": tc.limit}))["results"].([]any)
			if len(results) != tc.want {
				t.Fatal(len(results))
			}
			for i, v := range results {
				entry := v.(map[string]any)
				if entry["name"] != fmt.Sprintf("tool%02d", i) || entry["summary"] != strings.Repeat("é", 80) {
					t.Fatal(entry)
				}
			}
		}
		c.w.Close()
		if err := bridgeWait(t, done); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCodemodeBridgeOverlongLine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newCodemodeBridge(&bridgeTestInvoker{}, "")
		c, done := startBridge(t, b)
		written := make(chan struct{})
		go func() { defer close(written); _, _ = io.WriteString(c.w, strings.Repeat("x", codemodeMaxLine+1)+"\n") }()
		var reply map[string]any
		if err := c.decoder.Decode(&reply); err != nil {
			t.Fatal(err)
		}
		bridgeError(t, reply, "bad_request")
		if err := bridgeWait(t, done); err != nil {
			t.Fatal(err)
		}
		<-written
	})
}

func TestCodemodeBridgeCancellation(t *testing.T) {
	for _, blockedWrite := range []bool{false, true} {
		t.Run(fmt.Sprint(blockedWrite), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r, w := io.Pipe()
				rr, ww := io.Pipe()
				defer r.Close()
				defer w.Close()
				defer rr.Close()
				defer ww.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				b := newCodemodeBridge(&bridgeTestInvoker{}, "")
				go func() { done <- b.serve(ctx, r, ww) }()
				if blockedWrite {
					if _, err := io.WriteString(w, `{"method":"init"}`+"\n"); err != nil {
						t.Fatal(err)
					}
				}
				synctest.Wait()
				cancel()
				if err := bridgeWait(t, done); !errors.Is(err, context.Canceled) {
					t.Fatalf("serve: %v", err)
				}
				// serve waits for its cancellation callback, so both ends are closed now.
				if _, err := w.Write([]byte("x")); err == nil {
					t.Fatal("request pipe still open")
				}
				if _, err := rr.Read(make([]byte, 1)); err != io.EOF {
					t.Fatalf("response pipe: %v", err)
				}
			})
		})
	}
}

func TestCodemodeBridgeNameResolution(t *testing.T) {
	inv := &bridgeTestInvoker{tools: core.NewRegistry(
		bridgeTestTool{"read", ""}, bridgeTestTool{"mcp__svelte__list-sections", ""},
		bridgeTestTool{"foo-bar", ""}, bridgeTestTool{"foo_bar", ""},
		bridgeTestTool{"ask_user_question", ""}, bridgeTestTool{"codemode", ""},
		bridgeTestTool{"read_a", ""}, bridgeTestTool{"read_b", ""}, bridgeTestTool{"read_c", ""},
	)}
	var invoked string
	inv.invoke = func(_ context.Context, name string, _ json.RawMessage) core.NestedOutcome {
		invoked = name
		return core.NestedOutcome{}
	}
	b := newCodemodeBridge(inv, "")
	for _, method := range []string{"call", "describe"} {
		for _, tc := range []struct{ requested, canonical string }{
			{"Read", "read"}, {"mcp__svelte__list_sections", "mcp__svelte__list-sections"}, {"foo_bar", "foo_bar"},
		} {
			raw, _ := json.Marshal(map[string]any{"method": method, "params": map[string]string{"name": tc.requested}})
			reply := b.handle(context.Background(), raw)
			if reply.Error != nil {
				t.Fatal(reply.Error)
			}
			if method == "call" && invoked != tc.canonical {
				t.Fatal(invoked)
			}
			if method == "describe" && reply.Result.(map[string]any)["name"] != tc.canonical {
				t.Fatal(reply)
			}
		}
		for _, tc := range []struct{ name, kind, message string }{
			{"FOO_BAR", "unknown_tool", "candidates: foo-bar, foo_bar"},
			{"rea", "unknown_tool", "did you mean: read, read_a, read_b?"},
			{"ASK-USER-QUESTION", "not_allowed", "not allowed"},
			{"CODEMODE", "not_allowed", "not allowed"},
			{"codem", "unknown_tool", "unknown tool"},
		} {
			raw, _ := json.Marshal(map[string]any{"method": method, "params": map[string]string{"name": tc.name}})
			reply := b.handle(context.Background(), raw)
			if reply.Error == nil || reply.Error.Kind != tc.kind || !strings.Contains(reply.Error.Message, tc.message) {
				t.Fatalf("%s: %#v", tc.name, reply.Error)
			}
			if tc.name == "codem" && strings.Contains(reply.Error.Message, "did you mean") {
				t.Fatal(reply.Error)
			}
		}
	}
}

func TestCodemodeBridgeRankedSearch(t *testing.T) {
	inv := &bridgeTestInvoker{tools: core.NewRegistry(
		bridgeTestTool{"read", "Read files"}, bridgeTestTool{"reader", ""},
		bridgeTestTool{"mcp__blender__read", ""}, bridgeTestTool{"a", "read docs"},
		bridgeTestTool{"mcp__svelte__list-sections", "Svelte docs"},
		bridgeTestTool{"svelte", "components"}, bridgeTestTool{"codemode", "read docs"},
	)}
	b := newCodemodeBridge(inv, "")
	for _, tc := range []struct {
		query     string
		limit     int
		names     []string
		total     int
		truncated bool
	}{
		{"READ", 2, []string{"read", "reader"}, 4, true},
		{"read", 10, []string{"read", "reader", "mcp__blender__read", "a"}, 4, false},
		{"svelte docs", 10, []string{"mcp__svelte__list-sections"}, 1, false},
		{"LIST_SECTIONS", 10, []string{"mcp__svelte__list-sections"}, 1, false},
		{"missing", 10, []string{}, 0, false},
		{"  ", 2, []string{"a", "mcp__blender__read"}, 6, true},
	} {
		raw, _ := json.Marshal(map[string]any{"method": "search", "params": map[string]any{"query": tc.query, "limit": tc.limit}})
		reply := b.handle(context.Background(), raw)
		encoded, _ := json.Marshal(reply.Result)
		var result struct {
			Results []struct {
				Name    string
				Summary string
				Score   int
			}
			Total     int
			Truncated bool
		}
		if err := json.Unmarshal(encoded, &result); err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for _, entry := range result.Results {
			names = append(names, entry.Name)
		}
		if !reflect.DeepEqual(names, tc.names) || result.Total != tc.total || result.Truncated != tc.truncated {
			t.Fatalf("%q: %s", tc.query, encoded)
		}
		if tc.query == "read" {
			for i, want := range []int{4, 3, 2, 1} {
				if result.Results[i].Score != want {
					t.Fatal(string(encoded))
				}
			}
		}
	}
}
