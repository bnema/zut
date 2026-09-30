package extensions

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// interactiveHooks marks the host as able to show interactive tool prompts.
type interactiveHooks struct{ stubHooks }

func (*interactiveHooks) SupportsInteractiveTools() bool { return true }

// toolPeer wires a Manager to an in-process fake extension over io.Pipe
// pairs. The test plays the extension: it reads host frames from `frames`
// and replies through `replies`. The ordered transport is real, so write
// bounds, disconnects, and cancel frames exercise production code.
type toolPeer struct {
	m        *Manager
	ext      *Extension
	frames   chan []byte    // host -> extension frames, one per element
	replies  *io.PipeWriter // extension -> host (feeds readLoop)
	readDone chan struct{}
}

func newToolPeer(t *testing.T, hooks HostHooks, toolCancel bool) *toolPeer {
	return newToolPeerOpts(t, hooks, toolCancel, true)
}

// newToolPeerOpts builds the peer; drain=false leaves the host's stdin pipe
// unread so every host write blocks in the transport writer.
func newToolPeerOpts(t *testing.T, hooks HostHooks, toolCancel, drain bool) *toolPeer {
	t.Helper()
	m := New(t.TempDir(), "", "0.0.0-test", "", "", hooks)
	log, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	// Host stdin side: manager writes, test reads.
	stdinR, stdinW := io.Pipe()
	// Host stdout side: test writes, manager readLoop reads.
	stdoutR, stdoutW := io.Pipe()
	ext := &Extension{
		Manifest:         Manifest{Name: "peer"},
		readyCh:          make(chan struct{}),
		pending:          map[string]chan extproto.CommandResponseFromExt{},
		pendingTool:      map[string]chan extproto.ToolResultFromExt{},
		pendingIntercept: map[string]chan extproto.EventInterceptResponseFromExt{},
		eventSubs:        map[string]struct{}{},
		interceptSubs:    map[string]struct{}{},
		lifecycleFrames:  make(chan []byte, 64),
		lifecycleStop:    make(chan struct{}),
		logFile:          log,
		helloAck:         true,
		toolCancel:       toolCancel,
	}
	ext.stdin = newOrderedPipe(stdinW)
	m.mu.Lock()
	m.ext[ext.Manifest.Name] = ext
	m.mu.Unlock()

	p := &toolPeer{m: m, ext: ext, frames: make(chan []byte, 64), replies: stdoutW, readDone: make(chan struct{})}
	go func() {
		m.readLoop(ext, bufio.NewScanner(stdoutR))
		close(p.readDone)
	}()
	if drain {
		go func() {
			sc := bufio.NewScanner(stdinR)
			sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
			for sc.Scan() {
				p.frames <- append([]byte(nil), sc.Bytes()...)
			}
			close(p.frames)
		}()
	}
	t.Cleanup(func() {
		_ = stdoutW.Close()
		<-p.readDone
		_ = stdinR.Close()
		_ = log.Close()
	})
	return p
}

func (p *toolPeer) send(t *testing.T, frame any) {
	t.Helper()
	b, err := extproto.Encode(frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.replies.Write(b); err != nil {
		t.Fatal(err)
	}
}

func (p *toolPeer) register(t *testing.T, name string, interactive bool) {
	t.Helper()
	p.send(t, extproto.RegisterToolFromExt{Type: "register_tool", Name: name, Schema: json.RawMessage(`{"type":"object"}`), Interactive: interactive})
	p.send(t, extproto.ReadyFromExt{Type: "ready"})
	p.m.WaitForReady(2 * time.Second)
}

func (p *toolPeer) next(t *testing.T) extproto.Frame {
	t.Helper()
	select {
	case raw, ok := <-p.frames:
		if !ok {
			t.Fatal("host transport closed before expected frame")
		}
		var f extproto.Frame
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("no frame from host")
		return extproto.Frame{}
	}
}

func (p *toolPeer) expectToolCall(t *testing.T) extproto.Frame {
	t.Helper()
	f := p.next(t)
	if f.Type != "tool_call" {
		t.Fatalf("expected tool_call, got %+v", f)
	}
	return f
}

// expectClosedWithout drains until the host transport closes and fails if a
// frame of the given type was seen. Used to prove legacy peers never see
// tool_cancel and headless hosts never dispatch interactive calls.
func (p *toolPeer) expectClosedWithout(t *testing.T, frameType string) {
	t.Helper()
	for {
		select {
		case raw, ok := <-p.frames:
			if !ok {
				return
			}
			var f extproto.Frame
			_ = json.Unmarshal(raw, &f)
			if f.Type == frameType {
				t.Fatalf("host sent %s: %s", frameType, raw)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("host transport did not close")
		}
	}
}

func execTool(tool core.Tool, ctx context.Context) <-chan struct {
	res core.ToolResult
	err error
} {
	done := make(chan struct {
		res core.ToolResult
		err error
	}, 1)
	go func() {
		res, err := tool.Execute(ctx, nil, nil)
		done <- struct {
			res core.ToolResult
			err error
		}{res, err}
	}()
	return done
}

func resultText(res core.ToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

func TestInteractiveToolRegistrationAndTimeoutPolicy(t *testing.T) {
	p := newToolPeer(t, &interactiveHooks{}, true)
	p.send(t, extproto.RegisterToolFromExt{Type: "register_tool", Name: "ask", Schema: json.RawMessage(`{}`), Interactive: true})
	p.send(t, extproto.RegisterToolFromExt{Type: "register_tool", Name: "plain", Schema: json.RawMessage(`{}`)})
	p.send(t, extproto.ReadyFromExt{Type: "ready"})
	p.m.WaitForReady(2 * time.Second)

	infos := p.m.Tools()
	if len(infos) != 2 {
		t.Fatalf("tools = %+v", infos)
	}
	for _, info := range infos {
		tool := NewTool(p.m, info).(*extensionTool)
		switch info.Name {
		case "ask":
			if !info.Interactive || tool.timeout != 0 || !tool.Interactive() {
				t.Fatalf("interactive tool policy wrong: info=%+v timeout=%v", info, tool.timeout)
			}
		case "plain":
			if info.Interactive || tool.timeout != 60*time.Second || tool.Interactive() {
				t.Fatalf("normal tool policy wrong: info=%+v timeout=%v", info, tool.timeout)
			}
		}
	}
}

func TestInteractiveToolCompletesWithoutDeadline(t *testing.T) {
	p := newToolPeer(t, &interactiveHooks{}, true)
	p.register(t, "ask", true)
	tool := NewTool(p.m, p.m.Tools()[0])

	done := execTool(tool, context.Background())
	call := p.expectToolCall(t)
	p.send(t, extproto.ToolResultFromExt{Type: "tool_result", ID: call.ID, Content: []extproto.ContentBlock{{Type: "text", Text: "answer"}}})
	out := <-done
	if out.err != nil || out.res.IsError || resultText(out.res) != "answer" {
		t.Fatalf("unexpected result: %+v err=%v", out.res, out.err)
	}
	p.ext.mu.Lock()
	pending := len(p.ext.pendingTool)
	p.ext.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending slots leaked: %d", pending)
	}
}

func TestInteractiveToolResultSurvivesImmediateDisconnect(t *testing.T) {
	for range 30 {
		p := newToolPeer(t, &interactiveHooks{}, true)
		p.register(t, "ask", true)
		done := execTool(NewTool(p.m, p.m.Tools()[0]), context.Background())
		call := p.expectToolCall(t)
		p.send(t, extproto.ToolResultFromExt{Type: "tool_result", ID: call.ID, Content: []extproto.ContentBlock{{Type: "text", Text: "answer"}}})
		if err := p.replies.Close(); err != nil {
			t.Fatal(err)
		}
		<-p.readDone
		select {
		case out := <-done:
			if out.err != nil || out.res.IsError || resultText(out.res) != "answer" {
				t.Fatalf("completed reply lost to disconnect: result=%+v err=%v", out.res, out.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("tool call remained blocked after disconnect")
		}
	}
}

func TestInteractiveToolContextCancelSendsToolCancelAndIgnoresLateResult(t *testing.T) {
	p := newToolPeer(t, &interactiveHooks{}, true)
	p.register(t, "ask", true)
	tool := NewTool(p.m, p.m.Tools()[0])

	ctx, cancel := context.WithCancel(context.Background())
	done := execTool(tool, ctx)
	call := p.expectToolCall(t)
	cancel()
	out := <-done
	if out.err != nil || !out.res.IsError || !strings.Contains(resultText(out.res), "cancelled") {
		t.Fatalf("expected cancelled tool error, got %+v err=%v", out.res, out.err)
	}
	f := p.next(t)
	if f.Type != "tool_cancel" || f.ID != call.ID {
		t.Fatalf("expected tool_cancel for %s, got %+v", call.ID, f)
	}
	// A late reply for the abandoned id must be ignored and the pending map
	// must stay empty.
	p.send(t, extproto.ToolResultFromExt{Type: "tool_result", ID: call.ID})
	// Prove the read loop is still healthy by completing a fresh call.
	done = execTool(tool, context.Background())
	call2 := p.expectToolCall(t)
	if call2.ID == call.ID {
		t.Fatal("correlation ids repeated")
	}
	p.send(t, extproto.ToolResultFromExt{Type: "tool_result", ID: call2.ID, Content: []extproto.ContentBlock{{Type: "text", Text: "ok"}}})
	if out := <-done; out.err != nil || out.res.IsError {
		t.Fatalf("follow-up call failed: %+v err=%v", out.res, out.err)
	}
	p.ext.mu.Lock()
	pending := len(p.ext.pendingTool)
	p.ext.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending slots leaked: %d", pending)
	}
}

func TestInteractiveToolContextDeadlineEndsCall(t *testing.T) {
	p := newToolPeer(t, &interactiveHooks{}, true)
	p.register(t, "ask", true)
	tool := NewTool(p.m, p.m.Tools()[0])

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := execTool(tool, ctx)
	call := p.expectToolCall(t)
	out := <-done
	if out.err != nil || !out.res.IsError || !strings.Contains(resultText(out.res), "deadline") {
		t.Fatalf("expected deadline tool error, got %+v err=%v", out.res, out.err)
	}
	if f := p.next(t); f.Type != "tool_cancel" || f.ID != call.ID {
		t.Fatalf("expected tool_cancel, got %+v", f)
	}
}

func TestNormalToolReplyTimeoutSendsToolCancel(t *testing.T) {
	p := newToolPeer(t, &interactiveHooks{}, true)
	p.register(t, "plain", false)

	done := make(chan error, 1)
	go func() {
		_, err := p.m.InvokeTool(context.Background(), "plain", json.RawMessage(`{}`), 20*time.Millisecond)
		done <- err
	}()
	call := p.expectToolCall(t)
	err := <-done
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timeout waiting for peer/plain") {
		t.Fatalf("expected reply timeout, got %v", err)
	}
	if f := p.next(t); f.Type != "tool_cancel" || f.ID != call.ID {
		t.Fatalf("expected tool_cancel, got %+v", f)
	}
	// The wrapper reports a plain timeout as an ordinary tool error.
	tool := NewTool(p.m, p.m.Tools()[0]).(*extensionTool)
	tool.timeout = 20 * time.Millisecond
	res, execErr := tool.Execute(context.Background(), nil, nil)
	p.expectToolCall(t)
	if execErr != nil || !res.IsError || !strings.Contains(resultText(res), "failed: timeout") {
		t.Fatalf("expected timeout tool error, got %+v err=%v", res, execErr)
	}
}

func TestLegacyPeerNeverReceivesToolCancel(t *testing.T) {
	p := newToolPeer(t, &interactiveHooks{}, false)
	p.register(t, "ask", true)
	tool := NewTool(p.m, p.m.Tools()[0])

	ctx, cancel := context.WithCancel(context.Background())
	done := execTool(tool, ctx)
	p.expectToolCall(t)
	cancel()
	if out := <-done; out.err != nil || !out.res.IsError {
		t.Fatalf("expected cancelled tool error, got %+v err=%v", out.res, out.err)
	}
	// Legacy registration without the interactive field keeps the old
	// timeout, and cancellation never produces a tool_cancel frame.
	p.m.Stop(time.Second)
	p.expectClosedWithout(t, "tool_cancel")
}

func TestInteractiveToolDisconnectEndsCall(t *testing.T) {
	p := newToolPeer(t, &interactiveHooks{}, true)
	p.register(t, "ask", true)
	tool := NewTool(p.m, p.m.Tools()[0])

	done := execTool(tool, context.Background())
	p.expectToolCall(t)
	// Extension exits: its stdout closes, readLoop ends and tears the
	// transport down.
	_ = p.replies.Close()
	out := <-done
	if out.err != nil || !out.res.IsError || !strings.Contains(resultText(out.res), "disconnected") {
		t.Fatalf("expected disconnect tool error, got %+v err=%v", out.res, out.err)
	}
	if p.m.HasTool("ask") {
		t.Fatal("tool still registered after disconnect")
	}
}

func TestInteractiveToolShutdownEndsCall(t *testing.T) {
	p := newToolPeer(t, &interactiveHooks{}, true)
	p.register(t, "ask", true)
	tool := NewTool(p.m, p.m.Tools()[0])

	done := execTool(tool, context.Background())
	p.expectToolCall(t)
	stopped := make(chan struct{})
	go func() {
		p.m.Stop(200 * time.Millisecond)
		close(stopped)
	}()
	out := <-done
	if out.err != nil || !out.res.IsError {
		t.Fatalf("expected tool error on shutdown, got %+v err=%v", out.res, out.err)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop blocked on the pending interactive call")
	}
}

func TestInteractiveToolBlockedWriteIsBoundedAndCancellable(t *testing.T) {
	// The extension never reads stdin, so the tool_call write blocks in the
	// transport writer. Context cancellation must return promptly instead of
	// waiting on the (deadline-free) reply, and must not pin goroutines.
	t.Run("cancel", func(t *testing.T) {
		p := newToolPeerOpts(t, &interactiveHooks{}, true, false)
		p.register(t, "ask", true)
		tool := NewTool(p.m, p.m.Tools()[0])
		ctx, cancel := context.WithCancel(context.Background())
		done := execTool(tool, ctx)
		// Wait for the frame to reach the writer goroutine (blocked on the
		// unread pipe), then cancel.
		pipe := p.ext.stdin.(*orderedPipe)
		for {
			pipe.mu.Lock()
			queued := len(pipe.queue)
			pipe.mu.Unlock()
			if queued == 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
		select {
		case out := <-done:
			if out.err != nil || !out.res.IsError {
				t.Fatalf("expected tool error, got %+v err=%v", out.res, out.err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("cancelled call stayed blocked on the transport write")
		}
	})
	// Without cancellation the write itself is still bounded, independent of
	// the absent reply deadline: the transport times out and disconnects.
	t.Run("timeout", func(t *testing.T) {
		p := newToolPeerOpts(t, &interactiveHooks{}, true, false)
		p.register(t, "ask", true)
		pipe := p.ext.stdin.(*orderedPipe)
		done := make(chan error, 1)
		go func() {
			_, err := pipe.writeContext(context.Background(), []byte("{}\n"), 30*time.Millisecond)
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected transport timeout, got %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("blocked write not bounded")
		}
		// A subsequent tool call fails immediately on the dead transport.
		tool := NewTool(p.m, p.m.Tools()[0])
		res, err := tool.Execute(context.Background(), nil, nil)
		if err != nil || !res.IsError || !strings.Contains(resultText(res), "write:") {
			t.Fatalf("expected write failure, got %+v err=%v", res, err)
		}
	})
}

func TestOrderedPipeWriteTimeoutDisconnects(t *testing.T) {
	r, w := io.Pipe()
	pipe := newOrderedPipe(w)
	t.Cleanup(func() { _ = r.Close() })
	// Nobody reads r, so the first write blocks in the writer goroutine and
	// the second waits in the queue.
	first := make(chan error, 1)
	go func() {
		_, err := pipe.writeContext(context.Background(), []byte("a\n"), 30*time.Millisecond)
		first <- err
	}()
	select {
	case err := <-first:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected timeout, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bounded write did not return")
	}
	select {
	case <-pipe.Done():
	case <-time.After(time.Second):
		t.Fatal("transport not disconnected after write timeout")
	}
	if _, err := pipe.Write([]byte("b\n")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write after disconnect = %v", err)
	}
}

func TestOrderedPipeCancelWithdrawsQueuedFrameWithoutDisconnect(t *testing.T) {
	r, w := io.Pipe()
	pipe := newOrderedPipe(w)
	t.Cleanup(func() { pipe.Close(); _ = r.Close() })

	// Head frame blocks in the writer; the second frame stays queued.
	headAck := make(chan error, 1)
	if _, err := pipe.enqueue([]byte("head\n"), headAck); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() {
		_, err := pipe.writeContext(ctx, []byte("second\n"), time.Minute)
		second <- err
	}()
	cancel()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancel, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued write did not return on cancel")
	}
	select {
	case <-pipe.Done():
		t.Fatal("cancel of a queued frame must not disconnect the transport")
	default:
	}
	// Reader drains: only the head frame is delivered, the withdrawn one is
	// not, and the transport keeps working.
	go func() {
		_, _ = pipe.Write([]byte("third\n"))
	}()
	sc := bufio.NewScanner(r)
	var got []string
	for len(got) < 2 && sc.Scan() {
		got = append(got, sc.Text())
	}
	if strings.Join(got, ",") != "head,third" {
		t.Fatalf("delivered frames = %v", got)
	}
}

func TestOrderedPipeOverflowDisconnects(t *testing.T) {
	r, w := io.Pipe()
	pipe := newOrderedPipe(w)
	t.Cleanup(func() { _ = r.Close() })
	var err error
	for i := 0; i <= outboundFrames+1; i++ {
		if _, err = pipe.enqueue([]byte("x\n"), nil); err != nil {
			break
		}
	}
	if !errors.Is(err, errOutboundOverflow) {
		t.Fatalf("expected overflow, got %v", err)
	}
	select {
	case <-pipe.Done():
	case <-time.After(time.Second):
		t.Fatal("overflow did not disconnect")
	}
}

func TestCorrelationIDsAreUniqueUnderConcurrency(t *testing.T) {
	const n = 2000
	var mu sync.Mutex
	seen := make(map[string]struct{}, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := newCorrelationID()
			mu.Lock()
			seen[id] = struct{}{}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("got %d unique ids for %d calls", len(seen), n)
	}
}

func TestHeadlessHostsRejectInteractiveTools(t *testing.T) {
	cases := map[string]HostHooks{
		"missing_method": &stubHooks{},
		"returns_false":  &interactiveOptOut{},
	}
	for name, hooks := range cases {
		t.Run(name, func(t *testing.T) {
			p := newToolPeer(t, hooks, true)
			p.register(t, "ask", true)
			tool := NewTool(p.m, p.m.Tools()[0])
			res, err := tool.Execute(context.Background(), nil, nil)
			if err != nil || !res.IsError || !strings.Contains(resultText(res), "requires an interactive host") {
				t.Fatalf("expected fail-closed rejection, got %+v err=%v", res, err)
			}
			// Nothing was sent to the extension.
			p.m.Stop(time.Second)
			p.expectClosedWithout(t, "tool_call")
		})
	}
	// A non-interactive tool still works on a headless host.
	p := newToolPeer(t, &stubHooks{}, true)
	p.register(t, "plain", false)
	tool := NewTool(p.m, p.m.Tools()[0])
	done := execTool(tool, context.Background())
	call := p.expectToolCall(t)
	p.send(t, extproto.ToolResultFromExt{Type: "tool_result", ID: call.ID, Content: []extproto.ContentBlock{{Type: "text", Text: "ok"}}})
	if out := <-done; out.err != nil || out.res.IsError {
		t.Fatalf("normal tool on headless host failed: %+v err=%v", out.res, out.err)
	}
}

type interactiveOptOut struct{ stubHooks }

func (*interactiveOptOut) SupportsInteractiveTools() bool { return false }

func TestIsInteractiveToolClassifiesExtensionTools(t *testing.T) {
	m := New(t.TempDir(), "", "0.0.0-test", "", "", &stubHooks{})
	if !IsInteractiveTool(NewTool(m, ToolInfo{Name: "ask", Interactive: true})) {
		t.Fatal("interactive tool not classified")
	}
	if IsInteractiveTool(NewTool(m, ToolInfo{Name: "plain"})) {
		t.Fatal("plain tool classified as interactive")
	}
	if IsInteractiveTool(nil) {
		t.Fatal("nil classified as interactive")
	}
}
