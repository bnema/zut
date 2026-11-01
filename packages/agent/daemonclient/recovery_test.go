package daemonclient

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/stretchr/testify/require"
)

func TestRecoveryWaitRateLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &subscription{out: make(chan session.Event, SubscriberBuffer+1), stop: make(chan struct{})}
		start := time.Now()
		const attempts = 6
		for i := 0; i < attempts; i++ {
			done := make(chan error, 1)
			go func() { done <- s.waitDrained(context.Background()) }()
			synctest.Wait()
			select {
			case <-done:
				t.Fatal("recovery retried without its minimum delay")
			default:
			}
			// Timers in a synctest bubble advance virtual time, not wall time.
			timer := time.NewTimer(reconnectMin)
			<-timer.C
			require.NoError(t, <-done)
			require.Equal(t, time.Duration(i+1)*reconnectMin, time.Since(start))
		}
	})
}

func TestPausedRecoveryTerminates(t *testing.T) {
	for _, takeover := range []bool{false, true} {
		name := "cancel"
		if takeover {
			name = "detached_by_other"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				c := &Client{subs: make(map[session.ID]*subscription)}
				p := &RemotePort{c: c, id: "paused"}
				s := &subscription{port: p, out: make(chan session.Event, SubscriberBuffer+1), stop: make(chan struct{}), resub: make(chan struct{}, 1)}
				c.subs[p.id] = s
				for seq := uint64(1); seq <= SubscriberBuffer+1; seq++ {
					s.deliver(session.Event{Seq: seq, Session: p.id, Kind: session.EvTitle})
				}
				require.True(t, s.paused)
				left, right := net.Pipe()
				defer left.Close()
				defer right.Close()
				conn := daemonproto.NewConn(left, daemonproto.Options{})
				defer conn.Close()
				exited := make(chan struct{})
				go func() { defer close(exited); s.supervise(ctx, conn) }()
				synctest.Wait()
				// supervise is now waiting for local capacity, not reading its outer select.
				if takeover {
					c.onNotification(daemonproto.Notification{Type: "detached_by_other", Session: p.id})
				} else {
					cancel()
				}
				synctest.Wait()
				select {
				case <-exited:
				default:
					t.Fatal("paused supervisor leaked")
				}
				var events []session.Event
				for ev := range s.out {
					events = append(events, ev)
				}
				want := SubscriberBuffer
				if takeover {
					want++
				}
				require.Len(t, events, want)
				if takeover {
					require.Equal(t, session.AttachEvent{State: session.AttachStateDetached, Reason: "detached_by_other"}, events[len(events)-1].Data)
				}
				require.Empty(t, c.subs)
				// A late event cannot revive a terminal stream.
				s.deliver(session.Event{Seq: SubscriberBuffer + 2, Kind: session.EvTitle})
				_, ok := <-s.out
				require.False(t, ok)
			})
		})
	}
}
