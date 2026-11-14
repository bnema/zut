package modes

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestLatestFrameSchedulerKeepsNewestRequest(t *testing.T) {
	s := newLatestFrameScheduler()
	started := make(chan struct{})
	release := make(chan struct{})
	type renderedFrame struct {
		req     renderRequest
		version int64
	}
	frames := make(chan renderedFrame, 4)
	done := make(chan struct{})
	var startedOnce sync.Once
	var latestVersion atomic.Int64
	go func() {
		s.run(func(req renderRequest) {
			frames <- renderedFrame{req: req, version: latestVersion.Load()}
			startedOnce.Do(func() { close(started) })
			<-release
		})
		close(done)
	}()

	latestVersion.Store(1)
	if !s.request(false, false) {
		t.Fatal("initial render request was rejected")
	}
	first := <-frames
	<-started
	if first.req.clear {
		t.Fatal("ordinary request unexpectedly requested a clear")
	}
	for n := 0; n < 1000; n++ {
		latestVersion.Store(int64(n + 2))
		if !s.request(false, false) {
			t.Fatal("request rejected before shutdown")
		}
	}
	close(release)
	second := <-frames
	if second.req.clear {
		t.Fatal("coalesced ordinary request unexpectedly requested a clear")
	}
	if second.version != 1001 {
		t.Fatalf("renderer did not observe newest state: got version %d", second.version)
	}
	s.stop()
	<-done

	select {
	case <-frames:
		t.Fatal("scheduler retained more than one pending frame")
	default:
	}
}

func TestToolRenderRevisionsAreGloballyMonotonic(t *testing.T) {
	i := &Interactive{dirty: make(chan struct{}, 1), toolCalls: make(map[string]*tui.ToolCallView)}

	i.handleEventForPresentation(core.EvToolUseStart{ID: "alpha", Name: "edit"})
	alphaStart := i.toolCalls["alpha"].Revision
	i.handleEventForPresentation(core.EvToolUseArgs{ID: "alpha", Delta: `{"path":"alpha.go"}`})
	alphaArgs := i.toolCalls["alpha"].Revision
	i.handleEventForPresentation(core.EvToolUseStart{ID: "beta", Name: "edit"})
	betaStart := i.toolCalls["beta"].Revision
	i.handleEventForPresentation(core.EvToolUseArgs{ID: "beta", Delta: `{"path":"beta.go"}`})
	betaArgs := i.toolCalls["beta"].Revision

	if alphaStart >= alphaArgs || alphaArgs >= betaStart || betaStart >= betaArgs {
		t.Fatalf("tool revisions are not globally monotonic: alpha start=%d args=%d, beta start=%d args=%d",
			alphaStart, alphaArgs, betaStart, betaArgs)
	}
}

func TestToolEventBurstUsesThrottledInvalidationPath(t *testing.T) {
	scheduler := newLatestFrameScheduler()
	i := &Interactive{dirty: make(chan struct{}, 1), toolCalls: make(map[string]*tui.ToolCallView)}
	i.renderScheduler.Store(scheduler)

	i.handleEventForPresentation(core.EvToolUseStart{ID: "call", Name: "bash"})
	i.handleEventForPresentation(core.EvToolUseArgs{ID: "call", Delta: `{"command":"pwd"}`})
	i.handleEventForPresentation(core.EvToolUseEnd{ID: "call"})

	if got := len(scheduler.wake); got != 0 {
		t.Fatalf("tool events bypassed the main-loop redraw throttle: %d direct request", got)
	}
	select {
	case <-i.dirty:
	default:
		t.Fatal("tool events did not wake the throttled redraw path")
	}
	select {
	case <-i.dirty:
		t.Fatal("tool event burst queued more than one throttled redraw")
	default:
	}
}

func TestStableChatCacheTracksViewInvalidation(t *testing.T) {
	i := &Interactive{
		view: &tui.View{
			Theme: tui.Dark,
			Messages: []provider.Message{{
				Role:    provider.RoleUser,
				Content: []provider.Content{provider.TextBlock{Text: "cache theme"}},
			}},
			MessagesRevision: 1,
		},
	}

	i.mu.Lock()
	before := strings.Join(i.stableChatRowsLocked(80), "\n")
	i.view.Theme = tui.Light
	i.view.InvalidateRenderCache()
	after := strings.Join(i.stableChatRowsLocked(80), "\n")
	i.mu.Unlock()

	if before == after {
		t.Fatal("stable chat cache reused rows after a theme invalidation")
	}
}

func TestStableChatCacheRevealsMessageAfterStreamFlush(t *testing.T) {
	const finalText = "final message revealed after paced flush"

	port := sessionmocks.NewMockPort(t)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{
		Info: session.Info{ID: "render", Run: session.RunStateIdle},
		Seq:  1,
		Messages: []provider.Message{{
			Role:    provider.RoleUser,
			Content: []provider.Content{provider.TextBlock{Text: "prompt"}},
		}},
	}, nil).Once()
	port.EXPECT().Subscribe(mock.Anything, uint64(1)).Return(make(chan session.Event), nil).Once()
	i := NewInteractive(InteractiveConfig{Port: port})
	i.renderOutsideLock = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, i.attachPort(ctx))

	i.mu.Lock()
	i.startStreamLocked()
	i.stream.Push(finalText)
	i.stream.Finish()
	i.mu.Unlock()
	// The daemon publishes the finished reply while the pacer is still busy.
	i.mu.Lock()
	i.applyPortAgentTranscriptLocked(core.EvAssistantMessage{Message: provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: finalText}},
	}})
	i.mu.Unlock()

	i.mu.Lock()
	before := strings.Join(i.cachedChatLocked(80), "\n")
	for i.stream.Active() {
		i.stream.Tick()
	}
	after := strings.Join(i.cachedChatLocked(80), "\n")
	i.mu.Unlock()

	if strings.Contains(before, finalText) {
		t.Fatal("stable cache revealed the final message while stream flush was pending")
	}
	if !strings.Contains(after, finalText) {
		t.Fatal("stable cache did not reveal the final message after stream flush")
	}
}

func TestInteractiveToolProgressStormDoesNotInvalidate(t *testing.T) {
	i := &Interactive{dirty: make(chan struct{}, 1), toolCalls: make(map[string]*tui.ToolCallView)}
	i.handleEventForPresentation(core.EvToolUseStart{ID: "storm", Name: "bash"})
	select {
	case <-i.dirty:
	default:
		t.Fatal("tool start did not invalidate the presentation")
	}

	const progressEvents = 4096
	for n := 0; n < progressEvents; n++ {
		i.handleEventForPresentation(core.EvToolProgress{
			ID:   "storm",
			Text: strings.Repeat("progress payload ", 256),
		})
	}
	select {
	case <-i.dirty:
		t.Fatal("invisible progress created presentation invalidation")
	default:
	}

	result := strings.Repeat("completed tool output\n", 256)
	i.handleEventForPresentation(core.EvToolResult{
		ID: "storm",
		Result: core.ToolResult{Content: []provider.Content{
			provider.TextBlock{Text: result},
		}},
	})
	select {
	case <-i.dirty:
	default:
		t.Fatal("completed tool result did not invalidate the presentation")
	}
	if got := i.toolCalls["storm"].Result; got != result {
		t.Fatalf("completed tool result was not retained: got %d bytes", len(got))
	}
	if got := i.toolCalls["storm"].Revision; got != 2 {
		t.Fatalf("completed tool result revision: got %d, want 2", got)
	}

	view := &tui.View{Theme: tui.Dark, ExpandAll: false, ToolCalls: []tui.ToolCallView{*i.toolCalls["storm"]}}
	rows := view.BuildLive(80)
	if len(rows) == 0 {
		t.Fatal("completed tool result produced no live frame rows")
	}
}

func TestInteractiveNestedToolLines(t *testing.T) {
	i := &Interactive{dirty: make(chan struct{}, 1), toolCalls: make(map[string]*tui.ToolCallView)}
	i.handleEvent(core.EvToolCall{ID: "p", Name: "parent"})
	before := i.toolCalls["p"].Revision
	i.handleEvent(core.EvNestedToolStarted{ID: "p/1", ParentID: "p", Name: "child"})
	i.handleEvent(core.EvNestedToolResult{ParentID: "unknown", Name: "ignored"})
	require.Len(t, i.toolCalls, 1)
	require.Equal(t, before, i.toolCalls["p"].Revision)
	for n := 0; n < 10; n++ {
		i.handleEventForPresentation(core.EvNestedToolResult{ParentID: "p", Name: fmt.Sprintf("child%d", n), IsError: n == 8, Denied: n == 9})
	}
	tc := i.toolCalls["p"]
	require.Equal(t, before+10, tc.Revision)
	require.True(t, strings.HasPrefix(tc.Result, "… 2 earlier\n↳ child2 ok\n"))
	require.True(t, strings.HasSuffix(tc.Result, "↳ child8 error\n↳ child9 denied\n"))
	require.Equal(t, 9, strings.Count(tc.Result, "\n"))
	view := &tui.View{Theme: tui.Dark, ToolCalls: []tui.ToolCallView{*tc}}
	require.Contains(t, strings.Join(view.BuildLive(80), "\n"), "child9 denied")
	i.applyPortAgentTranscriptLocked(core.EvNestedToolResult{ParentID: "p", Name: "child"})
	require.Nil(t, i.portToolMsg)
	require.Empty(t, i.portMsgs)
	i.handleEvent(core.EvToolResult{ID: "p", Result: core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "real result"}}}})
	require.Equal(t, "real result", tc.Result)
	require.Empty(t, i.nestedToolLines)
	i.handleEvent(core.EvNestedToolResult{ParentID: "p", Name: "late"})
	require.Equal(t, "real result", tc.Result)
}

func TestInteractiveRestoreNestedToolLines(t *testing.T) {
	i := &Interactive{}
	tc := &tui.ToolCallView{ID: "p"}
	var progress []string
	for n := 0; n < 10; n++ {
		progress = append(progress, fmt.Sprintf("↳ child%d ok\n", n))
	}
	i.restoreNestedToolLinesLocked(tc, progress)
	require.True(t, strings.HasPrefix(tc.Result, "… 2 earlier\n↳ child2 ok\n"))
	require.NotContains(t, tc.Result, "ordinary")
	i.appendNestedToolLineLocked(tc, "↳ next denied\n")
	require.True(t, strings.HasPrefix(tc.Result, "… 3 earlier\n"))
	require.True(t, strings.HasSuffix(tc.Result, "↳ next denied\n"))
	untouched := &tui.ToolCallView{ID: "other"}
	i.restoreNestedToolLinesLocked(untouched, nil)
	require.Empty(t, untouched.Result)
}

func TestActivityNestedResultsDoNotCompleteParent(t *testing.T) {
	var a agentActivity
	a.apply(core.EvToolCall{ID: "p", Name: "parent"})
	a.apply(core.EvToolExecutionStarted{ID: "p", Name: "parent"})
	a.apply(core.EvNestedToolStarted{ID: "p/1", ParentID: "p", Name: "child"})
	a.apply(core.EvNestedToolResult{ID: "p/1", ParentID: "p", Name: "child"})
	require.Equal(t, activityRunningTool, a.kind)
	require.Equal(t, map[string]string{"p": "parent"}, a.pendingToolCalls)
}

func TestInteractiveHydrateNestedToolProgress(t *testing.T) {
	i := NewInteractive(InteractiveConfig{})
	i.mu.Lock()
	defer i.mu.Unlock()
	i.hydrateLocked(session.Snapshot{Live: session.LiveTurn{Tools: []session.LiveTool{
		{ID: "running", Name: "parent", State: "running", Progress: "ordinary↳ child denied\n", Nested: []string{"↳ child denied\n"}},
		{ID: "done", Name: "parent", State: "done", Progress: "↳ child ok\n", Result: &core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "real"}}}},
		{ID: "plain", Name: "parent", State: "running", Progress: "↳ child ok\n"},
	}}})
	require.Equal(t, "↳ child denied\n", i.toolCalls["running"].Result)
	require.Equal(t, "real", i.toolCalls["done"].Result)
	require.Empty(t, i.toolCalls["plain"].Result)
	require.Len(t, i.toolCalls, 3)
}
