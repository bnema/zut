package advisor

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

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

func TestWatcherOffModeNeverReviews(t *testing.T) {
	h := newHarness(t, providermocks.NewMockClient(t), Off)
	ctx := context.Background()
	h.w.RunStarted(ctx)
	defer h.w.RunEnded()
	h.w.StepCompleted()
	require.Empty(t, h.notes)
}

func TestWatcherNativeNeverReviews(t *testing.T) {
	h := newHarness(t, providermocks.NewMockClient(t), Native)
	h.w.RunStarted(context.Background())
	defer h.w.RunEnded()
	h.w.StepCompleted()
	require.Empty(t, h.notes)
}

func TestWatcherBackgroundStopIsQueued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, repliesInOrder(t, "STOP: check the caller"), Client)
		h.w.RunStarted(context.Background())
		defer h.w.RunEnded()
		h.w.StepCompleted()
		synctest.Wait()
		require.Contains(t, <-h.notes, "Stop and reconsider")
	})
}
