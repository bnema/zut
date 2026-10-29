package session_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/extensions"
	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func extController(t *testing.T, deps session.Deps) (*session.Controller, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := session.NewController(ctx, deps)
	t.Cleanup(func() { cancel(); <-c.Done() })
	return c, ctx
}
func TestExtensionHooksChromeAndEvents(t *testing.T) {
	c, ctx := extController(t, session.Deps{})
	h := c.ExtensionHooks()
	h.Notify("a", "warn", "notice")
	h.Display("b", "display")
	h.Insert("insert")
	h.SetStatus("a", "s", "info", "ready")
	lines := []string{"original"}
	h.SetWidget("a", "w", "above_input", "widget", lines)
	h.OpenPanel("a", extproto.PanelSpec{ID: "p", Lines: lines})
	lines[0] = "caller mutation"
	h.UpdatePanel("a", "p", "title", []string{"updated"}, "footer")
	h.(extensions.AlertHostHooks).Alert("a", extproto.AlertRequest{Kind: "attention", Reason: "reason"})
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snap.Notes, 2)
	require.Equal(t, "ready", snap.ExtChrome.Statuses["a"]["s"].Text)
	require.Equal(t, "original", snap.ExtChrome.Widgets["a"]["w"].Lines[0])
	require.Equal(t, "updated", snap.ExtChrome.Panels[0].Spec.Lines[0])
	require.Equal(t, "reason", snap.ExtChrome.Alert.Reason)
	snap.ExtChrome.Widgets["a"]["w"].Lines[0] = "snapshot mutation"
	snap.ExtChrome.Panels[0].Spec.Lines[0] = "snapshot mutation"
	snap2, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, "original", snap2.ExtChrome.Widgets["a"]["w"].Lines[0])
	require.Equal(t, "updated", snap2.ExtChrome.Panels[0].Spec.Lines[0])
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	kinds := []session.EventKind{session.EvNote, session.EvExtDisplay, session.EvExtInsert, session.EvExtChrome, session.EvExtChrome, session.EvExtPanel, session.EvExtPanel, session.EvExtAlert}
	for _, kind := range kinds {
		select {
		case ev := <-events:
			require.Equal(t, kind, ev.Kind)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	h.(extensions.ExtensionChromeHooks).ClearExtensionChrome("a")
	snap, err = c.Snapshot(ctx)
	require.NoError(t, err)
	require.Empty(t, snap.ExtChrome.Statuses)
	require.Empty(t, snap.ExtChrome.Widgets)
	require.Empty(t, snap.ExtChrome.Panels)
	require.Nil(t, snap.ExtChrome.Alert)
	require.Equal(t, []session.Note{{Level: "info", Text: "[b] display"}}, snap.Notes)
	h.ClearNotes("b")
	snap, err = c.Snapshot(ctx)
	require.NoError(t, err)
	require.Empty(t, snap.Notes)
}
func TestExtensionSlashRouting(t *testing.T) {
	c, ctx := extController(t, session.Deps{})
	h := c.ExtensionHooks()
	require.False(t, h.(extensions.InteractiveToolHostHooks).SupportsInteractiveTools())
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	h.SubmitSlash("/unknown value")
	select {
	case <-events:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, "[extensions] /unknown value", snap.Notes[0].Text)

}
func TestExtensionManagerOperations(t *testing.T) {
	m := sessionmocks.NewMockExtensionHost(t)
	c, ctx := extController(t, session.Deps{Extensions: m})
	m.EXPECT().SendPanelKey("a", "p", "enter", "text").Return(nil).Once()
	require.NoError(t, c.ExtensionPanelInput(ctx, "a", "p", extproto.PanelKeyFromHost{Key: "enter", Text: "text"}))
	c.ExtensionHooks().OpenPanel("a", extproto.PanelSpec{ID: "p"})
	m.EXPECT().SendPanelClose("a", "p").Return(nil).Once()
	require.NoError(t, c.ExtensionPanelInput(ctx, "a", "p", extproto.PanelKeyFromHost{Type: "panel_close"}))
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Empty(t, snap.ExtChrome.Panels)
	m.EXPECT().HasCommand("cmd").Return(true)
	m.EXPECT().CommandOwner("cmd").Return("a")
	m.EXPECT().Invoke(ctx, "cmd", "args", 30*time.Second).Return(extproto.CommandResponseFromExt{Action: "display", Display: "result"}, nil).Once()
	require.NoError(t, c.InvokeExtensionCommand(ctx, "cmd", "args"))
	m.EXPECT().Reload(ctx, 5*time.Second).RunAndReturn(func(context.Context, time.Duration) extensions.ReloadStats {
		c.ExtensionHooks().SetStatus("a", "s", "info", "reload callback")
		return extensions.ReloadStats{Loaded: 1, Ready: 1}
	}).Once()
	require.NoError(t, c.ReloadExtensions(ctx))
	snap, err = c.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, "reload callback", snap.ExtChrome.Statuses["a"]["s"].Text)
	require.Equal(t, "[a] result", snap.Notes[0].Text)
	m.EXPECT().Invoke(ctx, "cmd", "fail", 30*time.Second).Return(extproto.CommandResponseFromExt{Error: "failed"}, nil).Once()
	require.EqualError(t, c.InvokeExtensionCommand(ctx, "cmd", "fail"), "failed")
	m.EXPECT().Reload(ctx, 5*time.Second).Return(extensions.ReloadStats{Errors: []error{errors.New("reload failed")}}).Once()
	require.EqualError(t, c.ReloadExtensions(ctx), "reload failed")
	snap, err = c.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, "[extensions] reloaded: 0 stopped, 0 loaded (0 ready), 1 error(s): reload failed", snap.Notes[len(snap.Notes)-1].Text)
}
func TestExtensionRemovals(t *testing.T) {
	c, ctx := extController(t, session.Deps{})
	h := c.ExtensionHooks()
	h.SetStatus("a", "s", "info", "ready")
	h.SetStatus("a", "s", "info", "")
	h.SetWidget("a", "w", "", "", nil)
	h.ClearWidget("a", "w")
	h.OpenPanel("a", extproto.PanelSpec{ID: "p"})
	h.ClosePanel("other", "p")
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Len(t, snap.ExtChrome.Panels, 1)
	h.ClosePanel("a", "p")
	snap, err = c.Snapshot(ctx)
	require.NoError(t, err)
	require.Empty(t, snap.ExtChrome.Panels)
	require.Empty(t, snap.ExtChrome.Widgets)
	require.Empty(t, snap.ExtChrome.Statuses)
}

func TestExtensionDetachedDelegation(t *testing.T) {
	c, ctx := extController(t, session.Deps{})
	p := sessionmocks.NewMockPort(t)
	require.NoError(t, session.SetExtensionTestPort(c, ctx, p))
	completed := make(chan struct{})
	p.EXPECT().Compact(mock.Anything).Return(nil).Once()
	p.EXPECT().SetGoal(mock.Anything, session.GoalCommand{Op: "set", Objective: "ship the feature"}).Return(nil).Once()
	p.EXPECT().SetGoal(mock.Anything, session.GoalCommand{Op: "pause"}).Return(nil).Once()
	p.EXPECT().SetModel(mock.Anything, "provider", "model").Return(nil).Once()
	p.EXPECT().SetModel(mock.Anything, "", "model-only").RunAndReturn(func(context.Context, string, string) error { close(completed); return nil }).Once()
	h := c.ExtensionHooks()
	h.SubmitSlash("/compact")
	h.SubmitSlash("/goal ship the feature")
	h.SubmitSlash("/goal pause")
	h.SubmitSlash("/model provider/model")
	h.SubmitSlash("/model model-only")
	select {
	case <-completed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ch:
			return
		default:
			time.Sleep(time.Millisecond)
		}
	}
	t.Fatal("extension dispatcher goroutine still running")
}

func TestExtensionDomainSaturation(t *testing.T) {
	c, ctx := extController(t, session.Deps{})
	p := sessionmocks.NewMockPort(t)
	require.NoError(t, session.SetExtensionTestPort(c, ctx, p))
	events, err := c.Subscribe(ctx, 0)
	require.NoError(t, err)
	started := make(chan struct{})
	release := make(chan struct{})
	var ran atomic.Int32
	p.EXPECT().Compact(mock.Anything).RunAndReturn(func(context.Context) error {
		ran.Add(1)
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		return nil
	}).Times(65)
	h := c.ExtensionHooks()
	h.SubmitSlash("/compact")
	<-started
	for i := 0; i < 80; i++ {
		h.SubmitSlash("/compact") // 64 fit in the queue; the rest are dropped
	}
	for {
		select {
		case ev := <-events:
			if ev.Kind == session.EvNote {
				n := ev.Data.(session.NoteEvent).Note
				require.Equal(t, session.Note{Level: "error", Text: "extension extensions overloaded: hook dropped"}, n)
				close(release)
				snap, err := c.Snapshot(ctx)
				require.NoError(t, err)
				require.Len(t, snap.Notes, 1)
				// Let the 64 queued compacts drain so mock expectations hold.
				require.Eventually(t, func() bool { return ran.Load() == 65 }, 5*time.Second, time.Millisecond)
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestExtensionShutdownCancelsRunningDomainOp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	loopCtx, stop := context.WithCancel(ctx)
	c := session.NewController(loopCtx, session.Deps{})
	p := sessionmocks.NewMockPort(t)
	require.NoError(t, session.SetExtensionTestPort(c, ctx, p))
	started := make(chan struct{})
	cancelled := make(chan struct{})
	p.EXPECT().Compact(mock.Anything).RunAndReturn(func(opCtx context.Context) error {
		close(started)
		<-opCtx.Done()
		close(cancelled)
		return opCtx.Err()
	}).Once()
	h := c.ExtensionHooks()
	h.SubmitSlash("/compact")
	<-started
	h.SubmitSlash("/goal pending") // accepted but must never run
	time.Sleep(10 * time.Millisecond)
	stop()
	<-c.Done()
	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal("running domain op not cancelled")
	}
	waitClosed(t, session.ExtDispatcherDone(c))
	h.SubmitSlash("/compact")
	h.Notify("a", "info", "after")
	session.ExtShutdown(c) // idempotent
}
