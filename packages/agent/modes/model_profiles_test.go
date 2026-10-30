package modes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	profSol  = session.QuickModelShortcut{Provider: "openai", Model: "gpt-5.6-sol", Reasoning: "high"}
	profGPT5 = session.QuickModelShortcut{Provider: "openai", Model: "gpt-5.5", FastMode: true}
)

// profileInfo returns an idle Info with nine slots, set filling 1-based slots.
func profileInfo(active int, set map[int]session.QuickModelShortcut) session.Info {
	slots := make([]session.QuickModelShortcut, 9)
	for n, p := range set {
		slots[n-1] = p
	}
	return session.Info{ID: "s1", Provider: "openai", Model: "gpt-5.6-sol", Reasoning: "high", Run: session.RunStateIdle, Profiles: slots, ActiveProfile: active}
}

// newPortProfileInteractive builds a port-mode TUI with info applied, without
// running the loop: keys go straight to handleKey.
func newPortProfileInteractive(t *testing.T, info session.Info) (*Interactive, *sessionmocks.MockPort) {
	t.Helper()
	port := sessionmocks.NewMockPort(t)
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Port: port})
	i.rend = nil
	profSetInfo(i, info)
	return i, port
}

func profSetInfo(i *Interactive, info session.Info) {
	i.mu.Lock()
	i.applyInfoLocked(info)
	i.mu.Unlock()
}

func profKey(i *Interactive, k tui.Key) { i.handleKey(context.Background(), k) }

func profCtrl(n int) tui.Key { return tui.Key{Kind: tui.KeyRune, Rune: rune('0' + n), Ctrl: true} }

func profExpectApply(port *sessionmocks.MockPort, slot int, err error) chan struct{} {
	called := make(chan struct{})
	port.EXPECT().ApplyProfile(mock.Anything, slot).RunAndReturn(func(context.Context, int) error { close(called); return err }).Once()
	return called
}

func profWait(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s not called", what)
	}
}

func profStatusErr(i *Interactive) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.statusErr
}

func profPick(t *testing.T, i *Interactive, prov, model string) {
	t.Helper()
	m, err := provider.FindModel(prov, model)
	require.NoError(t, err)
	i.modelDialog.view = []provider.Model{m}
	i.modelDialog.cursor = 0
	profKey(i, tui.Key{Kind: tui.KeyEnter})
}

func TestProfileInfoDrivesIndicator(t *testing.T) {
	i, _ := newPortProfileInteractive(t, profileInfo(2, map[int]session.QuickModelShortcut{2: profGPT5}))
	require.Equal(t, 2, i.cfg.ActiveModelProfile)
	require.Equal(t, "gpt-5.5", i.cfg.QuickModelShortcuts[1].Model)
	require.True(t, i.cfg.QuickModelShortcuts[1].FastMode)
	profSetInfo(i, profileInfo(0, nil))
	require.Equal(t, 0, i.cfg.ActiveModelProfile)
}

func TestProfileSettingsItemsFromInfo(t *testing.T) {
	i, _ := newPortProfileInteractive(t, profileInfo(1, map[int]session.QuickModelShortcut{1: profSol}))
	require.Contains(t, i.quickModelSettingItem(1).hint, "(active)")
	require.Contains(t, i.quickModelSettingItem(1).hint, "gpt-5.6-sol")
	require.Equal(t, "not assigned", i.quickModelSettingItem(2).hint)
}

func TestCtrlDigitAppliesAssignedProfile(t *testing.T) {
	i, port := newPortProfileInteractive(t, profileInfo(0, map[int]session.QuickModelShortcut{3: profGPT5}))
	called := profExpectApply(port, 3, nil)
	profKey(i, profCtrl(3))
	profWait(t, called, "ApplyProfile")
	require.False(t, i.modelDialog.Active())
}

func TestAzertyCtrlAppliesProfile(t *testing.T) {
	i, port := newPortProfileInteractive(t, profileInfo(0, map[int]session.QuickModelShortcut{1: profSol}))
	called := profExpectApply(port, 1, nil)
	profKey(i, tui.Key{Kind: tui.KeyRune, Rune: '&', Ctrl: true})
	profWait(t, called, "ApplyProfile")
}

func TestProfileCommandApplies(t *testing.T) {
	i, port := newPortProfileInteractive(t, profileInfo(0, map[int]session.QuickModelShortcut{4: profSol}))
	called := profExpectApply(port, 4, nil)
	i.runSlash(context.Background(), "/profile 4")
	profWait(t, called, "ApplyProfile")
}

func TestProfileCommandRejectsInvalidSlots(t *testing.T) {
	for _, command := range []string{"/profile", "/profile 0", "/profile 10", "/profile x", "/profile 1 extra"} {
		i, _ := newPortProfileInteractive(t, profileInfo(0, nil))
		i.runSlash(context.Background(), command)
		require.Contains(t, profStatusErr(i), "usage:", command)
	}
}

func TestEmptySlotOpensActivatingPicker(t *testing.T) {
	i, _ := newPortProfileInteractive(t, profileInfo(1, map[int]session.QuickModelShortcut{1: profSol}))
	profKey(i, profCtrl(5))
	require.True(t, i.modelDialog.Active())
	require.Equal(t, 5, i.quickModelAssign)
	require.True(t, i.quickModelActivate)
}

func TestActivatingPickerAssignsAndActivates(t *testing.T) {
	i, port := newPortProfileInteractive(t, profileInfo(0, nil))
	called := make(chan struct{})
	port.EXPECT().AssignProfile(mock.Anything, 5, mock.MatchedBy(func(p session.QuickModelShortcut) bool {
		return p.Provider == "openai" && p.Model == "gpt-5.5"
	}), true).RunAndReturn(func(context.Context, int, session.QuickModelShortcut, bool) error { close(called); return nil }).Once()
	profKey(i, profCtrl(5))
	profPick(t, i, "openai", "gpt-5.5")
	profWait(t, called, "AssignProfile")
	require.False(t, i.modelDialog.Active())
	require.Zero(t, i.quickModelAssign)
}

func TestCancelActivatingPickerCallsNothing(t *testing.T) {
	i, _ := newPortProfileInteractive(t, profileInfo(0, nil))
	profKey(i, profCtrl(2))
	profKey(i, tui.Key{Kind: tui.KeyEsc})
	require.False(t, i.modelDialog.Active())
	require.Zero(t, i.quickModelAssign)
	require.False(t, i.quickModelActivate)
}

func TestProfileEmptyErrorOpensPicker(t *testing.T) {
	i, port := newPortProfileInteractive(t, profileInfo(0, map[int]session.QuickModelShortcut{6: profSol}))
	called := profExpectApply(port, 6, session.ErrProfileEmpty)
	profKey(i, profCtrl(6))
	profWait(t, called, "ApplyProfile")
	select {
	case fn := <-i.loopCalls:
		fn()
	case <-time.After(5 * time.Second):
		t.Fatal("picker not queued")
	}
	require.True(t, i.modelDialog.Active())
	require.Equal(t, 6, i.quickModelAssign)
	require.True(t, i.quickModelActivate)
	require.Empty(t, profStatusErr(i))
}

func TestApplyProfileErrorShown(t *testing.T) {
	i, port := newPortProfileInteractive(t, profileInfo(0, map[int]session.QuickModelShortcut{1: profSol}))
	called := profExpectApply(port, 1, errors.New("model switched; profile not saved: disk"))
	profKey(i, profCtrl(1))
	profWait(t, called, "ApplyProfile")
	require.Eventually(t, func() bool { return strings.Contains(profStatusErr(i), "profile not saved") }, 5*time.Second, 10*time.Millisecond)
}

func TestBusyGuardFromInfoRun(t *testing.T) {
	for _, run := range []session.RunState{session.RunStateRunning, session.RunStateCompacting} {
		info := profileInfo(1, map[int]session.QuickModelShortcut{1: profSol, 2: profGPT5})
		info.Run = run
		i, _ := newPortProfileInteractive(t, info)
		profKey(i, profCtrl(2))
		require.Contains(t, profStatusErr(i), "cannot switch model while a turn is running")
		require.False(t, i.modelDialog.Active())
	}
}

func TestBusyGuardIgnoresLocalBusy(t *testing.T) {
	i, port := newPortProfileInteractive(t, profileInfo(0, map[int]session.QuickModelShortcut{2: profGPT5}))
	i.busy = true // stale local state; Info says idle
	called := profExpectApply(port, 2, nil)
	profKey(i, profCtrl(2))
	profWait(t, called, "ApplyProfile")
}

func TestOldDaemonWithoutProfilesReportsError(t *testing.T) {
	info := profileInfo(0, nil)
	info.Profiles = nil
	i, _ := newPortProfileInteractive(t, info)
	profKey(i, profCtrl(1))
	require.Contains(t, profStatusErr(i), "newer zut daemon")
	require.False(t, i.modelDialog.Active())
}

func TestUnsupportedPortMethodSurfaces(t *testing.T) {
	i, port := newPortProfileInteractive(t, profileInfo(0, nil))
	called := make(chan struct{})
	port.EXPECT().ClearProfile(mock.Anything, 3).RunAndReturn(func(context.Context, int) error {
		close(called)
		return errors.New(`unsupported port method "clear_profile"`)
	}).Once()
	i.applyQuickModelSetting("quick_model_3", "")
	profWait(t, called, "ClearProfile")
	require.Eventually(t, func() bool { return strings.Contains(profStatusErr(i), "unsupported port method") }, 5*time.Second, 10*time.Millisecond)
}

func TestSettingsAssignDoesNotActivate(t *testing.T) {
	i, port := newPortProfileInteractive(t, profileInfo(1, map[int]session.QuickModelShortcut{1: profSol}))
	called := make(chan struct{})
	port.EXPECT().AssignProfile(mock.Anything, 4, mock.MatchedBy(func(p session.QuickModelShortcut) bool { return p.Model == "gpt-5.5" }), false).
		RunAndReturn(func(context.Context, int, session.QuickModelShortcut, bool) error { close(called); return nil }).Once()
	i.openQuickModelPicker(4)
	profPick(t, i, "openai", "gpt-5.5")
	profWait(t, called, "AssignProfile")
}

func TestSettingsAssignAllowedWhileBusy(t *testing.T) {
	info := profileInfo(0, nil)
	info.Run = session.RunStateRunning
	i, port := newPortProfileInteractive(t, info)
	called := make(chan struct{})
	port.EXPECT().AssignProfile(mock.Anything, 2, mock.Anything, false).
		RunAndReturn(func(context.Context, int, session.QuickModelShortcut, bool) error { close(called); return nil }).Once()
	i.openQuickModelPicker(2)
	profPick(t, i, "openai", "gpt-5.5")
	profWait(t, called, "AssignProfile")
}

func TestSettingsBackspaceClearsProfile(t *testing.T) {
	i, port := newPortProfileInteractive(t, profileInfo(1, map[int]session.QuickModelShortcut{1: profSol}))
	called := make(chan struct{})
	port.EXPECT().ClearProfile(mock.Anything, 1).RunAndReturn(func(context.Context, int) error { close(called); return nil }).Once()
	i.applyQuickModelSetting("quick_model_1", "")
	profWait(t, called, "ClearProfile")
}

func TestPickerReasoningCarriedIntoAssign(t *testing.T) {
	i, port := newPortProfileInteractive(t, profileInfo(0, nil))
	called := make(chan struct{})
	port.EXPECT().AssignProfile(mock.Anything, 7, mock.MatchedBy(func(p session.QuickModelShortcut) bool { return p.Reasoning == "low" }), false).
		RunAndReturn(func(context.Context, int, session.QuickModelShortcut, bool) error { close(called); return nil }).Once()
	i.openQuickModelPicker(7)
	i.modelDialog.reasoning = "low"
	profPick(t, i, "openai", "gpt-5.5")
	profWait(t, called, "AssignProfile")
}

func TestTabCyclesForward(t *testing.T) {
	i, port := newPortProfileInteractive(t, profileInfo(1, map[int]session.QuickModelShortcut{1: profSol, 2: profGPT5}))
	called := profExpectApply(port, 2, nil)
	i.ed.SetValue("hello")
	profKey(i, tui.Key{Kind: tui.KeyTab})
	profWait(t, called, "ApplyProfile")
	require.Equal(t, "hello", i.ed.Value())
}

func TestTabWrapsAround(t *testing.T) {
	i, port := newPortProfileInteractive(t, profileInfo(9, map[int]session.QuickModelShortcut{1: profSol, 9: profGPT5}))
	called := profExpectApply(port, 1, nil)
	profKey(i, tui.Key{Kind: tui.KeyTab})
	profWait(t, called, "ApplyProfile")
}

func TestShiftTabWithoutActiveStartsAtNine(t *testing.T) {
	i, _ := newPortProfileInteractive(t, profileInfo(0, nil))
	profKey(i, tui.Key{Kind: tui.KeyShiftTab})
	require.True(t, i.modelDialog.Active())
	require.Equal(t, 9, i.quickModelAssign)
}

func TestTabOnPathKeepsPathCompletion(t *testing.T) {
	i, _ := newPortProfileInteractive(t, profileInfo(0, nil))
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "alpha.txt"), []byte("x"), 0o600))
	i.cfg.CWD = dir
	i.ed.SetValue("./al")
	profKey(i, tui.Key{Kind: tui.KeyTab})
	require.Equal(t, "./alpha.txt", i.ed.Value())
	require.False(t, i.modelDialog.Active())
}

func TestTabWithSlashPopupCompletesCommand(t *testing.T) {
	i, _ := newPortProfileInteractive(t, profileInfo(0, nil))
	i.ed.SetValue("/mod")
	profKey(i, tui.Key{Kind: tui.KeyTab})
	require.Equal(t, "/model", i.ed.Value())
	require.False(t, i.modelDialog.Active())
}

func TestModifiedTabDoesNotCycle(t *testing.T) {
	i, _ := newPortProfileInteractive(t, profileInfo(0, nil))
	for _, k := range []tui.Key{
		{Kind: tui.KeyTab, Ctrl: true}, {Kind: tui.KeyTab, Alt: true}, {Kind: tui.KeyTab, Super: true}, {Kind: tui.KeyTab, Shift: true},
		{Kind: tui.KeyShiftTab, Ctrl: true}, {Kind: tui.KeyShiftTab, Alt: true}, {Kind: tui.KeyShiftTab, Super: true},
	} {
		i.ed.Clear()
		profKey(i, k)
		require.False(t, i.modelDialog.Active(), "%+v", k)
		require.Zero(t, i.quickModelAssign)
	}
}

func TestShortcutLayouts(t *testing.T) {
	for _, row := range []string{"123456789", "&é\"'(-è_ç"} {
		for idx, r := range []rune(row) {
			require.Equal(t, idx+1, quickModelShortcutSlot(tui.Key{Kind: tui.KeyRune, Rune: r, Ctrl: true}))
			for _, k := range []tui.Key{{Kind: tui.KeyRune, Rune: r}, {Kind: tui.KeyRune, Rune: r, Ctrl: true, Alt: true}, {Kind: tui.KeyPaste, Paste: string(r)}} {
				require.Zero(t, quickModelShortcutSlot(k))
			}
		}
	}
}
