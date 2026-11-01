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
						s.deliver(session.Event{Seq: 1, Kind: session.EvAttach, Data: session.AttachEvent{Reason: "slow_consumer"}})
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

func TestRecoveryIgnoresStaleFramesBeforeReplay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &subscription{port: &RemotePort{c: &Client{}}, last: 256, out: make(chan session.Event, SubscriberBuffer+1), stop: make(chan struct{}), resub: make(chan struct{}, 1)}
		// Old daemon outbox frames can precede a new attach's replay. They must
		// not pause delivery and discard the contiguous replay that follows.
		s.deliver(session.Event{Seq: 681, Kind: session.EvTitle})
		s.deliver(session.Event{Seq: 257, Kind: session.EvTitle})
		require.Equal(t, uint64(257), s.lastSeq())
		require.False(t, s.paused)
		require.Equal(t, uint64(257), (<-s.out).Seq)
	})
}

func TestRecoveryGapFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &subscription{port: &RemotePort{c: &Client{}}, out: make(chan session.Event, SubscriberBuffer+1), stop: make(chan struct{}), resub: make(chan struct{}, 1)}
		s.deliver(session.Event{Seq: 9, Kind: session.EvTitle})
		synctest.Wait()
		s.mu.Lock()
		require.False(t, s.paused)
		s.mu.Unlock()
		timer := time.NewTimer(reconnectMax)
		<-timer.C
		synctest.Wait()
		s.mu.Lock()
		require.True(t, s.paused)
		s.mu.Unlock()
		select {
		case <-s.resub:
		default:
			t.Fatal("missing replay did not request recovery")
		}
	})
}

func TestRecoveryStaleGapCallbackKeepsNewerTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &subscription{port: &RemotePort{c: &Client{}}, out: make(chan session.Event, SubscriberBuffer+1), stop: make(chan struct{}), resub: make(chan struct{}, 1)}
		s.deliver(session.Event{Seq: 5, Kind: session.EvTitle}) // gap arms T1 at last=0
		s.mu.Lock()
		t1 := s.gap
		s.mu.Unlock()
		s.deliver(session.Event{Seq: 1, Kind: session.EvTitle}) // contiguous: clears T1
		s.deliver(session.Event{Seq: 9, Kind: session.EvTitle}) // new gap arms T2 at last=1
		s.gapFired(t1, 0)                                       // stale T1 callback must leave T2 armed
		time.Sleep(reconnectMax)
		synctest.Wait()
		select {
		case <-s.resub:
		default:
			t.Fatal("newer gap timer was lost")
		}
	})
}

func TestRecoveryGapSupersededNeverPauses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &subscription{port: &RemotePort{c: &Client{}}, last: 1, out: make(chan session.Event, SubscriberBuffer+1), stop: make(chan struct{}), resub: make(chan struct{}, 1)}
		s.deliver(session.Event{Seq: 5, Kind: session.EvTitle})
		s.deliver(session.Event{Seq: 2, Kind: session.EvTitle})
		time.Sleep(2 * reconnectMax)
		synctest.Wait()
		s.mu.Lock()
		defer s.mu.Unlock()
		require.False(t, s.paused)
		require.Nil(t, s.gap)
		require.Empty(t, s.resub)
	})
}

func TestReattachClearsGapTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &subscription{port: &RemotePort{c: &Client{}}, out: make(chan session.Event, SubscriberBuffer+1), stop: make(chan struct{}), resub: make(chan struct{}, 1)}
		s.deliver(session.Event{Seq: 9, Kind: session.EvTitle})
		require.NotNil(t, s.gap)
		close(s.stop)
		_ = s.reattach(context.Background(), nil)
		require.Nil(t, s.gap)
	})
}
