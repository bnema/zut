package modes

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider/auth"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func newPortLoginInteractive(t *testing.T, port *sessionmocks.MockPort) (*Interactive, *auth.Store) {
	t.Helper()
	store := auth.NewStore(filepath.Join(t.TempDir(), "auth.json"))
	i := NewInteractive(InteractiveConfig{
		Terminal:    &alertTestTerminal{},
		Port:        port,
		AuthManager: auth.NewManager(store),
		BuildAgent: func() (*core.Agent, string, string, error) {
			t.Error("BuildAgent must not run in port mode")
			return nil, "", "", errors.New("unexpected")
		},
	})
	require.True(t, i.portMode.Load())
	return i, store
}

func expectReload(port *sessionmocks.MockPort, err error) chan struct{} {
	called := make(chan struct{})
	port.EXPECT().ReloadCredentials(mock.Anything).RunAndReturn(func(context.Context) error {
		close(called)
		return err
	}).Once()
	return called
}

func TestInteractivePortLoginSuccessReloadsCredentials(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	called := expectReload(port, nil)
	i, _ := newPortLoginInteractive(t, port)
	i.handleAuthEvent(auth.Event{Kind: "success", Provider: "anthropic", Method: "api key"})
	waitSignal(t, called, "ReloadCredentials")
	i.mu.Lock()
	defer i.mu.Unlock()
	require.Nil(t, i.agent)
	require.Empty(t, i.statusErr)
}

func TestInteractivePortLlamaLoginReloadsCredentials(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	called := expectReload(port, nil)
	i, _ := newPortLoginInteractive(t, port)
	i.saveLlamaCPPLogin("http://127.0.0.1:1", "")
	waitSignal(t, called, "ReloadCredentials")
}

func TestInteractivePortLogoutReloadsCredentials(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	called := expectReload(port, nil)
	i, store := newPortLoginInteractive(t, port)
	require.NoError(t, store.SetAPIKey("anthropic", "k"))
	i.doLogout("anthropic")
	waitSignal(t, called, "ReloadCredentials")
	i.mu.Lock()
	defer i.mu.Unlock()
	require.Empty(t, i.statusErr)
	require.Contains(t, i.statusOK, "logged out of anthropic")
}

func TestInteractivePortReloadCredentialsErrorShowsNote(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	expectReload(port, errors.New("daemon refused"))
	i, _ := newPortLoginInteractive(t, port)
	i.handleAuthEvent(auth.Event{Kind: "success", Provider: "anthropic", Method: "api key"})
	require.Eventually(t, func() bool {
		i.mu.Lock()
		defer i.mu.Unlock()
		return strings.Contains(i.statusErr, "reload daemon credentials: daemon refused")
	}, 5*time.Second, 10*time.Millisecond)
}
