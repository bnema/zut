package core

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/bnema/zut/packages/provider"
)

type invokeHooks struct {
	started  func()
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
		hooks.started()
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
type NestedInvoker interface {
	Tools() Registry
	Invoke(ctx context.Context, name string, args json.RawMessage) NestedOutcome
}

type nestedInvokerKey struct{}

func withNestedInvoker(ctx context.Context, inv NestedInvoker) context.Context {
	return context.WithValue(ctx, nestedInvokerKey{}, inv)
}

func NestedInvokerFrom(ctx context.Context) (NestedInvoker, bool) {
	inv, ok := ctx.Value(nestedInvokerKey{}).(NestedInvoker)
	return inv, ok && inv != nil
}

type nestedInvoker struct {
	agent   *Agent
	tools   Registry
	parent  provider.ToolCallBlock
	counter atomic.Uint64
}

func (n *nestedInvoker) Tools() Registry { return n.tools }

func (n *nestedInvoker) Invoke(ctx context.Context, name string, args json.RawMessage) NestedOutcome {
	id := n.parent.ID + "/" + strconv.FormatUint(n.counter.Add(1), 10)
	call := provider.ToolCallBlock{ID: id, Name: name, Arguments: args, Origin: n.parent.Origin}
	result, denied := n.agent.invokeTool(withNestedInvoker(ctx, nil), call, n.tools, invokeHooks{})
	if denied {
		n.agent.markDeniedToolCall()
	}
	return NestedOutcome{ID: id, Result: result, Denied: denied}
}
