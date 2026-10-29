package session_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/extensions"
	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
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
	h.SubmitSlash("/unknown value")
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
