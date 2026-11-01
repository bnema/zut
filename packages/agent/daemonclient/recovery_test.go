package daemonclient

import (
	"context"
	"io"
	"log/slog"
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
	for _, name := range []string{"cancel", "detached_by_other", "closed"} {
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
				if name == "cancel" {
					cancel()
				} else {
					c.onNotification(daemonproto.Notification{Type: name, Session: p.id})
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
				if name != "cancel" {
					want++
				}
				require.Len(t, events, want)
				switch name {
				case "detached_by_other":
					require.Equal(t, session.AttachEvent{State: session.AttachStateDetached, Reason: name}, events[len(events)-1].Data)
				case "closed":
					require.Equal(t, session.EvClosed, events[len(events)-1].Kind)
					require.Equal(t, session.ClosedEvent{}, events[len(events)-1].Data)
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

func TestStreamLoggingDoesNotHoldSubscriptionLock(t *testing.T) {
	for _, finish := range []bool{false, true} {
		name := "deliver"
		if finish {
			name = "finish"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				c := &Client{opts: Options{Logger: slog.New(slog.NewTextHandler(writer, nil))}, subs: make(map[session.ID]*subscription)}
				s := &subscription{port: &RemotePort{c: c, id: "logging"}, out: make(chan session.Event, SubscriberBuffer+1), stop: make(chan struct{}), resub: make(chan struct{}, 1)}
				c.subs[s.port.id] = s
				done := make(chan struct{})
				go func() {
					defer close(done)
					if finish {
						s.finish(nil)
					} else {
						s.deliver(session.Event{Seq: 2, Kind: session.EvTitle}) // gap requests recovery
					}
				}()
				synctest.Wait() // logging is durably blocked on the pipe, with no reader
				require.True(t, s.mu.TryLock(), "logging held the subscription mutex")
				s.mu.Unlock()
				if finish {
					select {
					case <-s.stop:
					default:
						t.Fatal("finish must close the stream before logging")
					}
				} else {
					select {
					case <-s.resub:
					default:
						t.Fatal("recovery must be signaled before logging")
					}
				}
				reader.Close() // unblock the logger and join its caller
				<-done
			})
		})
	}
}
