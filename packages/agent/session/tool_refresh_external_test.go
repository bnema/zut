package session_test

import (
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/stretchr/testify/require"
)

const refreshWait = 10 * time.Second

// fireConcurrently calls RequestToolRefresh n times from n goroutines and
// returns once every call returned (it must never block).
func fireConcurrently(c *session.Controller, n int) {
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() { defer wg.Done(); c.RequestToolRefresh() }()
	}
	wg.Wait()
}

func recv(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(refreshWait):
		t.Fatal("timed out waiting for " + what)
	}
}

func TestRequestToolRefreshCoalescesWhileQueued(t *testing.T) {
	refresh := sessionmocks.NewMockToolRefresher(t)
	rebuilt := make(chan struct{}, 4)
	refresh.EXPECT().RefreshTools().RunAndReturn(func() error { rebuilt <- struct{}{}; return nil }).Once()
	h := session.NewToolRefreshHarness(t, session.Deps{ToolRefresher: refresh})
	release := h.BlockActor(t)
	fireConcurrently(h.C, 20) // never blocks although the actor is busy
	release()
	recv(t, rebuilt, "rebuild")
	require.False(t, h.Pending(t)) // barrier: the actor drained the request
	require.False(t, h.Queued())
	select {
	case <-rebuilt:
		t.Fatal("coalesced requests rebuilt more than once")
	default:
	}
}

func TestRequestToolRefreshDuringRebuildRunsAgain(t *testing.T) {
	refresh := sessionmocks.NewMockToolRefresher(t)
	entered, unblock, rebuilt := make(chan struct{}, 2), make(chan struct{}), make(chan struct{}, 2)
	first := true
	refresh.EXPECT().RefreshTools().RunAndReturn(func() error {
		entered <- struct{}{}
		if first { // only the actor calls this; no race
			first = false
			<-unblock
		}
		rebuilt <- struct{}{}
		return nil
	}).Twice()
	h := session.NewToolRefreshHarness(t, session.Deps{ToolRefresher: refresh})
	h.C.RequestToolRefresh()
	recv(t, entered, "first rebuild start")
	// Arrives mid-rebuild: must not be lost.
	fireConcurrently(h.C, 5)
	close(unblock)
	recv(t, rebuilt, "first rebuild end")
	recv(t, rebuilt, "second rebuild")
	require.False(t, h.Pending(t))
}

func TestRequestToolRefreshDefersWhileBusy(t *testing.T) {
	refresh := sessionmocks.NewMockToolRefresher(t)
	h := session.NewToolRefreshHarness(t, session.Deps{ToolRefresher: refresh})
	h.SetBusy(t, true)
	fireConcurrently(h.C, 10)
	// The hand-off reaches the actor and is recorded, not run (the mock
	// fails on an unexpected call).
	require.Eventually(t, func() bool { return !h.Queued() && h.Pending(t) }, refreshWait, time.Millisecond)
	refresh.EXPECT().RefreshTools().Return(nil).Once()
	h.SetBusy(t, false)
	require.False(t, h.Pending(t))
}
