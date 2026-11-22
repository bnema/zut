package advisor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var editCall = provider.ToolCallBlock{ID: "e1", Name: "edit", Arguments: json.RawMessage(`{}`)}

type watcherHarness struct {
	w      *Watcher
	tr     *transcript
	notes  chan string
	native bool
}

func newHarness(t *testing.T, client provider.Client, native bool) *watcherHarness {
	t.Helper()
	h := &watcherHarness{tr: &transcript{}, notes: make(chan string, 8), native: native}
	h.tr.set(text(provider.RoleUser, "task", time.Unix(1, 0)))
	var reviewer *Reviewer
	if client != nil {
		reviewer = newTestReviewer(client, h.tr)
	}
	h.w = NewWatcher(WatcherConfig{
		Reviewer: reviewer,
		Native:   func() bool { return h.native },
		Queue:    func(note string) bool { h.notes <- note; return true },
		Messages: h.tr.get,
	})
	return h
}

func repliesInOrder(t *testing.T, replies ...string) *providermocks.MockClient {
	client := providermocks.NewMockClient(t)
	for _, r := range replies {
		r := r
		client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, provider.Request) (<-chan provider.Event, error) {
			return reply(r), nil
		}).Once()
	}
	return client
}

func TestWatcherClientHoldsFirstChangeOnly(t *testing.T) {
	h := newHarness(t, repliesInOrder(t, "STOP: wrong file, the bug is in lexer.go"), false)
	ctx := context.Background()
	h.w.RunStarted(ctx)
	defer h.w.RunEnded()

	require.Equal(t, core.WatcherVerdict{}, h.w.BeforeTool(ctx, provider.ToolCallBlock{Name: "read"}), "reads are never reviewed")
	v := h.w.BeforeTool(ctx, editCall)
	require.True(t, v.Hold)
	require.Contains(t, v.Note, "Held before running this change")
	require.Contains(t, v.Note, "lexer.go")
	require.Equal(t, core.WatcherVerdict{}, h.w.BeforeTool(ctx, editCall), "only the first change is a checkpoint")
}

func TestWatcherClientNoteAtChangeIsQueuedAndProceeds(t *testing.T) {
	h := newHarness(t, repliesInOrder(t, "NOTE: also update the docs"), false)
	ctx := context.Background()
	h.w.RunStarted(ctx)
	defer h.w.RunEnded()

	require.False(t, h.w.BeforeTool(ctx, editCall).Hold)
	require.Contains(t, <-h.notes, "also update the docs")
}

func TestWatcherClientFinishReviewedOnceAfterChange(t *testing.T) {
	h := newHarness(t, repliesInOrder(t, "SILENT", "STOP: tests were not run"), false)
	ctx := context.Background()
	h.w.RunStarted(ctx)
	defer h.w.RunEnded()

	require.False(t, h.w.BeforeTool(ctx, editCall).Hold)
	v := h.w.BeforeFinish(ctx)
	require.True(t, v.Hold)
	require.Contains(t, v.Note, "Before you finish")
	require.Contains(t, v.Note, "tests were not run")
	require.Equal(t, core.WatcherVerdict{}, h.w.BeforeFinish(ctx), "no second finish hold: no ping-pong")
}

func TestWatcherSkipsFinishWithoutChange(t *testing.T) {
	h := newHarness(t, providermocks.NewMockClient(t), false)
	ctx := context.Background()
	h.w.RunStarted(ctx)
	defer h.w.RunEnded()
	require.Equal(t, core.WatcherVerdict{}, h.w.BeforeFinish(ctx))
}

func TestWatcherFailedReviewLetsExecutorProceed(t *testing.T) {
	client := providermocks.NewMockClient(t)
	client.EXPECT().Stream(mock.Anything, mock.Anything).Return(nil, context.DeadlineExceeded).Once()
	h := newHarness(t, client, false)
	ctx := context.Background()
	h.w.RunStarted(ctx)
	defer h.w.RunEnded()
	require.Equal(t, core.WatcherVerdict{}, h.w.BeforeTool(ctx, editCall))
}

func TestWatcherBackgroundNoteIsQueued(t *testing.T) {
	h := newHarness(t, repliesInOrder(t, "NOTE: you are rereading the same file"), false)
	h.w.RunStarted(context.Background())
	defer h.w.RunEnded()

	h.w.StepCompleted()
	select {
	case note := <-h.notes:
		require.True(t, strings.HasPrefix(note, "[advisor note]"), note)
		require.Contains(t, note, "rereading the same file")
	case <-time.After(5 * time.Second):
		t.Fatal("background note not delivered")
	}
}

func TestWatcherBackgroundReviewStopsWithRun(t *testing.T) {
	client := providermocks.NewMockClient(t)
	started := make(chan struct{})
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
		close(started)
		<-ctx.Done()
		ch := make(chan provider.Event, 2)
		ch <- provider.EventTextDelta{Delta: "NOTE: late"}
		ch <- provider.EventDone{Err: ctx.Err()}
		close(ch)
		return ch, nil
	}).Once()
	h := newHarness(t, client, false)
	h.w.RunStarted(context.Background())
	h.w.StepCompleted()
	<-started
	h.w.RunEnded()

	require.Eventually(t, func() bool {
		h.w.mu.Lock()
		defer h.w.mu.Unlock()
		return !h.w.running
	}, 5*time.Second, time.Millisecond)
	require.Empty(t, h.notes, "a canceled run never receives a late note")
}

func TestWatcherNativeAsksExecutorToConsultAtCheckpoints(t *testing.T) {
	h := newHarness(t, nil, true)
	ctx := context.Background()
	h.w.RunStarted(ctx)
	defer h.w.RunEnded()

	h.w.StepCompleted() // native mode never reviews in the background
	require.Empty(t, h.notes)

	v := h.w.BeforeTool(ctx, editCall)
	require.True(t, v.Hold)
	require.Equal(t, nativeChangeNotice, v.Note)

	// The executor consults, then changes again: the finish checkpoint
	// still applies because the change came after the consultation.
	h.tr.add(provider.Message{Role: provider.RoleAssistant, Time: time.Unix(2, 0), Content: []provider.Content{
		provider.ServerToolBlock{Provider: "anthropic", Kind: "advisor_tool_result", ID: "s1", Name: "advisor"},
	}}, provider.Message{Role: provider.RoleAssistant, Time: time.Unix(3, 0), Content: []provider.Content{editCall}})
	require.False(t, h.w.BeforeTool(ctx, editCall).Hold)
	v = h.w.BeforeFinish(ctx)
	require.True(t, v.Hold)
	require.Equal(t, nativeFinishNotice, v.Note)
}

func TestWatcherNativeSkipsCheckpointsAfterConsult(t *testing.T) {
	h := newHarness(t, nil, true)
	ctx := context.Background()
	h.w.RunStarted(ctx)
	defer h.w.RunEnded()
	h.tr.add(provider.Message{Role: provider.RoleAssistant, Time: time.Unix(2, 0), Content: []provider.Content{
		provider.ServerToolBlock{Provider: "anthropic", Kind: "server_tool_use", ID: "s1", Name: "advisor"},
	}})
	require.False(t, h.w.BeforeTool(ctx, editCall).Hold, "already consulted in this request")
	require.False(t, h.w.BeforeFinish(ctx).Hold, "consulted after the last change")
}

func TestWatcherCapsBackgroundNotesPerRun(t *testing.T) {
	client := providermocks.NewMockClient(t)
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, provider.Request) (<-chan provider.Event, error) {
		return reply("NOTE: something"), nil
	}).Times(maxBackgroundNotes)
	h := newHarness(t, client, false)
	h.w.RunStarted(context.Background())
	defer h.w.RunEnded()

	idle := func() bool {
		h.w.mu.Lock()
		defer h.w.mu.Unlock()
		return !h.w.running
	}
	for i := 0; i < 20; i++ {
		require.Eventually(t, idle, 5*time.Second, time.Millisecond)
		h.w.StepCompleted()
	}
	require.Eventually(t, idle, 5*time.Second, time.Millisecond)
	require.Len(t, h.notes, maxBackgroundNotes)
}

func TestWatcherFinishCancelsBackgroundReview(t *testing.T) {
	client := providermocks.NewMockClient(t)
	started := make(chan struct{})
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
		close(started)
		<-ctx.Done()
		ch := make(chan provider.Event, 2)
		ch <- provider.EventTextDelta{Delta: "NOTE: late"}
		ch <- provider.EventDone{Err: ctx.Err()}
		close(ch)
		return ch, nil
	}).Once()
	h := newHarness(t, client, false)
	ctx := context.Background()
	h.w.RunStarted(ctx)
	defer h.w.RunEnded()
	h.w.StepCompleted()
	<-started
	// No change in this run: the finish skips its own review but still
	// stops the background one.
	require.Equal(t, core.WatcherVerdict{}, h.w.BeforeFinish(ctx))
	require.Eventually(t, func() bool {
		h.w.mu.Lock()
		defer h.w.mu.Unlock()
		return !h.w.running
	}, 5*time.Second, time.Millisecond)
	require.Empty(t, h.notes)
}
