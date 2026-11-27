package agent

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/config"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/stretchr/testify/require"
)

func TestModelProfileStoreNonDefaultSaveKeepsActive(t *testing.T) {
	sessionAssemblyEnv(t)
	require.NoError(t, config.Save(config.Config{ActiveModelProfile: 2, QuickModelShortcuts: []config.QuickModelShortcut{{}, {Provider: "openai", Model: "gpt-5"}}}))
	s := &modelProfileStore{}
	require.NoError(t, s.Save(4, session.QuickModelShortcut{Provider: "openai", Model: "gpt-5-mini"}, false))
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, 2, cfg.ActiveModelProfile)
	require.Equal(t, "gpt-5-mini", cfg.QuickModelShortcuts[3].Model)
	require.Equal(t, "gpt-5-mini", s.Profiles()[3].Model)
}

func TestModelProfileStoreMakeDefaultWritesDefaults(t *testing.T) {
	sessionAssemblyEnv(t)
	s := &modelProfileStore{}
	require.NoError(t, s.Save(3, session.QuickModelShortcut{Provider: "openai", Model: "gpt-5", Reasoning: "high", FastMode: true}, true))
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, 3, cfg.ActiveModelProfile)
	require.Equal(t, "openai", cfg.Provider)
	require.Equal(t, "gpt-5", cfg.Model)
	require.Equal(t, "high", cfg.Reasoning)
	require.NotNil(t, cfg.FastMode)
	require.True(t, *cfg.FastMode)
	require.Equal(t, 3, s.ActiveDefault())
}

func TestModelProfileStoreClearConfigActiveZeroes(t *testing.T) {
	sessionAssemblyEnv(t)
	s := &modelProfileStore{}
	require.NoError(t, s.Save(2, session.QuickModelShortcut{Provider: "openai", Model: "gpt-5"}, true))
	require.NoError(t, s.Save(2, session.QuickModelShortcut{}, false))
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, 0, cfg.ActiveModelProfile)
	require.Empty(t, cfg.QuickModelShortcuts)
	require.Equal(t, 0, s.ActiveDefault(), "no slot is active without a configured default")
}

func TestModelProfileStoreConcurrentSaves(t *testing.T) {
	sessionAssemblyEnv(t)
	s := &modelProfileStore{}
	var wg sync.WaitGroup
	for slot := 1; slot <= 9; slot++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, s.Save(slot, session.QuickModelShortcut{Provider: "openai", Model: "gpt-5"}, false))
		}()
	}
	wg.Wait()
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Len(t, cfg.QuickModelShortcuts, 9)
	for _, p := range cfg.QuickModelShortcuts {
		require.Equal(t, "gpt-5", p.Model)
	}
}

func TestDaemonProfileAssignReachesOtherSessions(t *testing.T) {
	home := sessionAssemblyEnv(t)
	server := sessionAssemblyProvider(t)
	td := startFactoryDaemon(t, home, daemonTestSocket(t))
	c := dialTestDaemon(t, td.socket)
	ctx := factoryTestCtx(t)
	env := []string{"PATH=" + os.Getenv("PATH")}
	a, err := c.Create(ctx, session.CreateSpec{CWD: t.TempDir(), Env: env, Args: sessionAssemblyArgs(server)})
	require.NoError(t, err)
	b, err := c.Create(ctx, session.CreateSpec{CWD: t.TempDir(), Env: env, Args: sessionAssemblyArgs(server)})
	require.NoError(t, err)
	require.Len(t, b.Profiles, 9)

	bh := td.server.factory.lookup(session.ID(b.ID))
	events, err := bh.Subscribe(ctx, 0)
	require.NoError(t, err)
	prof := session.QuickModelShortcut{Provider: "openai", Model: "gpt-5-mini"}
	require.NoError(t, c.Port(session.ID(a.ID), true).AssignProfile(ctx, 5, prof, false))

waitInfo:
	for {
		select {
		case ev := <-events:
			if info, ok := ev.Data.(session.InfoEvent); ok && info.Info.Profiles[4].Model == "gpt-5-mini" {
				break waitInfo
			}
		case <-ctx.Done():
			t.Fatal("profile info event not received: ", ctx.Err())
		}
	}
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, 0, cfg.ActiveModelProfile)
}

func TestNewSessionsStartWithoutDefaultSlot(t *testing.T) {
	sessionAssemblyEnv(t)
	s := &modelProfileStore{}
	require.NoError(t, s.Reload())
	require.Equal(t, 0, s.ActiveDefault())
	require.NoError(t, config.Save(config.Config{ActiveModelProfile: 12}))
	require.NoError(t, s.Reload())
	require.Equal(t, 0, s.ActiveDefault())
	require.NoError(t, config.Save(config.Config{ActiveModelProfile: 4}))
	require.NoError(t, s.Reload())
	require.Equal(t, 4, s.ActiveDefault())
}

// Two sessions saving profiles while settings writes run concurrently must
// keep every update.
func TestConcurrentProfileAndSettingsWritesKeepAllUpdates(t *testing.T) {
	sessionAssemblyEnv(t)
	s := &modelProfileStore{}
	ctx, cancel := context.WithCancel(context.Background())
	a := session.NewController(ctx, session.Deps{Profiles: s})
	b := session.NewController(ctx, session.Deps{Profiles: s})
	t.Cleanup(func() { cancel(); <-a.Done(); <-b.Done() })
	var wg sync.WaitGroup
	for slot := 1; slot <= 9; slot++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := a
			if slot%2 == 0 {
				c = b
			}
			require.NoError(t, c.AssignProfile(ctx, slot, session.QuickModelShortcut{Provider: "openai", Model: "gpt-5"}, false))
		}()
	}
	store := configSettingsStore{}
	wg.Add(4)
	go func() { defer wg.Done(); require.NoError(t, store.SetFastMode(true)) }()
	go func() { defer wg.Done(); require.NoError(t, store.SetReasoning("high")) }()
	go func() { defer wg.Done(); require.NoError(t, store.SetAutoSubagents(true)) }()
	go func() { defer wg.Done(); require.NoError(t, store.SetJailByDefault(true)) }()
	wg.Wait()
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Len(t, cfg.QuickModelShortcuts, 9)
	for i, p := range cfg.QuickModelShortcuts {
		require.Equal(t, "gpt-5", p.Model)
		require.Equal(t, p.Model, s.Profiles()[i].Model)
	}
	require.True(t, *cfg.FastMode)
	require.Equal(t, "high", cfg.Reasoning)
	require.True(t, *cfg.AutoSubagentsEnabled)
	require.True(t, *cfg.JailByDefault)
}

func TestProfileFanoutDrainsOnShutdown(t *testing.T) {
	f := newSessionFactory("test", t.TempDir(), nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	live := session.NewController(ctx, session.Deps{})
	closed := session.NewController(ctx, session.Deps{})
	require.NoError(t, closed.Close(context.Background(), "test"))
	<-closed.Done()
	require.ErrorIs(t, closed.ProfilesChanged(context.Background()), session.ErrClosed)
	events, err := live.Subscribe(ctx, 0)
	require.NoError(t, err)
	f.mu.Lock()
	f.live["live"] = &factoryHandle{Controller: live}
	f.live["closed"] = &factoryHandle{Controller: closed}
	f.mu.Unlock()

	f.profilesChanged()
	select {
	case ev := <-events:
		require.Equal(t, session.EvInfo, ev.Kind)
	case <-time.After(5 * time.Second):
		t.Fatal("live session not notified")
	}
	// The closed controller answers ErrClosed; wait drains both goroutines.
	waited := make(chan struct{})
	go func() { f.wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not drain profile notifications")
	}
	// After shutdown no new notification starts.
	f.profilesChanged()
	f.fanout.Wait()
	cancel()
	<-live.Done()
}
