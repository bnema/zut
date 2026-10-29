package modes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func startPortSettings(t *testing.T, info session.Info) (*sessionmocks.MockPort, *portRun, *keyTerminal) {
	t.Helper()
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 4)
	info.ID, info.Run = "s1", session.RunStateIdle
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: info}, nil).Once()
	expectPortSubscribe(port, events, nil)
	r, term := startPortKeys(t, port)
	return port, r, term
}

func TestInteractivePortModelCommandSetsModel(t *testing.T) {
	port, _, term := startPortSettings(t, session.Info{})
	called := make(chan struct{})
	var gotProv, gotModel string
	port.EXPECT().SetModel(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, p, m string) error {
		gotProv, gotModel = p, m
		close(called)
		return nil
	}).Once()
	term.send("/model acme/m1\r")
	waitSignal(t, called, "SetModel")
	require.Equal(t, "acme", gotProv)
	require.Equal(t, "m1", gotModel)
}

func TestInteractivePortFastTogglesFromInfo(t *testing.T) {
	port, r, term := startPortSettings(t, session.Info{FastMode: true})
	called := make(chan struct{})
	got := true
	port.EXPECT().SetFastMode(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, enabled bool) error {
		got = enabled
		close(called)
		return nil
	}).Once()
	require.Eventually(t, func() bool { return r.i.SessionInfo().FastMode }, 5*time.Second, 10*time.Millisecond)
	term.send("/fast\r")
	waitSignal(t, called, "SetFastMode")
	require.False(t, got)
}

func TestInteractivePortProfileAppliesProfile(t *testing.T) {
	port, _, term := startPortSettings(t, session.Info{})
	called := make(chan struct{})
	port.EXPECT().ApplyProfile(mock.Anything, 2).RunAndReturn(func(context.Context, int) error {
		close(called)
		return nil
	}).Once()
	term.send("/profile 2\r")
	waitSignal(t, called, "ApplyProfile")
}

func TestInteractivePortProfileErrorShown(t *testing.T) {
	port, r, term := startPortSettings(t, session.Info{})
	port.EXPECT().ApplyProfile(mock.Anything, 3).Return(errors.New("daemonrefused")).Once()
	term.send("/profile 3\r")
	r.waitOutput(t, "profile 3: daemonrefused")
}

// Without Run, so the direct call does not race the render loop.
func TestInteractivePortReasoningPickSetsReasoning(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	called := make(chan struct{})
	port.EXPECT().SetReasoning(mock.Anything, "high").RunAndReturn(func(context.Context, string) error {
		close(called)
		return nil
	}).Once()
	i := NewInteractive(InteractiveConfig{Terminal: &alertTestTerminal{}, Port: port})
	i.applySettingChange(settingsAction{Key: "reasoning", StringValue: "high"})
	waitSignal(t, called, "SetReasoning")
}

func TestInteractivePortSettingToggleAppliesPatch(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	called := make(chan struct{})
	var got session.SettingsPatch
	port.EXPECT().ApplySettings(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, p session.SettingsPatch) error {
		got = p
		close(called)
		return nil
	}).Once()
	i := NewInteractive(InteractiveConfig{Terminal: &alertTestTerminal{}, Port: port})
	i.applySettingChange(settingsAction{Toggle: true, Key: "lsp_enabled", Value: false})
	waitSignal(t, called, "ApplySettings")
	require.NotNil(t, got.LSPEnabled)
	require.False(t, *got.LSPEnabled)
	require.Nil(t, i.cfg.LSPEnabled, "local state must wait for EvInfo")
}
