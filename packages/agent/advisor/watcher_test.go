package advisor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var editCall = provider.ToolCallBlock{ID: "e1", Name: "edit", Arguments: json.RawMessage(`{}`)}

type watcherHarness struct {
	w     *Watcher
	tr    *transcript
	notes chan string
	mode  Mode
}

func newHarness(t *testing.T, client provider.Client, mode Mode) *watcherHarness {
	t.Helper()
	h := &watcherHarness{tr: &transcript{}, notes: make(chan string, 8), mode: mode}
	h.tr.set(text(provider.RoleUser, "task", time.Unix(1, 0)))
	var reviewer *Reviewer
	if client != nil {
		reviewer = newTestReviewer(client, h.tr)
	}
	h.w = NewWatcher(WatcherConfig{
		Reviewer: reviewer,
		Mode:     func() Mode { return h.mode },
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

// blockingReply answers only once its request is canceled.
func blockingReply(started chan<- struct{}) func(context.Context, provider.Request) (<-chan provider.Event, error) {
	return func(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
		if started != nil {
			close(started)
		}
		<-ctx.Done()
		ch := make(chan provider.Event, 2)
		ch <- provider.EventTextDelta{Delta: "NOTE: late"}
		ch <- provider.EventDone{Err: ctx.Err()}
		close(ch)
		return ch, nil
	}
}

func TestWatcherClientHoldsFirstChangeOnly(t *testing.T) {
	h := newHarness(t, repliesInOrder(t, "STOP: wrong file, the bug is in lexer.go"), Client)
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
	h := newHarness(t, repliesInOrder(t, "NOTE: also update the docs"), Client)
	ctx := context.Background()
	h.w.RunStarted(ctx)
	defer h.w.RunEnded()

	require.False(t, h.w.BeforeTool(ctx, editCall).Hold)
	require.Contains(t, <-h.notes, "also update the docs")
}

func TestWatcherClientFinishReviewedOnceAfterChange(t *testing.T) {
	h := newHarness(t, repliesInOrder(t, "SILENT", "STOP: tests were not run"), Client)
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
	h := newHarness(t, providermocks.NewMockClient(t), Client)
	ctx := context.Background()
	h.w.RunStarted(ctx)
	defer h.w.RunEnded()
	require.Equal(t, core.WatcherVerdict{}, h.w.BeforeFinish(ctx))
}

func TestWatcherFailedReviewLetsExecutorProceed(t *testing.T) {
	client := providermocks.NewMockClient(t)
	client.EXPECT().Stream(mock.Anything, mock.Anything).Return(nil, context.DeadlineExceeded).Once()
	h := newHarness(t, client, Client)
	ctx := context.Background()
	h.w.RunStarted(ctx)
	defer h.w.RunEnded()
	require.Equal(t, core.WatcherVerdict{}, h.w.BeforeTool(ctx, editCall))
}

func TestWatcherCheckpointTimesOutAndProceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := providermocks.NewMockClient(t)
		client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(blockingReply(nil)).Once()
		h := newHarness(t, client, Client)
		ctx := context.Background()
		h.w.RunStarted(ctx)
		defer h.w.RunEnded()
		start := time.Now()
		require.Equal(t, core.WatcherVerdict{}, h.w.BeforeTool(ctx, editCall))
		require.Equal(t, checkpointTimeout, time.Since(start))
	})
}

func TestWatcherCheckpointDoesNotWaitForBackgroundReview(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := providermocks.NewMockClient(t)
		started := make(chan struct{})
		client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(blockingReply(started)).Once()
		client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, provider.Request) (<-chan provider.Event, error) {
			return reply("STOP: wrong file"), nil
		}).Once()
		h := newHarness(t, client, Client)
		ctx := context.Background()
		h.w.RunStarted(ctx)
		defer h.w.RunEnded()

		h.w.StepCompleted()
		<-started
		start := time.Now()
		v := h.w.BeforeTool(ctx, editCall)
		require.True(t, v.Hold)
		require.Zero(t, time.Since(start), "the checkpoint cancels the background review instead of waiting")
		synctest.Wait()
		require.Empty(t, h.notes, "the canceled background review delivers nothing")
	})
}

func TestWatcherBackgroundNoteIsQueued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, repliesInOrder(t, "NOTE: you are rereading the same file"), Client)
		h.w.RunStarted(context.Background())
		defer h.w.RunEnded()

		h.w.StepCompleted()
		synctest.Wait()
		require.Len(t, h.notes, 1)
		note := <-h.notes
		require.True(t, strings.HasPrefix(note, "[advisor note]"), note)
		require.Contains(t, note, "rereading the same file")
	})
}

func TestWatcherBackgroundReviewStopsWithRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := providermocks.NewMockClient(t)
		started := make(chan struct{})
		client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(blockingReply(started)).Once()
		h := newHarness(t, client, Client)
		h.w.RunStarted(context.Background())
		h.w.StepCompleted()
		<-started
		h.w.RunEnded()
		synctest.Wait()
		require.Empty(t, h.notes, "a canceled run never receives a late note")
	})
}

func TestWatcherNextRunReviewsAfterStaleBackground(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := providermocks.NewMockClient(t)
		started := make(chan struct{})
		client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(blockingReply(started)).Once()
		client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, provider.Request) (<-chan provider.Event, error) {
			return reply("NOTE: fresh"), nil
		}).Once()
		h := newHarness(t, client, Client)
		h.w.RunStarted(context.Background())
		h.w.StepCompleted()
		<-started
		h.w.RunEnded()

		h.w.RunStarted(context.Background())
		defer h.w.RunEnded()
		h.w.StepCompleted()
		synctest.Wait()
		require.Len(t, h.notes, 1, "the new run's first review is not lost to the old goroutine")
		require.Contains(t, <-h.notes, "fresh")
	})
}

func TestWatcherCapsBackgroundNotesPerRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := providermocks.NewMockClient(t)
		client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, provider.Request) (<-chan provider.Event, error) {
			return reply("NOTE: something"), nil
		}).Times(maxBackgroundNotes)
		h := newHarness(t, client, Client)
		h.w.RunStarted(context.Background())
		defer h.w.RunEnded()

		for i := 0; i < 20; i++ {
			h.w.StepCompleted()
			synctest.Wait()
		}
		require.Len(t, h.notes, maxBackgroundNotes)
	})
}

func TestWatcherBackgroundResumesAfterFinishCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, repliesInOrder(t, "SILENT", "STOP: tests not run", "NOTE: still no tests"), Client)
		ctx := context.Background()
		h.w.RunStarted(ctx)
		defer h.w.RunEnded()

		require.False(t, h.w.BeforeTool(ctx, editCall).Hold)
		require.True(t, h.w.BeforeFinish(ctx).Hold)
		// Core continues after the hold: the work that follows is reviewed.
		h.w.StepCompleted()
		synctest.Wait()
		require.Len(t, h.notes, 1)
		require.Contains(t, <-h.notes, "still no tests")
	})
}

func TestWatcherOffModeNeverReviews(t *testing.T) {
	h := newHarness(t, providermocks.NewMockClient(t), Off)
	ctx := context.Background()
	h.w.RunStarted(ctx)
	defer h.w.RunEnded()
	h.w.StepCompleted()
	require.Equal(t, core.WatcherVerdict{}, h.w.BeforeTool(ctx, editCall))
	require.Equal(t, core.WatcherVerdict{}, h.w.BeforeFinish(ctx))
	require.Empty(t, h.notes)
}

func TestWatcherNativeAsksExecutorToConsultAtCheckpoints(t *testing.T) {
	h := newHarness(t, nil, Native)
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
	h := newHarness(t, nil, Native)
	ctx := context.Background()
	h.w.RunStarted(ctx)
	defer h.w.RunEnded()
	h.tr.add(provider.Message{Role: provider.RoleAssistant, Time: time.Unix(2, 0), Content: []provider.Content{
		provider.ServerToolBlock{Provider: "anthropic", Kind: "server_tool_use", ID: "s1", Name: "advisor"},
	}})
	require.False(t, h.w.BeforeTool(ctx, editCall).Hold, "already consulted in this request")
	require.False(t, h.w.BeforeFinish(ctx).Hold, "consulted after the last change")
}

func TestConsultedSinceIgnoresStartPastCompactedTranscript(t *testing.T) {
	msgs := []provider.Message{{Role: provider.RoleAssistant, Content: []provider.Content{
		provider.ServerToolBlock{Name: "advisor"},
	}}}
	require.False(t, consultedSince(msgs, 5))
	require.True(t, consultedSince(msgs, 0))
}

func TestWatcherFinishWithoutChangeStopsBackgroundReview(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := providermocks.NewMockClient(t)
		started := make(chan struct{})
		client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(blockingReply(started)).Once()
		h := newHarness(t, client, Client)
		ctx := context.Background()
		h.w.RunStarted(ctx)
		defer h.w.RunEnded()
		h.w.StepCompleted()
		<-started
		require.Equal(t, core.WatcherVerdict{}, h.w.BeforeFinish(ctx))
		synctest.Wait()
		require.Empty(t, h.notes, "no note lands after a final read-only answer")
	})
}
