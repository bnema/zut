package ext

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/extproto"
)

// interactiveHarness registers one interactive tool whose handler blocks on
// ctx or on an explicit answer, and reports when it started and ended.
type interactiveHarness struct {
	*extHarness
	started chan string // tool_call id
	ended   chan error  // ctx.Err() at handler exit
	answers chan string
}

func newInteractiveHarness(t *testing.T) *interactiveHarness {
	t.Helper()
	h := &interactiveHarness{
		extHarness: newHarness("interactive-ext"),
		started:    make(chan string, 8),
		ended:      make(chan error, 8),
		answers:    make(chan string, 8),
	}
	h.ext.InteractiveTool("ask", "ask the user", json.RawMessage(`{"type":"object"}`),
		func(ctx context.Context, args json.RawMessage) ToolResult {
			var in struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(args, &in)
			h.started <- in.ID
			select {
			case a := <-h.answers:
				h.ended <- ctx.Err()
				return TextResult(a)
			case <-ctx.Done():
				h.ended <- ctx.Err()
				return TextErrorResult("cancelled")
			}
		})
	h.ext.Tool("plain", "plain", json.RawMessage(`{"type":"object"}`), func(json.RawMessage) ToolResult {
		return TextResult("plain-ok")
	})
	return h
}

func (h *interactiveHarness) call(t *testing.T, id string) {
	t.Helper()
	h.sendToExt(t, extproto.ToolCallFromHost{Type: "tool_call", ID: id, Name: "ask", Args: json.RawMessage(`{"id":"` + id + `"}`)})
	select {
	case got := <-h.started:
		if got != id {
			t.Fatalf("handler started for %q, want %q", got, id)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("handler for %s did not start", id)
	}
}

func (h *interactiveHarness) awaitEnd(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.ended:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not end")
		return nil
	}
}

// expectNoToolResult drains frames for a short window and fails on any
// tool_result. Used after a cancel to prove late results are suppressed.
func (h *interactiveHarness) expectNoToolResult(t *testing.T) {
	t.Helper()
	// Force a round trip: an event frame is processed on the read loop after
	// any tool_result the handler goroutine could have queued, so once we
	// observe the ack for it, an earlier tool_result would already be here.
	h.ext.OnPanelKey("probe", func(string, string) { h.ext.Notify("info", "probe") }, nil)
	h.sendToExt(t, extproto.PanelKeyFromHost{Type: "panel_key", PanelID: "probe", Key: "rune", Text: "x"})
	for {
		f := h.next(t)
		switch f.hdr.Type {
		case "tool_result":
			t.Fatalf("late tool_result emitted after cancellation: %s", f.raw)
		case "notify":
			return
		}
	}
}

func TestInteractiveToolRegistersFlagAndCapability(t *testing.T) {
	h := newInteractiveHarness(t)
	h.startRun(t)
	f := h.next(t)
	var hello extproto.HelloFromExt
	if err := json.Unmarshal(f.raw, &hello); err != nil {
		t.Fatal(err)
	}
	hasCancel := false
	for _, c := range hello.Capabilities {
		if c == "tool_cancel" {
			hasCancel = true
		}
	}
	if !hasCancel {
		t.Fatalf("hello capabilities = %v, want tool_cancel", hello.Capabilities)
	}
	h.sendToExt(t, extproto.HelloAckFromHost{Type: "hello_ack", ProtocolVersion: extproto.ProtocolVersion, ZutVersion: "0.0.0-test"})
	seen := map[string]bool{}
	for {
		f := h.next(t)
		if f.hdr.Type == "ready" {
			break
		}
		if f.hdr.Type != "register_tool" {
			continue
		}
		var rt extproto.RegisterToolFromExt
		if err := json.Unmarshal(f.raw, &rt); err != nil {
			t.Fatal(err)
		}
		seen[rt.Name] = rt.Interactive
	}
	if !seen["ask"] || seen["plain"] {
		t.Fatalf("interactive flags = %v", seen)
	}
}

func TestInteractiveToolCompletes(t *testing.T) {
	h := newInteractiveHarness(t)
	h.startRun(t)
	h.handshake(t)
	h.call(t, "c1")
	h.answers <- "yes"
	if err := h.awaitEnd(t); err != nil {
		t.Fatalf("handler ctx err = %v, want nil", err)
	}
	f := h.drainUntil(t, "tool_result")
	var tr extproto.ToolResultFromExt
	if err := json.Unmarshal(f.raw, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.ID != "c1" || tr.IsError || len(tr.Content) != 1 || tr.Content[0].Text != "yes" {
		t.Fatalf("tool_result = %+v", tr)
	}
	// The goroutine's deferred cleanup runs after respondTool; poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.ext.mu.Lock()
		inflight := len(h.ext.toolCancels)
		h.ext.mu.Unlock()
		if inflight == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("in-flight cancel handles leaked: %d", inflight)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestToolCancelCancelsHandlerAndSuppressesResult(t *testing.T) {
	h := newInteractiveHarness(t)
	h.startRun(t)
	h.handshake(t)
	h.call(t, "c1")
	h.call(t, "c2")
	h.sendToExt(t, extproto.ToolCancelFromHost{Type: "tool_cancel", ID: "c1"})
	if err := h.awaitEnd(t); err != context.Canceled {
		t.Fatalf("cancelled handler ctx err = %v", err)
	}
	h.expectNoToolResult(t)
	// The other in-flight call is unaffected and still completes.
	h.answers <- "two"
	if err := h.awaitEnd(t); err != nil {
		t.Fatalf("second handler ctx err = %v", err)
	}
	f := h.drainUntil(t, "tool_result")
	var tr extproto.ToolResultFromExt
	_ = json.Unmarshal(f.raw, &tr)
	if tr.ID != "c2" || tr.Content[0].Text != "two" {
		t.Fatalf("tool_result = %+v", tr)
	}
	// Unknown / already-finished ids are ignored without side effects.
	h.sendToExt(t, extproto.ToolCancelFromHost{Type: "tool_cancel", ID: "c1"})
	h.sendToExt(t, extproto.ToolCancelFromHost{Type: "tool_cancel", ID: "nope"})
	h.sendToExt(t, extproto.ToolCallFromHost{Type: "tool_call", ID: "c3", Name: "plain", Args: json.RawMessage(`{}`)})
	f = h.drainUntil(t, "tool_result")
	_ = json.Unmarshal(f.raw, &tr)
	if tr.ID != "c3" || tr.Content[0].Text != "plain-ok" {
		t.Fatalf("plain tool after cancels = %+v", tr)
	}
}

func TestShutdownCancelsInteractiveHandlers(t *testing.T) {
	h := newInteractiveHarness(t)
	h.startRun(t)
	h.handshake(t)
	h.call(t, "c1")
	h.sendToExt(t, extproto.ShutdownFromHost{Type: "shutdown"})
	if err := h.awaitEnd(t); err != context.Canceled {
		t.Fatalf("handler ctx err on shutdown = %v", err)
	}
	h.drainUntil(t, "shutdown_ack")
}

func TestHostEOFCancelsInteractiveHandlers(t *testing.T) {
	h := newInteractiveHarness(t)
	runDone := make(chan error, 1)
	go func() { runDone <- h.ext.Run() }()
	h.handshake(t)
	h.call(t, "c1")
	if err := h.hostW.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.awaitEnd(t); err != context.Canceled {
		t.Fatalf("handler ctx err on EOF = %v", err)
	}
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after EOF")
	}
}

func TestPlainToolHandlersRemainSourceCompatible(t *testing.T) {
	h := newInteractiveHarness(t)
	h.startRun(t)
	h.handshake(t)
	h.sendToExt(t, extproto.ToolCallFromHost{Type: "tool_call", ID: "p1", Name: "plain", Args: json.RawMessage(`{}`)})
	f := h.drainUntil(t, "tool_result")
	var tr extproto.ToolResultFromExt
	_ = json.Unmarshal(f.raw, &tr)
	if tr.ID != "p1" || tr.IsError || tr.Content[0].Text != "plain-ok" {
		t.Fatalf("tool_result = %+v", tr)
	}
}
