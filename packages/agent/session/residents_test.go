package session_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	. "github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestResidentsCompletionDelivery(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "busy"}[busy], func(t *testing.T) {
			m := sessionmocks.NewMockResidentHost(t)
			var accepted func(subagents.ResidentChildSpec, string, string)
			var completed func(subagents.ResidentCompletion) bool
			var update func(string)
			m.EXPECT().SetAcceptedObserver(mock.Anything).Run(func(f func(subagents.ResidentChildSpec, string, string)) { accepted = f })
			m.EXPECT().SetCompletionObserver(mock.Anything).Run(func(f func(subagents.ResidentCompletion) bool) { completed = f })
			m.EXPECT().SetUpdateObserver(mock.Anything).Run(func(f func(string)) { update = f })
			m.EXPECT().Snapshot().Return([]subagents.ResidentSnapshot{{ID: "child", State: subagents.ResidentRunning, Profile: "worker"}})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			c := NewController(ctx, Deps{ResidentManager: m})
			t.Cleanup(func() { cancel(); <-c.Done() })
			events, err := c.Subscribe(ctx, 0)
			require.NoError(t, err)
			require.False(t, completed(subagents.ResidentCompletion{ChildID: "unknown", TurnID: "turn"}))
			accepted(subagents.ResidentChildSpec{ID: "child", Workspace: "/workspace"}, "turn", strings.Repeat("界", 130)+"\nignored")
			accepted(subagents.ResidentChildSpec{ID: "child"}, "turn", "duplicate")
			update("child")
			// Acceptance publishes info once the actor ran it.
			for accepted := false; !accepted; {
				select {
				case ev := <-events:
					info, ok := ev.Data.(InfoEvent)
					accepted = ok && len(info.Info.Residents) > 0 && info.Info.Residents[0].Workspace != ""
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			snapshot, err := c.Snapshot(ctx)
			require.NoError(t, err)
			require.Equal(t, "/workspace", snapshot.Info.Residents[0].Workspace)
			require.Len(t, []rune(snapshot.Info.Residents[0].Task), 120)
			completion := subagents.ResidentCompletion{ChildID: "child", TurnID: "turn", Summary: "finished"}
			require.True(t, completed(completion))
			require.False(t, completed(completion))
			cancel()
			<-c.Done()
			require.False(t, completed(completion))
		})
	}
}

func TestResidentActions(t *testing.T) {
	m := sessionmocks.NewMockResidentHost(t)
	m.EXPECT().SetAcceptedObserver(mock.Anything)
	m.EXPECT().SetCompletionObserver(mock.Anything)
	m.EXPECT().SetUpdateObserver(mock.Anything)
	m.EXPECT().Snapshot().Return(nil)
	ctx, cancel := context.WithCancel(context.Background())
	c := NewController(ctx, Deps{ResidentManager: m})
	t.Cleanup(func() { cancel(); <-c.Done() })
	failure := errors.New("child error")
	m.EXPECT().Stop(ctx, "child").Return(failure)
	m.EXPECT().Interrupt(ctx, "child").Return(true, nil)
	m.EXPECT().Resume(ctx, "child", "follow up").Return(nil)
	require.ErrorIs(t, c.ResidentAction(ctx, ResidentCommand{Op: "stop", ChildID: "child"}), failure)
	require.NoError(t, c.ResidentAction(ctx, ResidentCommand{Op: "interrupt", ChildID: "child"}))
	require.NoError(t, c.ResidentAction(ctx, ResidentCommand{Op: "resume", ChildID: "child", Prompt: "follow up"}))
	require.Error(t, c.ResidentAction(ctx, ResidentCommand{Op: "unknown"}))
	require.Error(t, (&Controller{}).ResidentAction(ctx, ResidentCommand{Op: "stop"}))
}

func TestResidentConcurrentReports(t *testing.T) {
	m := sessionmocks.NewMockResidentHost(t)
	var accepted func(subagents.ResidentChildSpec, string, string)
	var completed func(subagents.ResidentCompletion) bool
	m.EXPECT().SetAcceptedObserver(mock.Anything).Run(func(f func(subagents.ResidentChildSpec, string, string)) { accepted = f })
	m.EXPECT().SetCompletionObserver(mock.Anything).Run(func(f func(subagents.ResidentCompletion) bool) { completed = f })
	m.EXPECT().SetUpdateObserver(mock.Anything)
	m.EXPECT().Snapshot().Return(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := NewController(ctx, Deps{ResidentManager: m})
	t.Cleanup(func() { cancel(); <-c.Done() })
	accepted(subagents.ResidentChildSpec{ID: "child"}, "turn", "task")
	results := make(chan bool, 16)
	for n := 0; n < 16; n++ {
		go func() { results <- completed(subagents.ResidentCompletion{ChildID: "child", TurnID: "turn"}) }()
	}
	count := 0
	for n := 0; n < 16; n++ {
		select {
		case ok := <-results:
			if ok {
				count++
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	require.Equal(t, 1, count)
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	select {
	case ev := <-events:
		require.Equal(t, EvInfo, ev.Kind)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestResidentQueries(t *testing.T) {
	m := sessionmocks.NewMockResidentHost(t)
	m.EXPECT().SetAcceptedObserver(mock.Anything)
	m.EXPECT().SetCompletionObserver(mock.Anything)
	m.EXPECT().SetUpdateObserver(mock.Anything)
	m.EXPECT().Snapshot().Return(nil)
	ctx, cancel := context.WithCancel(context.Background())
	c := NewController(ctx, Deps{ResidentManager: m})
	t.Cleanup(func() { cancel(); <-c.Done() })
	rows := []subagents.ResidentSnapshot{{ID: "child", State: subagents.ResidentRunning}}
	m.EXPECT().RecentSnapshotPage(2, 3).Return(rows, 7).Once()
	got, total, err := c.ResidentList(ctx, 2, 3)
	require.NoError(t, err)
	require.Equal(t, rows, got)
	require.Equal(t, 7, total)
	page := subagents.ResidentHistoryPage{Items: []subagents.ResidentHistoryItem{{Type: "assistant", Message: []byte(`{"role":"assistant","content":"hello"}`)}}, OlderCursor: "older"}
	m.EXPECT().HistoryPage("child", "cursor", 5).Return(page, nil).Once()
	history, err := c.ResidentHistory(ctx, "child", "cursor", 5)
	require.NoError(t, err)
	require.Equal(t, page, history)
	failure := errors.New("invalid child or cursor")
	m.EXPECT().HistoryPage("unknown", "bad", 0).Return(subagents.ResidentHistoryPage{}, failure).Once()
	_, err = c.ResidentHistory(ctx, "unknown", "bad", 0)
	require.ErrorIs(t, err, failure)
	_, _, err = (&Controller{}).ResidentList(ctx, 0, 0)
	require.ErrorContains(t, err, "unavailable")
	_, err = (&Controller{}).ResidentHistory(ctx, "child", "", 0)
	require.ErrorContains(t, err, "unavailable")
	canceled, stop := context.WithCancel(ctx)
	stop()
	_, _, err = c.ResidentList(canceled, 0, 0)
	require.ErrorIs(t, err, context.Canceled)
	_, err = c.ResidentHistory(canceled, "child", "", 0)
	require.ErrorIs(t, err, context.Canceled)
}
