package modes

import (
	"context"
	"errors"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/stretchr/testify/mock"
)

func newCmdPort(t *testing.T, cwd string) *sessionmocks.MockPort {
	t.Helper()
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 4)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", CWD: cwd, Run: session.RunStateIdle}}, nil).Once()
	expectPortSubscribe(port, events, nil)
	return port
}

func TestInteractivePortCompact(t *testing.T) {
	port := newCmdPort(t, "/w")
	called := make(chan struct{})
	port.EXPECT().Compact(mock.Anything).RunAndReturn(func(context.Context) error { close(called); return nil }).Once()
	_, term := startPortKeys(t, port)
	term.send("/compact\r")
	waitSignal(t, called, "Compact")
}

func TestInteractivePortChangeCWDResolvesRelative(t *testing.T) {
	port := newCmdPort(t, "/w")
	called := make(chan struct{})
	port.EXPECT().ChangeCWD(mock.Anything, "/w/sub").RunAndReturn(func(context.Context, string) error { close(called); return nil }).Once()
	r, term := startPortKeys(t, port)
	waitFor(t, "session cwd", func() bool { return r.i.SessionInfo().CWD == "/w" })
	term.send("/cd sub\r")
	waitSignal(t, called, "ChangeCWD")
}

func TestInteractivePortJail(t *testing.T) {
	port := newCmdPort(t, "/w")
	called := make(chan struct{})
	port.EXPECT().SetJail(mock.Anything, true).RunAndReturn(func(context.Context, bool) error { close(called); return nil }).Once()
	_, term := startPortKeys(t, port)
	term.send("/jail\r")
	waitSignal(t, called, "SetJail(true)")
}

func TestInteractivePortUnjail(t *testing.T) {
	port := newCmdPort(t, "/w")
	called := make(chan struct{})
	port.EXPECT().SetJail(mock.Anything, false).RunAndReturn(func(context.Context, bool) error { close(called); return nil }).Once()
	_, term := startPortKeys(t, port)
	term.send("/unjail\r")
	waitSignal(t, called, "SetJail(false)")
}

func TestInteractivePortReloadExtensions(t *testing.T) {
	port := newCmdPort(t, "/w")
	called := make(chan struct{})
	port.EXPECT().ReloadExtensions(mock.Anything).RunAndReturn(func(context.Context) error { close(called); return nil }).Once()
	_, term := startPortKeys(t, port)
	term.send("/reload-ext\r")
	waitSignal(t, called, "ReloadExtensions")
}

func TestInteractivePortGoal(t *testing.T) {
	port := newCmdPort(t, "/w")
	set := make(chan struct{})
	called := make(chan struct{})
	port.EXPECT().SetGoal(mock.Anything, session.GoalCommand{Op: "set", Objective: "ship it"}).RunAndReturn(func(context.Context, session.GoalCommand) error { close(set); return nil }).Once()
	port.EXPECT().SetGoal(mock.Anything, session.GoalCommand{Op: "pause"}).RunAndReturn(func(context.Context, session.GoalCommand) error { close(called); return nil }).Once()
	_, term := startPortKeys(t, port)
	term.send("/goal ship it\r")
	waitSignal(t, set, "SetGoal(set)")
	term.send("/goal pause\r")
	waitSignal(t, called, "SetGoal(pause)")
}

func TestInteractivePortSkillPin(t *testing.T) {
	port := newCmdPort(t, "/w")
	called := make(chan struct{})
	port.EXPECT().ToggleSkillPin(mock.Anything, "review", true).RunAndReturn(func(context.Context, string, bool) error { close(called); return nil }).Once()
	_, term := startPortKeys(t, port)
	term.send("/skills pin --global review\r")
	waitSignal(t, called, "ToggleSkillPin")
}

func TestInteractivePortSetTitle(t *testing.T) {
	port := newCmdPort(t, "/w")
	called := make(chan struct{})
	port.EXPECT().SetTitle(mock.Anything, "My title").RunAndReturn(func(context.Context, string) error { close(called); return nil }).Once()
	r, _ := startPortKeys(t, port)
	// Waiting on the snapshot (published under i.mu after Run sets runCtx)
	// orders this call after Run's setup.
	waitFor(t, "snapshot", func() bool { return r.i.SessionInfo().ID == "s1" })
	r.i.setManualSessionTitle("My title")
	waitSignal(t, called, "SetTitle")
}

func TestInteractivePortCommandErrorShown(t *testing.T) {
	port := newCmdPort(t, "/w")
	port.EXPECT().SetJail(mock.Anything, true).Return(errors.New("jailrefused")).Once()
	r, term := startPortKeys(t, port)
	term.send("/jail\r")
	r.waitOutput(t, "/jail: jailrefused")
}
