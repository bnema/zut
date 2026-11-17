package core

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bnema/zut/packages/provider"
)

type invokeHooks struct {
	started  func(json.RawMessage)
	progress func(string)
}

func (a *Agent) invokeTool(ctx context.Context, tc provider.ToolCallBlock, tools Registry, hooks invokeHooks) (res ToolResult, denied bool) {
	started := time.Now()
	defer func() {
		if recover() != nil {
			res = ToolResult{
				Content: []provider.Content{provider.TextBlock{Text: "tool execution failed"}},
				IsError: true,
			}
		}
		res.Timing = &provider.ToolTiming{
			StartedAt:   started,
			CompletedAt: time.Now(),
			Duration:    time.Since(started),
		}
	}()

	tool, err := tools.Get(tc.Name)
	if err != nil {
		return ToolResult{
			Content: []provider.Content{provider.TextBlock{Text: err.Error()}},
			IsError: true,
		}, denied
	}

	args := tc.Arguments

	// Intercept hook: an extension or other guard can refuse the
	// call before any side effect happens, OR rewrite the args
	// seen by the tool. The model sees the reason as the tool
	// error, learns from it, and (typically) proposes a different
	// action; rewrites are invisible to the model (they apply only
	// to the execution).
	if a.BeforeToolExecute != nil {
		allowed, reason, modified := a.BeforeToolExecute(tc)
		if !allowed {
			// A guard or confirmation denial is tracked separately from an
			// ordinary tool error: a denied invocation must not gain a
			// recovery prompt urging more actions.
			a.markDeniedToolCall()
			denied = true
			if reason == "" {
				reason = "tool call refused by extension guard"
			}
			return ToolResult{
				Content: []provider.Content{provider.TextBlock{Text: reason}},
				IsError: true,
			}, denied
		}
		if len(modified) > 0 && json.Valid(modified) {
			if policy, ok := tool.(ToolArgumentRewritePolicy); ok && !policy.AllowArgumentRewrite() {
				return ToolResult{
					Content: []provider.Content{provider.TextBlock{Text: "tool arguments cannot be rewritten"}},
					IsError: true,
				}, true
			}
			args = modified
		}
	}

	if len(args) == 0 {
		args = json.RawMessage("{}")
	}

	// Tool panics are recovered by the invocation-level defer above so
	// guard, lookup, progress, and Execute panics all receive timing.
	if hooks.started != nil {
		hooks.started(args)
	}
	out, err := tool.Execute(ctx, args, func(text string) {
		if hooks.progress != nil {
			hooks.progress(text)
		}
	})
	if err != nil {
		// A refused execution (permission scope, allowlist, jail, or
		// admission gate) is tracked separately from an ordinary tool
		// error: like a guard or confirmation denial, it must not gain
		// a recovery prompt urging more actions.
		if asToolDeniedError(err) {
			a.markDeniedToolCall()
			denied = true
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ToolResult{
				Content: []provider.Content{provider.TextBlock{Text: "aborted: " + err.Error()}},
				IsError: true,
			}, denied
		}
		return ToolResult{
			Content: []provider.Content{provider.TextBlock{Text: err.Error()}},
			IsError: true,
		}, denied
	}
	return out, denied
}

// NestedOutcome describes an invocation without committing its result to history.
type NestedOutcome struct {
	ID     string
	Result ToolResult
	Denied bool
}

// NestedInvoker invokes tools without committing nested results. Every nested call
// goes through BeforeToolExecute (guards, confirmation, and rewrite policy) and
// denied tracking.
// Callers must not retain the invoker beyond the parent Execute call.
type NestedInvoker interface {
	Tools() Registry
	Invoke(ctx context.Context, name string, args json.RawMessage) NestedOutcome
}

type nestedInvokerKey struct{}

type toolCallIDKey struct{}

func withToolCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, toolCallIDKey{}, id)
}

// ToolCallIDFrom returns the provider call ID of the top-level tool call whose
// Execute received ctx. Nested (codemode) calls see the parent call's ID.
func ToolCallIDFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(toolCallIDKey{}).(string)
	return id, ok && id != ""
}

func withNestedInvoker(ctx context.Context, inv NestedInvoker) context.Context {
	return context.WithValue(ctx, nestedInvokerKey{}, inv)
}

func NestedInvokerFrom(ctx context.Context) (NestedInvoker, bool) {
	inv, ok := ctx.Value(nestedInvokerKey{}).(NestedInvoker)
	return inv, ok && inv != nil
}

type nestedInvoker struct {
	agent      *Agent
	tools      Registry
	parent     provider.ToolCallBlock
	counter    atomic.Uint64
	sink       func(AgentEvent)
	lifetimeMu sync.Mutex
	closed     bool
	inflight   sync.WaitGroup
}

// toolEventDispatcher serializes delivery without holding a lock across callbacks.
type toolEventDispatcher struct {
	mu                   sync.Mutex
	cond                 *sync.Cond
	queue                []AgentEvent
	draining             bool
	pendingProgressBytes int
	sink                 func(AgentEvent)
}

func newToolEventDispatcher(sink func(AgentEvent)) *toolEventDispatcher {
	d := &toolEventDispatcher{sink: sink}
	d.cond = sync.NewCond(&d.mu)
	return d
}
func (d *toolEventDispatcher) emit(ev AgentEvent) {
	if p, ok := ev.(EvToolProgress); ok && p.Text == "" {
		return
	}
	d.mu.Lock()
	d.queue = append(d.queue, ev)
	if progress, ok := ev.(EvToolProgress); ok {
		d.pendingProgressBytes += len(progress.Text)
		if d.pendingProgressBytes > maxPendingToolProgressBytes {
			d.boundProgressLocked()
		}
	}
	if d.draining {
		d.mu.Unlock()
		return
	}
	d.draining = true
	d.drainLocked()
}

const maxPendingToolProgressBytes = 256 * 1024
const toolProgressTruncated = "…[progress truncated]"

// boundProgressLocked coalesces only adjacent progress for the same call, then
// evicts oldest progress text. Lifecycle events keep their relative positions.
// A single marker precedes the surviving progress, within the byte budget.
func (d *toolEventDispatcher) boundProgressLocked() {
	out := d.queue[:0]
	for i := 0; i < len(d.queue); {
		event := d.queue[i]
		p, ok := event.(EvToolProgress)
		if !ok {
			out = append(out, event)
			i++
			continue
		}
		// Identify a run before writing into the queue's reused backing array.
		end, size := i, 0
		for end < len(d.queue) {
			next, ok := d.queue[end].(EvToolProgress)
			if !ok || next.ID != p.ID {
				break
			}
			size += len(strings.TrimPrefix(next.Text, toolProgressTruncated))
			end++
		}
		var text strings.Builder
		text.Grow(size)
		for j := i; j < end; j++ {
			text.WriteString(strings.TrimPrefix(d.queue[j].(EvToolProgress).Text, toolProgressTruncated))
		}
		p.Text = text.String()
		out = append(out, p)
		i = end
	}
	clear(d.queue[len(out):])
	d.queue = out
	total := 0
	for _, event := range out {
		if p, ok := event.(EvToolProgress); ok {
			total += len(p.Text)
		}
	}
	drop := total - (maxPendingToolProgressBytes - len(toolProgressTruncated))
	out = d.queue[:0]
	marked := false
	d.pendingProgressBytes = 0
	for _, event := range d.queue {
		if p, ok := event.(EvToolProgress); ok {
			if drop >= len(p.Text) {
				drop -= len(p.Text)
				continue
			}
			if drop > 0 {
				cut := drop
				for cut < len(p.Text) && !utf8.RuneStart(p.Text[cut]) {
					cut++
				}
				p.Text = p.Text[cut:]
				drop = 0
			}
			if !marked {
				p.Text = toolProgressTruncated + p.Text
				marked = true
			}
			// Clone so a short suffix cannot retain a large discarded backing string.
			p.Text = strings.Clone(p.Text)
			d.pendingProgressBytes += len(p.Text)
			event = p
		}
		out = append(out, event)
	}
	clear(d.queue[len(out):])
	d.queue = out
}

// drainLocked takes ownership of the lock and releases it before delivery.
func (d *toolEventDispatcher) drainLocked() {
	for {
		if len(d.queue) == 0 {
			d.queue = nil
			d.draining = false
			d.cond.Broadcast()
			d.mu.Unlock()
			return
		}
		ev := d.queue[0]
		if progress, ok := ev.(EvToolProgress); ok {
			d.pendingProgressBytes -= len(progress.Text)
		}
		d.queue[0] = nil
		d.queue = d.queue[1:]
		d.mu.Unlock()
		d.deliver(ev)
		d.mu.Lock()
	}
}
func (d *toolEventDispatcher) deliver(ev AgentEvent) {
	completed := false
	defer func() {
		if !completed {
			d.mu.Lock()
			d.draining = false
			d.cond.Broadcast()
			d.mu.Unlock()
			// The original panic continues; queued events remain for emit or flush.
		}
	}()
	if d.sink != nil {
		d.sink(ev)
	}
	completed = true
}

// flush waits for delivery to finish. Never call it from inside a sink callback.
func (d *toolEventDispatcher) flush() {
	d.mu.Lock()
	for d.draining {
		d.cond.Wait()
	}
	if len(d.queue) == 0 {
		d.mu.Unlock()
		return
	}
	d.draining = true
	d.drainLocked()
}
func (n *nestedInvoker) close() {
	n.lifetimeMu.Lock()
	n.closed = true
	n.lifetimeMu.Unlock()
	n.inflight.Wait()
}

func nestedSummary(content []provider.Content) string {
	var text strings.Builder
	for _, c := range content {
		if b, ok := c.(provider.TextBlock); ok {
			remaining := 200 + utf8.UTFMax - text.Len()
			if len(b.Text) > remaining {
				text.WriteString(b.Text[:remaining])
			} else {
				text.WriteString(b.Text)
			}
			if text.Len() >= 200+utf8.UTFMax {
				break
			}
		}
	}
	s := text.String()
	if len(s) <= 200 {
		return strings.Clone(s)
	}
	end := 200
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return strings.Clone(s[:end])
}

func (n *nestedInvoker) Tools() Registry { return n.tools }

func (n *nestedInvoker) Invoke(ctx context.Context, name string, args json.RawMessage) NestedOutcome {
	n.lifetimeMu.Lock()
	if n.closed {
		n.lifetimeMu.Unlock()
		return NestedOutcome{Result: ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: "nested tool calls are no longer available: parent tool finished"}}}}
	}
	n.inflight.Add(1)
	n.lifetimeMu.Unlock()
	defer n.inflight.Done()
	id := n.parent.ID + "/" + strconv.FormatUint(n.counter.Add(1), 10)
	call := provider.ToolCallBlock{ID: id, Name: name, Arguments: args, Origin: n.parent.Origin}
	result, denied := n.agent.invokeTool(withNestedInvoker(ctx, nil), call, n.tools, invokeHooks{
		// These are the normalized arguments after guard rewrites and empty -> {}.
		started: func(executed json.RawMessage) {
			if n.sink != nil {
				n.sink(EvNestedToolStarted{ID: id, ParentID: n.parent.ID, Name: name, Args: append(json.RawMessage(nil), executed...)})
			}
		},
	})
	if denied {
		n.agent.markDeniedToolCall()
	}
	n.agent.foldToolUsage(&result, n.sink)
	if n.sink != nil {
		n.sink(EvNestedToolResult{ID: id, ParentID: n.parent.ID, Name: name, IsError: result.IsError, Denied: denied, Summary: nestedSummary(result.Content)})
	}
	return NestedOutcome{ID: id, Result: result, Denied: denied}
}
