package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/stretchr/testify/require"
)

func newTestController(t *testing.T) (*Controller, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := NewController(ctx, Deps{})
	t.Cleanup(func() { cancel(); <-c.Done() })
	return c, ctx
}

// newSyncTestController must be called inside a synctest bubble. Its cleanup
// stops the actor before the bubble waits for all remaining goroutines.
func newSyncTestController(t *testing.T) (*Controller, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c := NewController(ctx, Deps{})
	t.Cleanup(func() { cancel(); <-c.Done() })
	return c, ctx
}

func emit(t *testing.T, c *Controller, ctx context.Context, n int) {
	t.Helper()
	if err := c.call(ctx, func() {
		for i := 0; i < n; i++ {
			c.publish(EvTitle, TitleEvent{Title: "test"})
		}
	}); err != nil {
		t.Fatal(err)
	}
}
func TestRingReplay(t *testing.T) {
	c, ctx := newTestController(t)
	emit(t, c, ctx, 3)
	ch, err := c.Subscribe(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for seq := uint64(2); seq <= 3; seq++ {
		select {
		case ev := <-ch:
			if ev.Seq != seq {
				t.Fatalf("seq %d, want %d", ev.Seq, seq)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	snap, err := c.Snapshot(ctx)
	if err != nil || snap.Seq != 3 {
		t.Fatalf("snapshot: %+v %v", snap, err)
	}
}
func TestRingSeqTooOld(t *testing.T) {
	c, ctx := newTestController(t)
	emit(t, c, ctx, EventRingSize+1)
	if _, err := c.Subscribe(ctx, 0); !errors.Is(err, ErrSeqTooOld) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.Subscribe(ctx, 1); err != nil {
		t.Fatal(err)
	}
}
func TestSubscribeAheadOfSeqIsTooOld(t *testing.T) {
	c, ctx := newTestController(t)
	emit(t, c, ctx, 2)
	if _, err := c.Subscribe(ctx, 500); !errors.Is(err, ErrSeqTooOld) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.Subscribe(ctx, 2); err != nil {
		t.Fatal(err)
	}
}
func TestRingByteLimit(t *testing.T) {
	r := new(eventRing)
	r.append(Event{Seq: 1, Kind: EvTitle, Data: TitleEvent{Title: strings.Repeat("x", eventRingBytes/2)}})
	r.append(Event{Seq: 2, Kind: EvTitle, Data: TitleEvent{Title: strings.Repeat("x", eventRingBytes/2)}})
	if r.bytes > eventRingBytes || len(r.entries) != 1 {
		t.Fatalf("bytes=%d entries=%d", r.bytes, len(r.entries))
	}
	if _, err := r.replay(0, 2); !errors.Is(err, ErrSeqTooOld) {
		t.Fatal(err)
	}
}
func TestSlowConsumerDrop(t *testing.T) {
	c, ctx := newTestController(t)
	ch, err := c.Subscribe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	emit(t, c, ctx, subscriberBuffer+2)
	var last Event
	for ev := range ch {
		last = ev
	}
	if last.Kind != EvAttach {
		t.Fatalf("last event: %+v", last)
	}
	payload := last.Data.(AttachEvent)
	if payload.State != AttachStateDetached || payload.Reason != "slow_consumer" {
		t.Fatalf("last payload: %+v", payload)
	}
	// The dropped notification is client-local; authoritative replay retains the event.
	replay, err := c.Subscribe(ctx, subscriberBuffer)
	if err != nil {
		t.Fatal(err)
	}
	if ev := <-replay; ev.Kind != EvTitle {
		t.Fatalf("replay: %+v", ev)
	}
}
func TestConcurrentPostsMonotonic(t *testing.T) {
	c, ctx := newTestController(t)
	const total = 200
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.post(ctx, func() { c.publish(EvTitle, TitleEvent{}) }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	ch, err := c.Subscribe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= total; seq++ {
		select {
		case ev := <-ch:
			if ev.Seq != seq {
				t.Fatalf("got %d want %d", ev.Seq, seq)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
func TestSubscribeCancellationAndShutdown(t *testing.T) {
	c, ctx := newTestController(t)
	subCtx, cancel := context.WithCancel(ctx)
	ch, err := c.Subscribe(subCtx, 0)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("subscription still open")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, err := c.Snapshot(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !c.Idle() {
		t.Fatal("empty skeleton must be idle")
	}
}

func TestSlowConsumerDropLogs(t *testing.T) {
	for _, agent := range []bool{false, true} {
		t.Run(fmt.Sprint("agent=", agent), func(t *testing.T) {
			var logs bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			c := NewControllerWithOptions(ctx, Deps{Logger: slog.New(slog.NewJSONHandler(&logs, nil))}, Options{ID: "slow-session"})
			t.Cleanup(func() { cancel(); <-c.Done() })
			_, err := c.Subscribe(ctx, 0)
			require.NoError(t, err)
			emit(t, c, ctx, subscriberBuffer)
			require.Empty(t, logs.String(), "normal delivery must not log")
			require.NoError(t, c.call(ctx, func() {
				if agent {
					c.publish(EvAgent, core.EvTextDelta{Delta: "private-content"})
				} else {
					c.publish(EvTitle, TitleEvent{Title: "private-content"})
				}
				c.publish(EvTitle, TitleEvent{}) // a dropped subscriber must not log again
			}))
			require.Equal(t, 1, strings.Count(logs.String(), "\n"))
			var record map[string]any
			require.NoError(t, json.Unmarshal(logs.Bytes(), &record))
			require.Equal(t, "WARN", record["level"])
			require.Equal(t, "session subscriber dropped", record["msg"])
			require.Equal(t, "slow_consumer", record["reason"])
			require.Equal(t, "slow-session", record["session"])
			require.Equal(t, float64(1), record["subscriber"])
			require.Equal(t, float64(subscriberBuffer+1), record["seq"])
			require.Equal(t, float64(subscriberBuffer), record["queued"])
			require.Equal(t, float64(subscriberBuffer+1), record["cap"])
			if agent {
				require.Equal(t, string(EvAgent), record["kind"])
				require.Equal(t, "text_delta", record["agent_event_type"])
			} else {
				require.Equal(t, string(EvTitle), record["kind"])
				require.NotContains(t, record, "agent_event_type")
			}
			require.NotContains(t, logs.String(), "private-content")
		})
	}
}
