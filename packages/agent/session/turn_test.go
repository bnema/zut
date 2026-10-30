package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func turnSeams(t *testing.T, c *Controller, ctx context.Context) {
	t.Helper()
	compact := newMockcompactSeam(t)
	compact.EXPECT().compacting().Return(false).Maybe()
	compact.EXPECT().maybeAutoCompact(mock.Anything).Return(false).Maybe()
	goal := newMockgoalSeam(t)
	goal.EXPECT().goalBeforeTurn(mock.Anything).Return().Maybe()
	goal.EXPECT().goalAfterTurn(mock.Anything).Return().Maybe()
	goal.EXPECT().goalOnAbort().Return().Maybe()
	confirm := newMockconfirmSeam(t)
	confirm.EXPECT().denyAllPending("turn cancelled").Return().Maybe()
	require.NoError(t, c.call(ctx, func() { c.compactSeam = compact; c.goalSeam = goal; c.confirmSeam = confirm }))
}
func TestTurnStreamAndQueue(t *testing.T) {
	c, ctx := newTestController(t)
	turnSeams(t, c, ctx)
	residents := newMockresidentSeam(t)
	residents.EXPECT().residentsRunStarted().Return().Twice()
	residents.EXPECT().residentsRunEnded().Return().Twice()
	residents.EXPECT().residentSummaries().Return(nil).Maybe()
	require.NoError(t, c.call(ctx, func() { c.residentSeam = residents }))

	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("test").Maybe()
	first := make(chan provider.Event, 8)
	second := make(chan provider.Event, 8)
	started := make(chan struct{}, 2)
	count := 0
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		count++
		started <- struct{}{}
		if count == 1 {
			return first, nil
		}
		return second, nil
	}).Twice()
	ag := core.NewAgent(client, "test", "", nil)
	require.NoError(t, c.call(ctx, func() { c.turn.agent = ag }))
	clientCtx, cancel := context.WithCancel(ctx)
	require.NoError(t, c.Submit(clientCtx, Input{Text: "first"}))
	cancel()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, c.Submit(ctx, Input{Text: "second", Mode: "queue"}))
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snap.Queue, 1)
	first <- provider.EventTextDelta{Delta: "hel"}
	first <- provider.EventTextDelta{Delta: "lo"}
	first <- provider.EventUsage{Usage: provider.Usage{InputTokens: 12, OutputTokens: 2}}
	first <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "hello"}}}}
	close(first)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	snap, err = c.Snapshot(ctx)
	require.NoError(t, err)
	events, err := c.Subscribe(ctx, snap.Seq)
	require.NoError(t, err)
	second <- provider.EventTextDelta{Delta: "world"}
	second <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "world"}}}}
	close(second)
	seq := snap.Seq
	done := false
	for !done {
		select {
		case e := <-events:
			seq++
			require.Equal(t, seq, e.Seq)
			if e.Kind == EvInfo && e.Data.(InfoEvent).Info.Run == RunStateIdle {
				done = true
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	snap, err = c.Snapshot(ctx)
	require.NoError(t, err)
	require.Empty(t, snap.Queue)
	require.Empty(t, snap.Live.AssistantText, "idle snapshot has no live turn")
	require.Empty(t, snap.Live.Tools)
	last := snap.Messages[len(snap.Messages)-1]
	require.Equal(t, "world", last.Content[0].(provider.TextBlock).Text)
	require.Equal(t, 12, snap.Info.ContextUsed)
}
func TestTurnQueueEditingAndAbort(t *testing.T) {
	c, ctx := newTestController(t)
	turnSeams(t, c, ctx)
	require.NoError(t, c.call(ctx, func() { c.turn.busy = true }))
	require.NoError(t, c.Submit(ctx, Input{Text: "a"}))
	require.NoError(t, c.Submit(ctx, Input{Text: "b"}))
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snap.Queue, 2)
	require.NoError(t, c.CancelQueued(ctx, snap.Queue[0].ID))
	in, err := c.SlideBackQueued(ctx)
	require.NoError(t, err)
	require.Equal(t, "b", in.Text)
	_, err = c.SlideBackQueued(ctx)
	require.ErrorIs(t, err, ErrUnknownRequest)
	require.NoError(t, c.Abort(ctx))
}
func TestTurnToolProjection(t *testing.T) {
	c, ctx := newTestController(t)
	require.NoError(t, c.call(ctx, func() {
		for _, e := range []core.AgentEvent{core.EvToolUseStart{ID: "x", Name: "bash"}, core.EvToolUseArgs{ID: "x", Delta: `{}`}, core.EvToolUseEnd{ID: "x"}, core.EvToolCall{ID: "x", Name: "bash", Args: []byte(`{}`)}, core.EvToolExecutionStarted{ID: "x", Name: "bash"}, core.EvToolProgress{ID: "x", Text: "ok"}, core.EvToolResult{ID: "x", Result: core.ToolResult{}}} {
			c.applyAgentEvent(e)
		}
	}))
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snap.Live.Tools, 1)
	require.Equal(t, "done", snap.Live.Tools[0].State)
	require.Equal(t, "ok", snap.Live.Tools[0].Progress)
}

func TestTurnSteerAndCancelProvider(t *testing.T) {
	c, ctx := newTestController(t)
	turnSeams(t, c, ctx)
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("test").Maybe()
	started := make(chan context.Context, 1)
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(run context.Context, _ provider.Request) (<-chan provider.Event, error) {
		started <- run
		return make(chan provider.Event), nil
	}).Once()
	ag := core.NewAgent(client, "test", "", nil)
	require.NoError(t, c.call(ctx, func() { c.turn.agent = ag }))
	require.NoError(t, c.Submit(ctx, Input{Text: "run"}))
	var run context.Context
	select {
	case run = <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.NoError(t, c.Submit(ctx, Input{Text: "steer", Mode: "steer", HostEvent: true}))
	pending := ag.PendingQueuedMessages()
	require.Len(t, pending, 1)
	require.True(t, pending[0].HostEvent)
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.Abort(ctx))
	select {
	case <-run.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for {
		select {
		case e := <-events:
			if e.Kind == EvInfo && e.Data.(InfoEvent).Info.Run == RunStateIdle {
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestTurnAgentReplay(t *testing.T) {
	c, ctx := newTestController(t)
	require.NoError(t, c.call(ctx, func() {
		c.applyAgentEvent(core.EvTextDelta{Delta: "a"})
		c.applyAgentEvent(core.EvTextDelta{Delta: "b"})
	}))
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	ch, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	var text string
	for seq := uint64(1); seq <= snap.Seq; seq++ {
		select {
		case e := <-ch:
			require.Equal(t, seq, e.Seq)
			if e.Kind == EvAgent {
				text += e.Agent.(core.EvTextDelta).Delta
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	require.Equal(t, "ab", text)
}

func TestTurnAbortExecutingTool(t *testing.T) {
	c, ctx := newTestController(t)
	turnSeams(t, c, ctx)
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("test").Maybe()
	stream := make(chan provider.Event, 1)
	stream <- provider.EventDone{Stop: provider.StopToolUse, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "bash1", Name: "bash", Arguments: []byte(`{"command":"printf 'executing\\n'; sleep 5","timeout":6}`)}}}}
	close(stream)
	client.EXPECT().Stream(mock.Anything, mock.Anything).Return((<-chan provider.Event)(stream), nil).Once()
	ag := core.NewAgent(client, "test", "", core.NewRegistry(&tools.BashTool{CWD: t.TempDir(), Env: os.Environ()}))
	require.NoError(t, c.call(ctx, func() { c.turn.agent = ag }))
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.Submit(ctx, Input{Text: "run tool"}))
	for {
		select {
		case e := <-events:
			if p, ok := e.Agent.(core.EvToolProgress); ok && p.Text != "" {
				goto executing
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
executing:
	require.NoError(t, c.Abort(ctx))
	result := false
	for {
		select {
		case e := <-events:
			if _, ok := e.Agent.(core.EvToolResult); ok {
				result = true
			}
			if e.Kind == EvInfo && e.Data.(InfoEvent).Info.Run == RunStateIdle {
				require.True(t, result, "tool result must precede idle")
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestTurnFollowUpErrorPolicy(t *testing.T) {
	for _, tc := range []struct {
		name               string
		err                error
		cancel             bool
		wantUser, wantHost bool
	}{
		{name: "success", wantUser: true, wantHost: true},
		{name: "provider_error", err: errors.New("provider failed"), wantHost: true},
		{name: "cancelled", cancel: true},
		{name: "repetition", err: core.ErrRepetitiveLoop, wantUser: true, wantHost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ctx := newTestController(t)
			turnSeams(t, c, ctx)
			ag := core.NewAgent(nil, "test", "", nil)
			// Prevent follow-up execution while inspecting the selected input and queues.
			compact := newMockcompactSeam(t)
			compact.EXPECT().compacting().Return(false).Maybe()
			var selected Input
			compact.EXPECT().maybeAutoCompact(mock.Anything).RunAndReturn(func(in Input) bool { selected = in; return true }).Maybe()
			require.NoError(t, c.call(ctx, func() {
				c.compactSeam = compact
				c.turn.agent = ag
				c.turn.busy = true
				c.enqueue(Input{Text: "scheduled", Origin: "scheduler"})
				c.enqueue(Input{Text: "user", Origin: "user"})
				ag.QueuePrompt(core.QueuedMessage{Text: "host", HostEvent: true})
				run, cancel := context.WithCancel(ctx)
				defer cancel()
				if tc.cancel {
					cancel()
				}
				c.finishTurn(run, tc.err)
			}))
			if tc.wantUser {
				require.Equal(t, "user", selected.Text)
			} else {
				require.Equal(t, "scheduled", selected.Text)
			}
			require.Equal(t, tc.wantHost, ag.QueuedMessageCount() > 0)
		})
	}
}

func TestTurnQueuedHostProvenance(t *testing.T) {
	c, ctx := newTestController(t)
	turnSeams(t, c, ctx)
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("test").Maybe()
	response := make(chan provider.Event, 2)
	response <- provider.EventTextDelta{Delta: "received"}
	response <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "received"}}}}
	close(response)
	client.EXPECT().Stream(mock.Anything, mock.Anything).Return((<-chan provider.Event)(response), nil).Once()
	ag := core.NewAgent(client, "test", "", nil)
	require.NoError(t, c.call(ctx, func() { c.turn.agent = ag }))
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.Submit(ctx, Input{Text: "host report", HostEvent: true, Origin: "resident"}))
	for {
		select {
		case e := <-events:
			if e.Kind == EvInfo && e.Data.(InfoEvent).Info.Run == RunStateIdle {
				messages := ag.Messages()
				require.GreaterOrEqual(t, len(messages), 2)
				require.Equal(t, "host report", messages[0].Content[0].(provider.TextBlock).Text)
				require.Equal(t, "received", messages[len(messages)-1].Content[0].(provider.TextBlock).Text)
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestTurnAgentOwnership(t *testing.T) {
	c, ctx := newTestController(t)
	first := core.NewAgent(nil, "first", "", nil)
	second := core.NewAgent(nil, "second", "", nil)
	require.NoError(t, c.call(ctx, func() {
		require.Nil(t, c.currentAgent(), "missing builder must not manufacture an agent")
		require.NoError(t, c.setAgent(first))
		require.Same(t, first, c.currentAgent(), "cached agent must not need a builder")
		c.turn.busy = true
		require.ErrorIs(t, c.setAgent(second), ErrBusy)
		require.Same(t, first, c.currentAgent())
		c.turn.busy = false
		require.NoError(t, c.setAgent(second))
		require.Same(t, second, c.currentAgent())
		require.NoError(t, c.setAgent(nil))
		require.Nil(t, c.currentAgent())
	}))
}

func TestTurnGoalHiddenBeforePrompt(t *testing.T) {
	c, ctx := newTestController(t)
	turnSeams(t, c, ctx)
	goal := newMockgoalSeam(t)
	goal.EXPECT().goalBeforeTurn(mock.Anything).RunAndReturn(func(in *Input) {
		in.Hidden = append(in.Hidden, provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "hidden reassessment"}}, Meta: map[string]string{"goal_interrupt_reassess": "true"}})
	}).Once()
	goal.EXPECT().goalAfterTurn(mock.Anything).Return().Once()
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("test").Maybe()
	response := make(chan provider.Event, 2)
	response <- provider.EventTextDelta{Delta: "answer"}
	response <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "answer"}}}}
	close(response)
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		require.GreaterOrEqual(t, len(req.Messages), 2)
		require.Equal(t, "hidden reassessment", req.Messages[0].Content[len(req.Messages[0].Content)-1].(provider.TextBlock).Text)
		require.Equal(t, "true", req.Messages[0].Meta["goal_interrupt_reassess"])
		require.Equal(t, "visible prompt", req.Messages[1].Content[len(req.Messages[1].Content)-1].(provider.TextBlock).Text)
		return response, nil
	}).Once()
	ag := core.NewAgent(client, "test", "", nil)
	require.NoError(t, c.call(ctx, func() { c.turn.agent = ag; c.goalSeam = goal }))
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.Submit(ctx, Input{Text: "visible prompt"}))
	for {
		select {
		case e := <-events:
			if e.Kind == EvInfo && e.Data.(InfoEvent).Info.Run == RunStateIdle {
				// Barrier includes goalAfterTurn, which follows idle publication.
				require.NoError(t, c.call(ctx, func() {}))
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestTurnCallbacksReentrantAndOrdered(t *testing.T) {
	c, ctx := newTestController(t)
	q := c.newTurnCallbacks()
	require.NoError(t, c.call(ctx, func() {
		q.push(func() { c.applyAgentEvent(core.EvTextDelta{Delta: "one"}) })
		q.push(func() { c.applyAgentEvent(core.EvTextDelta{Delta: "two"}) })
		q.push(func() { close(q.finished) })
	}))
	select {
	case <-q.finished:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, "onetwo", snap.Live.AssistantText)
}

func TestTurnCallbacksBoundedBurst(t *testing.T) {
	c, ctx := newTestController(t)
	gate := make(chan struct{})
	require.NoError(t, c.post(ctx, func() { <-gate }))
	q := c.newTurnCallbacks()
	var want strings.Builder
	maxPending := 0
	for i := 0; i < 10000; i++ {
		d := fmt.Sprint(i%10, ",")
		want.WriteString(d)
		q.pushEvent(core.EvTextDelta{Delta: d})
		q.mu.Lock()
		maxPending = max(maxPending, len(q.pending))
		q.mu.Unlock()
	}
	require.LessOrEqual(t, maxPending, callbackMergeThreshold+1)
	q.push(func() { close(q.finished) })
	close(gate)
	select {
	case <-q.finished:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, want.String(), snap.Live.AssistantText)
}

func TestTurnInfoOnlyOnProjectionChange(t *testing.T) {
	c, ctx := newTestController(t)
	before, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.NoError(t, c.call(ctx, func() {
		for i := 0; i < 1000; i++ {
			c.applyAgentEvent(core.EvTextDelta{Delta: "x"})
		}
		c.applyAgentEvent(core.EvUsage{Usage: provider.Usage{InputTokens: 3}})
	}))
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	events, err := c.Subscribe(ctx, before.Seq)
	require.NoError(t, err)
	infos := 0
	for seq := before.Seq; seq < snap.Seq; seq++ {
		if (<-events).Kind == EvInfo {
			infos++
		}
	}
	require.Equal(t, 2, infos, "streaming start and usage only")
}

func idleWithQueue(t *testing.T, c *Controller, ctx context.Context, items ...Input) {
	t.Helper()
	require.NoError(t, c.call(ctx, func() {
		c.turn.busy = true
		for _, in := range items {
			c.enqueue(in)
		}
	}))
}

func TestTurnQueuedStartFailureRestoresItem(t *testing.T) {
	c, ctx := newTestController(t)
	turnSeams(t, c, ctx)
	// No agent and no builder: ensureAgent fails when the queued item starts.
	idleWithQueue(t, c, ctx, Input{Text: "scheduled", Origin: "scheduler"}, Input{Text: "user", Origin: "user"})
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	var queue []QueuedInput
	var note Note
	require.NoError(t, c.call(ctx, func() {
		c.finishTurn(context.Background(), nil)
		queue = append(queue, c.turn.queue...)
	}))
	require.Len(t, queue, 2)
	require.Equal(t, "user", queue[1].Input.Text)
	for e := range events {
		if e.Kind == EvNote {
			note = e.Data.(NoteEvent).Note
			break
		}
	}
	require.Equal(t, "error", note.Level)
	require.Contains(t, note.Text, "builder unavailable")
}

func TestTurnContinuingSkipsAutoCompact(t *testing.T) {
	c, ctx := newTestController(t)
	turnSeams(t, c, ctx)
	compact := newMockcompactSeam(t)
	compact.EXPECT().compacting().Return(false).Maybe()
	compactCalls := 0
	compact.EXPECT().maybeAutoCompact(mock.Anything).RunAndReturn(func(Input) bool { compactCalls++; return false }).Maybe()
	goal := newMockgoalSeam(t)
	goal.EXPECT().goalAfterTurn(mock.Anything).Return().Once()
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("test").Maybe()
	response := make(chan provider.Event, 1)
	response <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "ok"}}}}
	close(response)
	client.EXPECT().Stream(mock.Anything, mock.Anything).Return((<-chan provider.Event)(response), nil).Once()
	ag := core.NewAgent(client, "test", "", nil)
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	require.NoError(t, c.call(ctx, func() {
		c.compactSeam = compact
		c.goalSeam = goal
		c.turn.agent = ag
		c.turn.busy = true
		ag.QueuePrompt(core.QueuedMessage{Text: "agent queued"})
		c.finishTurn(context.Background(), nil)
	}))
	idle := 0
	for idle < 2 {
		select {
		case e := <-events:
			if e.Kind == EvInfo && e.Data.(InfoEvent).Info.Run == RunStateIdle {
				idle++
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	require.NoError(t, c.call(ctx, func() {}))
	require.Zero(t, compactCalls)
}

func TestTurnRepetitionRebuildsQueueOnce(t *testing.T) {
	c, ctx := newTestController(t)
	turnSeams(t, c, ctx)
	compact := newMockcompactSeam(t)
	compact.EXPECT().compacting().Return(false).Maybe()
	compact.EXPECT().maybeAutoCompact(mock.Anything).Return(true).Once()
	ag := core.NewAgent(nil, "test", "", nil)
	require.NoError(t, c.call(ctx, func() {
		c.compactSeam = compact
		c.turn.agent = ag
		c.turn.busy = true
		c.enqueue(Input{Text: "existing-1", Origin: "user"})
		c.enqueue(Input{Text: "existing-2", Origin: "user"})
		ag.QueuePrompt(core.QueuedMessage{Text: "typed"})
	}))
	before, err := c.Snapshot(ctx)
	require.NoError(t, err)
	var queue []QueuedInput
	require.NoError(t, c.call(ctx, func() {
		c.finishTurn(context.Background(), core.ErrRepetitiveLoop)
		queue = append(queue, c.turn.queue...)
	}))
	// Stacked user inputs start together as one turn (compaction intercepts).
	require.Empty(t, queue)
	events, err := c.Subscribe(ctx, before.Seq)
	require.NoError(t, err)
	first := (<-events)
	require.Equal(t, EvQueue, first.Kind)
	texts := []string{}
	for _, q := range first.Data.(QueueEvent).Queue {
		texts = append(texts, q.Input.Text)
	}
	require.Equal(t, []string{"existing-1", "existing-2", "typed"}, texts)
	require.NotEqual(t, EvQueue, (<-events).Kind, "rebuild publishes one queue event before info")
}

func TestTurnHiddenTextOnly(t *testing.T) {
	c, ctx := newTestController(t)
	turnSeams(t, c, ctx)
	compact := newMockcompactSeam(t)
	compact.EXPECT().compacting().Return(false).Maybe()
	compact.EXPECT().maybeAutoCompact(mock.Anything).Return(false).Maybe()
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("test").Maybe()
	stream := make(chan provider.Event)
	started := make(chan struct{})
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		close(started)
		return stream, nil
	}).Once()
	ag := core.NewAgent(client, "test", "", nil)
	require.NoError(t, c.call(ctx, func() { c.turn.agent = ag; c.compactSeam = compact }))
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	hidden := []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "ctx"}, provider.ImageBlock{}}}}
	require.NoError(t, c.Submit(ctx, Input{Text: "go", Hidden: hidden}))
	for e := range events {
		if e.Kind == EvNote {
			require.Equal(t, "error", e.Data.(NoteEvent).Note.Level)
			require.Contains(t, e.Data.(NoteEvent).Note.Text, "non-text")
			break
		}
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// A busy steer with hidden context is queued, not steered.
	require.NoError(t, c.Submit(ctx, Input{Text: "steer", Mode: "steer", Hidden: hidden}))
	require.Zero(t, ag.QueuedMessageCount())
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snap.Queue, 1)
	require.NoError(t, c.Abort(ctx))
}
