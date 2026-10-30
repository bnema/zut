package agent

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/stretchr/testify/require"
)

func TestModelProfileStoreNonDefaultSaveKeepsActive(t *testing.T) {
	sessionAssemblyEnv(t)
	require.NoError(t, SaveConfig(Config{ActiveModelProfile: 2, QuickModelShortcuts: []QuickModelShortcut{{}, {Provider: "openai", Model: "gpt-5"}}}))
	s := &modelProfileStore{}
	require.NoError(t, s.Save(4, session.QuickModelShortcut{Provider: "openai", Model: "gpt-5-mini"}, false))
	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, 2, cfg.ActiveModelProfile)
	require.Equal(t, "gpt-5-mini", cfg.QuickModelShortcuts[3].Model)
	require.Equal(t, "gpt-5-mini", s.Profiles()[3].Model)
}

func TestModelProfileStoreMakeDefaultWritesDefaults(t *testing.T) {
	sessionAssemblyEnv(t)
	s := &modelProfileStore{}
	require.NoError(t, s.Save(3, session.QuickModelShortcut{Provider: "openai", Model: "gpt-5", Reasoning: "high", FastMode: true}, true))
	cfg, err := LoadConfig()
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
	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, 0, cfg.ActiveModelProfile)
	require.Empty(t, cfg.QuickModelShortcuts)
	require.Equal(t, 1, s.ActiveDefault(), "invalid active falls back to slot 1")
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
	cfg, err := LoadConfig()
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

	prof := session.QuickModelShortcut{Provider: "openai", Model: "gpt-5-mini"}
	require.NoError(t, c.Port(session.ID(a.ID), true).AssignProfile(ctx, 5, prof, false))
	bh := td.server.factory.lookup(session.ID(b.ID))
	require.Eventually(t, func() bool { return bh.Info().Profiles[4].Model == "gpt-5-mini" }, 10*time.Second, 10*time.Millisecond)
	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, 0, cfg.ActiveModelProfile)
}
