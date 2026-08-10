package modes

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
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
	i := &Interactive{
		dirty:     make(chan struct{}, 1),
		toolCalls: make(map[string]*tui.ToolCallView),
		toolGate:  make(map[string]int),
	}

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
	i := &Interactive{
		dirty:     make(chan struct{}, 1),
		toolCalls: make(map[string]*tui.ToolCallView),
		toolGate:  make(map[string]int),
	}
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

	agent := &core.Agent{}
	agent.SetMessages([]provider.Message{
		{
			Role:    provider.RoleUser,
			Content: []provider.Content{provider.TextBlock{Text: "prompt"}},
		},
		{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.TextBlock{Text: finalText}},
		},
	})
	i := &Interactive{
		agent:              agent,
		view:               &tui.View{Theme: tui.Dark},
		renderOutsideLock:  true,
		streamFlushPending: true,
	}

	revision := agent.Revision()
	i.mu.Lock()
	before := strings.Join(i.cachedChatLocked(80), "\n")
	i.streamFlushPending = false
	after := strings.Join(i.cachedChatLocked(80), "\n")
	i.mu.Unlock()

	if got := agent.Revision(); got != revision {
		t.Fatalf("stream flush changed the transcript revision: got %d, want %d", got, revision)
	}
	if strings.Contains(before, finalText) {
		t.Fatal("stable cache revealed the final message while stream flush was pending")
	}
	if !strings.Contains(after, finalText) {
		t.Fatal("stable cache did not reveal the final message after stream flush")
	}
}

func TestInteractiveToolProgressStormDoesNotInvalidate(t *testing.T) {
	i := &Interactive{
		dirty:     make(chan struct{}, 1),
		toolCalls: make(map[string]*tui.ToolCallView),
		toolGate:  make(map[string]int),
	}
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
