package session

import (
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestResidentActivityCoalescesAndPublishesTrailingState(t *testing.T) {
	clock := newMockresidentTimerClock(t)
	timer := newMockresidentTimer(t)
	var fire func()
	clock.EXPECT().AfterFunc(residentActivityInterval, mock.Anything).RunAndReturn(func(_ time.Duration, f func()) residentTimer { fire = f; return timer }).Twice()
	m := newMockResidentHostLocal(t)
	var rev uint64
	m.EXPECT().Revision().RunAndReturn(func() uint64 { return rev })
	m.EXPECT().Snapshot().RunAndReturn(func() []subagents.ResidentSnapshot {
		return []subagents.ResidentSnapshot{{ID: "child", State: subagents.ResidentRunning, HistoryRev: rev}}
	})
	c := &Controller{deps: Deps{Clock: time.Now, ResidentManager: m}, ring: &eventRing{}, subs: make(map[uint64]*subscription)}
	s := &c.res
	s.controller, s.timerClock, s.wake = c, clock, make(chan struct{}, 1)
	c.residentSeam = c
	mark := func() { s.inboxMu.Lock(); s.markActivityLocked(); s.inboxMu.Unlock() }
	for range 100 {
		rev++
		mark()
		s.runPending()
	}
	require.Zero(t, c.seq, "activity must not publish before timer")
	fire()
	rev++
	mark() // Last update after timer fires but before actor runs is included.
	s.runPending()
	require.Equal(t, uint64(1), c.seq)
	require.Equal(t, rev, c.Info().Residents[0].HistoryRev)
	rev++
	mark()
	s.runPending()
	require.Equal(t, uint64(1), c.seq)
	fire()
	s.runPending()
	require.Equal(t, uint64(2), c.seq)
	require.Equal(t, rev, c.Info().Residents[0].HistoryRev)
}

func TestResidentActivityDoesNotDelayImmediateMailboxWork(t *testing.T) {
	clock := newMockresidentTimerClock(t)
	timer := newMockresidentTimer(t)
	clock.EXPECT().AfterFunc(residentActivityInterval, mock.Anything).Return(timer).Once()
	c := &Controller{deps: Deps{Clock: time.Now}, ring: &eventRing{}, subs: make(map[uint64]*subscription)}
	turn := newMockturnSeam(t)
	turn.EXPECT().drainQueued().Once()
	turn.EXPECT().isBusy().Return(false)
	turn.EXPECT().turnOrigin().Return(InputOriginUser)
	turn.EXPECT().turnInfo().Return(turnInfoProjection{})
	c.turnSeam = turn
	s := &c.res
	s.controller, s.timerClock, s.wake = c, clock, make(chan struct{}, 1)
	s.inboxMu.Lock()
	s.markActivityLocked()
	// Completion observers post this immediate publication through the mailbox.
	c.postResidentLocked(func() { c.publish(EvInfo, InfoEvent{Info: c.residentInfo()}) })
	s.inboxMu.Unlock()
	s.runPending()
	require.Equal(t, uint64(1), c.seq)
	require.True(t, s.dirty, "activity still has its trailing publication pending")
}
