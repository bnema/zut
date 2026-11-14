package core

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"unicode/utf8"

	"github.com/bnema/zut/packages/provider"
)

type invokingTestTool struct {
	name            string
	execute         func(context.Context) ToolResult
	immutable       bool
	progress        func(func(string))
	executeProgress func(context.Context, func(string)) ToolResult
}

func (t *invokingTestTool) Name() string               { return t.name }
func (*invokingTestTool) Description() string          { return "nested invocation test" }
func (*invokingTestTool) Schema() json.RawMessage      { return json.RawMessage(`{"type":"object"}`) }
func (t *invokingTestTool) AllowArgumentRewrite() bool { return !t.immutable }
func (t *invokingTestTool) Execute(ctx context.Context, _ json.RawMessage, progress func(string)) (ToolResult, error) {
	if t.progress != nil {
		t.progress(progress)
	}
	if t.executeProgress != nil {
		return t.executeProgress(ctx, progress), nil
	}
	return t.execute(ctx), nil
}

func TestInvokeToolReportsDenied(t *testing.T) {
	a := NewAgent(nil, "test", "", Registry{"echo": &recordingTool{}})
	a.BeforeToolExecute = func(provider.ToolCallBlock) (bool, string, json.RawMessage) { return false, "guard reason", nil }
	res, denied := a.invokeTool(context.Background(), provider.ToolCallBlock{Name: "echo"}, a.ToolsSnapshot(), invokeHooks{})
	if !denied || !res.IsError || res.Content[0].(provider.TextBlock).Text != "guard reason" {
		t.Fatalf("result=%#v denied=%v", res, denied)
	}
	assertToolTiming(t, res.Timing)
}

func TestNestedInvokerAgentTurn(t *testing.T) {
	for _, mode := range []string{"allow", "deny", "rewrite"} {
		t.Run(mode, func(t *testing.T) {
			var outcome NestedOutcome
			var childRan bool
			child := &invokingTestTool{name: "child", immutable: true, execute: func(ctx context.Context) ToolResult {
				childRan = true
				if _, ok := NestedInvokerFrom(ctx); ok {
					t.Error("nested tool received invoker")
				}
				return ToolResult{}
			}}
			parent := &invokingTestTool{name: "parent", execute: func(ctx context.Context) ToolResult {
				inv, ok := NestedInvokerFrom(ctx)
				if !ok {
					t.Fatal("parent missing invoker")
				}
				if _, err := inv.Tools().Get("child"); err != nil {
					t.Fatal(err)
				}
				outcome = inv.Invoke(ctx, "child", nil)
				return ToolResult{Content: []provider.Content{provider.TextBlock{Text: "parent done"}}}
			}}
			client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
				{content: []provider.Content{provider.ToolCallBlock{ID: "parent-id", Name: "parent", Origin: "test-origin"}}, stop: provider.StopToolUse},
				{content: []provider.Content{textContent("done", "")}, stop: provider.StopEnd},
			}}
			a := NewAgent(client, "test", "", Registry{"parent": parent, "child": child})
			var calls []provider.ToolCallBlock
			a.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
				calls = append(calls, call)
				if call.Name == "child" {
					if mode == "deny" {
						return false, "nested denied", nil
					}
					if mode == "rewrite" {
						return true, "", json.RawMessage(`{"changed":true}`)
					}
				}
				return true, "", nil
			}
			var events []AgentEvent
			if err := runPrompt(t, a, &events); err != nil {
				t.Fatal(err)
			}
			if len(calls) != 2 || calls[1].ID != "parent-id/1" || calls[1].Origin != "test-origin" {
				t.Fatalf("calls=%#v", calls)
			}
			if outcome.ID != "parent-id/1" || outcome.Denied != (mode != "allow") || outcome.Result.IsError != (mode != "allow") || childRan != (mode == "allow") {
				t.Fatalf("outcome=%#v childRan=%v", outcome, childRan)
			}
			if mode == "rewrite" && outcome.Result.Content[0].(provider.TextBlock).Text != "tool arguments cannot be rewritten" {
				t.Fatalf("rewrite result=%#v", outcome)
			}
			var order []string
			for _, ev := range events {
				switch e := ev.(type) {
				case EvToolExecutionStarted, EvToolResult:
					order = append(order, ev.Type())
				case EvNestedToolStarted:
					order = append(order, ev.Type())
					if e.ID != outcome.ID || e.ParentID != "parent-id" || e.Name != "child" || string(e.Args) != "{}" {
						t.Errorf("started=%#v", e)
					}
				case EvNestedToolResult:
					order = append(order, ev.Type())
					if e.ID != outcome.ID || e.ParentID != "parent-id" || e.Name != "child" || e.Denied != outcome.Denied || e.IsError != outcome.Result.IsError {
						t.Errorf("result=%#v", e)
					}
				}
			}
			want := []string{"tool_execution_started", "nested_tool_result", "tool_result"}
			if mode == "allow" {
				want = []string{"tool_execution_started", "nested_tool_started", "nested_tool_result", "tool_result"}
			}
			if !reflect.DeepEqual(order, want) {
				t.Fatalf("order=%v want=%v", order, want)
			}
			results := 0
			for _, ev := range events {
				if e, ok := ev.(EvToolResult); ok {
					results++
					if e.ID != "parent-id" {
						t.Errorf("nested result event: %#v", e)
					}
				}
			}
			if results != 1 {
				t.Fatalf("result events=%d", results)
			}
			blocks := 0
			for _, msg := range a.Messages() {
				for _, c := range msg.Content {
					if b, ok := c.(provider.ToolResultBlock); ok {
						blocks++
						if b.CallID != "parent-id" {
							t.Errorf("nested transcript result: %#v", b)
						}
					}
				}
			}
			if blocks != 1 {
				t.Fatalf("transcript result blocks=%d", blocks)
			}
		})
	}
}

func TestNestedInvokerContext(t *testing.T) {
	ctx := context.Background()
	if _, ok := NestedInvokerFrom(ctx); ok {
		t.Fatal("unexpected invoker")
	}
	ctx = withNestedInvoker(ctx, &nestedInvoker{})
	if _, ok := NestedInvokerFrom(ctx); !ok {
		t.Fatal("missing invoker")
	}
	if _, ok := NestedInvokerFrom(withNestedInvoker(ctx, nil)); ok {
		t.Fatal("nil did not strip invoker")
	}
}

func TestNestedInvokerConcurrent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const count = 16
		a := NewAgent(nil, "test", "", Registry{"echo": &resultTool{}})
		var mu sync.Mutex
		seen := make(map[string]bool)
		a.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
			mu.Lock()
			defer mu.Unlock()
			if seen[call.ID] {
				t.Errorf("duplicate ID %q", call.ID)
			}
			seen[call.ID] = true
			return call.ID != "parent/1", "denied", nil
		}
		var events []AgentEvent
		inv := &nestedInvoker{agent: a, tools: a.ToolsSnapshot(), parent: provider.ToolCallBlock{ID: "parent"}, sink: newToolEventDispatcher(func(ev AgentEvent) { events = append(events, ev) }).emit}
		start := make(chan struct{})
		var ready, done sync.WaitGroup
		ready.Add(count)
		done.Add(count)
		var outcomes []NestedOutcome
		for i := 0; i < count; i++ {
			go func() {
				defer done.Done()
				ready.Done()
				<-start
				out := inv.Invoke(context.Background(), "echo", nil)
				mu.Lock()
				outcomes = append(outcomes, out)
				mu.Unlock()
			}()
		}
		ready.Wait()
		close(start)
		done.Wait()
		if len(events) != count*2-1 {
			t.Fatalf("events=%d", len(events))
		}
		if len(seen) != count || len(outcomes) != count {
			t.Fatalf("calls=%d outcomes=%d", len(seen), len(outcomes))
		}
		for i := 1; i <= count; i++ {
			if !seen[fmt.Sprintf("parent/%d", i)] {
				t.Errorf("missing ID parent/%d", i)
			}
		}
		denied := 0
		for _, out := range outcomes {
			if out.Denied {
				denied++
				if out.ID != "parent/1" || !out.Result.IsError {
					t.Errorf("unexpected denial: %#v", out)
				}
			}
		}
		if denied != 1 || !a.sawDeniedToolCall() {
			t.Fatalf("denials=%d tracked=%v", denied, a.sawDeniedToolCall())
		}
	})
}

func TestNestedInvokerDoesNotCommitMetadata(t *testing.T) {
	op := PlanOperation{Action: "set", Steps: []PlanStep{{Step: "nested plan", Status: PlanInProgress}}}
	child := &invokingTestTool{name: "child", execute: func(context.Context) ToolResult {
		return ToolResult{ActivateTools: []string{"extra"}, Details: op, PlanOperation: &op}
	}}
	parent := &invokingTestTool{name: "parent", execute: func(ctx context.Context) ToolResult {
		inv, ok := NestedInvokerFrom(ctx)
		if !ok {
			t.Fatal("missing invoker")
		}
		out := inv.Invoke(ctx, "child", nil)
		if out.Result.IsError || len(out.Result.ActivateTools) != 1 || out.Result.PlanOperation == nil {
			t.Fatalf("nested metadata lost: %#v", out)
		}
		return ToolResult{}
	}}
	a := NewAgent(nil, "test", "", Registry{"parent": parent, "child": child, "extra": &resultTool{}})
	commits, observations := 0, 0
	a.CommitToolResult = func(id string, res ToolResult) error {
		commits++
		if id != "parent" || len(res.ActivateTools) != 0 || res.PlanOperation != nil {
			t.Errorf("unexpected commit %q: %#v", id, res)
		}
		return nil
	}
	a.OnToolResult = func(id string, res ToolResult) {
		observations++
		if id != "parent" || len(res.ActivateTools) != 0 || res.PlanOperation != nil {
			t.Errorf("unexpected observation %q: %#v", id, res)
		}
	}
	msg, failed := a.executeTools(context.Background(), provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "parent", Name: "parent"}}}, func(AgentEvent) {})
	if failed || commits != 1 || observations != 1 {
		t.Fatalf("failed=%v commits=%d observations=%d", failed, commits, observations)
	}
	if len(msg.AddedToolNames) != 0 || len(a.CurrentPlan()) != 0 {
		t.Fatalf("nested metadata applied: tools=%v plan=%v", msg.AddedToolNames, a.CurrentPlan())
	}
}

func TestTopLevelRewriteRefusalDoesNotTrackDenial(t *testing.T) {
	a := NewAgent(nil, "test", "", Registry{"result": &immutableResultTool{}})
	a.BeforeToolExecute = func(provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return true, "", json.RawMessage(`{"changed":true}`)
	}
	res := a.runOneTool(context.Background(), provider.ToolCallBlock{ID: "top", Name: "result"}, a.ToolsSnapshot(), func(AgentEvent) {})
	if !res.IsError || a.sawDeniedToolCall() {
		t.Fatalf("result=%#v tracked=%v", res, a.sawDeniedToolCall())
	}
}

func TestNestedInvokerSummaryAndRewrittenArgs(t *testing.T) {
	for _, prefix := range []string{strings.Repeat("a", 200), strings.Repeat("a", 199), strings.Repeat("a", 198)} {
		t.Run(fmt.Sprint(len(prefix)), func(t *testing.T) {
			child := &invokingTestTool{name: "child", execute: func(context.Context) ToolResult {
				return ToolResult{Content: []provider.Content{provider.TextBlock{Text: prefix}, provider.TextBlock{Text: "界tail"}}}
			}}
			a := NewAgent(nil, "test", "", Registry{"child": child})
			a.BeforeToolExecute = func(provider.ToolCallBlock) (bool, string, json.RawMessage) {
				return true, "", json.RawMessage(`{"rewritten":true}`)
			}
			var events []AgentEvent
			inv := &nestedInvoker{agent: a, tools: a.ToolsSnapshot(), parent: provider.ToolCallBlock{ID: "parent"}, sink: newToolEventDispatcher(func(ev AgentEvent) { events = append(events, ev) }).emit}
			inv.Invoke(context.Background(), "child", nil)
			if len(events) != 2 {
				t.Fatalf("events=%v", events)
			}
			if got := string(events[0].(EvNestedToolStarted).Args); got != `{"rewritten":true}` {
				t.Fatal(got)
			}
			summary := events[1].(EvNestedToolResult).Summary
			if summary != prefix || !utf8.ValidString(summary) || len(summary) > 200 {
				t.Fatalf("summary=%q", summary)
			}
		})
	}
}

func TestNestedInvokerConcurrentParentProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var workers sync.WaitGroup
		start := make(chan struct{})
		child := &invokingTestTool{name: "child", execute: func(context.Context) ToolResult { return ToolResult{} }, progress: func(progress func(string)) { progress("not emitted") }}
		parent := &invokingTestTool{name: "parent", progress: func(progress func(string)) {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				for j := 0; j < 32; j++ {
					progress("parent")
				}
			}()
		}, execute: func(ctx context.Context) ToolResult {
			inv, _ := NestedInvokerFrom(ctx)
			for j := 0; j < 16; j++ {
				workers.Add(1)
				go func() { defer workers.Done(); <-start; inv.Invoke(ctx, "child", nil) }()
			}
			close(start)
			workers.Wait()
			return ToolResult{}
		}}
		a := NewAgent(nil, "test", "", Registry{"parent": parent, "child": child})
		var events []AgentEvent
		a.executeTools(context.Background(), provider.Message{Content: []provider.Content{provider.ToolCallBlock{ID: "p", Name: "parent"}}}, func(ev AgentEvent) { events = append(events, ev) })
		starts, results, progress := 0, 0, 0
		for _, ev := range events {
			switch e := ev.(type) {
			case EvNestedToolStarted:
				starts++
			case EvNestedToolResult:
				results++
			case EvToolProgress:
				progress++
				if e.ID != "p" {
					t.Errorf("nested progress: %#v", e)
				}
			}
		}
		if starts != 16 || results != 16 || progress != 32 {
			t.Fatalf("starts=%d results=%d progress=%d", starts, results, progress)
		}
		if events[0].Type() != "tool_execution_started" || events[len(events)-1].Type() != "tool_result" {
			t.Fatal("parent event order")
		}
	})
}

func TestNestedInvokerReentrantProgressObserver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var inv NestedInvoker
		parent := &invokingTestTool{name: "parent", executeProgress: func(ctx context.Context, progress func(string)) ToolResult {
			inv, _ = NestedInvokerFrom(ctx)
			progress("progress")
			return ToolResult{}
		}}
		a := NewAgent(nil, "test", "", Registry{"parent": parent, "child": &resultTool{}})
		done := make(chan []string, 1)
		go func() {
			var order []string
			observer := func(ev AgentEvent) {
				order = append(order, ev.Type())
				if _, ok := ev.(EvToolProgress); ok {
					inv.Invoke(context.Background(), "child", nil)
				}
			}
			a.executeTools(context.Background(), provider.Message{Content: []provider.Content{provider.ToolCallBlock{ID: "p", Name: "parent"}}}, observer)
			done <- order
		}()
		order := <-done
		{
			want := []string{"tool_execution_started", "tool_progress", "nested_tool_started", "nested_tool_result", "tool_result"}
			if !reflect.DeepEqual(order, want) {
				t.Fatalf("order=%v", order)
			}
		}
	})
}

func TestNestedSummaryLargeOutput(t *testing.T) {
	content := []provider.Content{provider.TextBlock{Text: strings.Repeat("界", 1<<20)}, provider.TextBlock{Text: "ignored"}}
	got := nestedSummary(content)
	if len(got) != 198 || !utf8.ValidString(got) {
		t.Fatalf("summary bytes=%d", len(got))
	}
}

func TestToolDispatcherRecoversAfterObserverPanic(t *testing.T) {
	for _, kind := range []string{"tool_execution_started", "tool_progress"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent := &invokingTestTool{name: "parent", executeProgress: func(_ context.Context, progress func(string)) ToolResult { progress("hello"); return ToolResult{} }}
				a := NewAgent(nil, "test", "", Registry{"parent": parent})
				done := make(chan []string, 1)
				go func() {
					var events []string
					panicked := false
					a.executeTools(context.Background(), provider.Message{Content: []provider.Content{provider.ToolCallBlock{ID: "p", Name: "parent"}}}, func(ev AgentEvent) {
						events = append(events, ev.Type())
						if ev.Type() == kind && !panicked {
							panicked = true
							panic("observer")
						}
					})
					done <- events
				}()
				events := <-done
				{
					if events[len(events)-1] != "tool_result" {
						t.Fatalf("events=%v", events)
					}
				}
			})
		})
	}
}

func TestNestedInvokerCloseWaitsForDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		returning := make(chan struct{})
		done := make(chan []string, 1)
		var retained NestedInvoker
		var delivered atomic.Int64
		parent := &invokingTestTool{name: "parent", execute: func(ctx context.Context) ToolResult {
			retained, _ = NestedInvokerFrom(ctx)
			go retained.Invoke(ctx, "child", nil)
			<-entered
			close(returning)
			return ToolResult{}
		}}
		a := NewAgent(nil, "test", "", Registry{"parent": parent, "child": &resultTool{}})
		go func() {
			var events []string
			a.executeTools(context.Background(), provider.Message{Content: []provider.Content{provider.ToolCallBlock{ID: "p", Name: "parent"}}}, func(ev AgentEvent) {
				delivered.Add(1)
				events = append(events, ev.Type())
				if _, ok := ev.(EvNestedToolStarted); ok {
					close(entered)
					<-release
				}
			})
			done <- events
		}()
		<-returning
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("batch returned before nested delivery")
		default:
		}
		close(release)
		events := <-done
		{
			want := []string{"tool_execution_started", "nested_tool_started", "nested_tool_result", "tool_result"}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events=%v", events)
			}
		}
		before := delivered.Load()
		out := retained.Invoke(context.Background(), "child", nil)
		if delivered.Load() != before {
			t.Fatal("closed invoker emitted events")
		}
		if !out.Result.IsError || out.ID != "" || out.Result.Content[0].(provider.TextBlock).Text != "nested tool calls are no longer available: parent tool finished" {
			t.Fatalf("outcome=%#v", out)
		}
	})
}

func TestToolDispatcherFlushResumesQueuedEventsAfterPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var events []string
		var d *toolEventDispatcher
		d = newToolEventDispatcher(func(ev AgentEvent) {
			events = append(events, ev.Type())
			if ev.Type() == "tool_progress" {
				d.emit(EvDone{})
				panic("once")
			}
		})
		func() { defer func() { _ = recover() }(); d.emit(EvToolProgress{}) }()
		d.flush()
		if !reflect.DeepEqual(events, []string{"tool_progress", "done"}) {
			t.Fatalf("events=%v", events)
		}
	})
}

func TestToolDispatcherBoundsPendingProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var events []AgentEvent
		dispatcher := newToolEventDispatcher(func(event AgentEvent) {
			events = append(events, event)
			if event.Type() == "tool_execution_started" {
				<-release
			}
		})
		go dispatcher.emit(EvToolExecutionStarted{})
		synctest.Wait()
		for range 1024 {
			dispatcher.emit(EvToolProgress{ID: "parent", Text: strings.Repeat("界", 1024)})
		}
		dispatcher.emit(EvNestedToolStarted{ID: "child"})
		for range 1024 {
			dispatcher.emit(EvToolProgress{ID: "parent", Text: strings.Repeat("x", 1024)})
		}
		dispatcher.emit(EvNestedToolResult{ID: "child"})
		dispatcher.mu.Lock()
		if dispatcher.pendingProgressBytes > maxPendingToolProgressBytes {
			t.Errorf("pending bytes %d", dispatcher.pendingProgressBytes)
		}
		if len(dispatcher.queue) > 4 {
			t.Errorf("unbounded queue: %d", len(dispatcher.queue))
		}
		dispatcher.mu.Unlock()
		close(release)
		synctest.Wait()
		dispatcher.flush()
		markers := 0
		var lifecycle []string
		for _, event := range events {
			if p, ok := event.(EvToolProgress); ok {
				markers += strings.Count(p.Text, toolProgressTruncated)
				if !utf8.ValidString(p.Text) {
					t.Error("invalid UTF-8")
				}
			} else {
				lifecycle = append(lifecycle, event.Type())
			}
		}
		if markers != 1 {
			t.Errorf("markers=%d", markers)
		}
		if !reflect.DeepEqual(lifecycle, []string{"tool_execution_started", "nested_tool_started", "nested_tool_result"}) {
			t.Fatal(lifecycle)
		}
	})
}
