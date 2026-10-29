package session_test

import (
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/require"
)

func opsAgent(t *testing.T, model string) *core.Agent {
	ag := core.NewAgent(providermocks.NewMockClient(t), model, "", nil)
	ag.AppendUserContext("keep me", nil)
	return ag
}
func opsStore(t *testing.T) *sessionmocks.MockSessionStore {
	s := sessionmocks.NewMockSessionStore(t)
	s.EXPECT().CurrentSessionPath().Return("").Maybe()
	return s
}

func TestSetModelSwapsAgentKeepingTranscript(t *testing.T) {
	old := opsAgent(t, "old")
	next := core.NewAgent(providermocks.NewMockClient(t), "new", "", nil)
	builder := sessionmocks.NewMockAgentBuilder(t)
	builder.EXPECT().BuildAgentFor("openai", "new").Return(next, "openai", "new", nil).Once()
	store := opsStore(t)
	store.EXPECT().PersistModel("openai", "new").Once()
	h := session.NewLane9Harness(t, session.Deps{AgentBuilder: builder, SessionStore: store}, old)
	require.NoError(t, h.C.SetModel(h.Ctx, "openai", "new"))
	require.Same(t, next, h.Agent(t))
	require.Equal(t, old.Messages(), next.Messages())
	require.Equal(t, "new", h.C.Info().Model)
}

func TestSetReasoningPersistsAndUpdatesAgent(t *testing.T) {
	settings := sessionmocks.NewMockSettingsStore(t)
	settings.EXPECT().SetReasoning("high").Return(nil).Once()
	ag := opsAgent(t, "m")
	h := session.NewLane9Harness(t, session.Deps{SettingsStore: settings}, ag)
	require.NoError(t, h.C.SetReasoning(h.Ctx, "high"))
	require.Equal(t, "high", h.Agent(t).Reasoning)
	require.Equal(t, "high", h.C.Info().Reasoning)
}

func TestSetFastModeValidatesProvider(t *testing.T) {
	refresh := sessionmocks.NewMockToolRefresher(t)
	refresh.EXPECT().RefreshTools().Return(nil).Once()
	ag := opsAgent(t, "m")
	h := session.NewLane9Harness(t, session.Deps{Provider: "openai", ToolRefresher: refresh}, ag)
	require.NoError(t, h.C.SetFastMode(h.Ctx, true))
	require.True(t, h.Agent(t).FastModeEnabled())
	require.True(t, h.C.Info().FastMode)

	h2 := session.NewLane9Harness(t, session.Deps{Provider: "anthropic"}, opsAgent(t, "m"))
	require.Error(t, h2.C.SetFastMode(h2.Ctx, true))
	require.False(t, h2.C.Info().FastMode)
}

func TestApplyProfileSwapsModelAndReasoning(t *testing.T) {
	next := core.NewAgent(providermocks.NewMockClient(t), "p", "", nil)
	builder := sessionmocks.NewMockAgentBuilder(t)
	builder.EXPECT().BuildAgentFor("openai", "p").Return(next, "openai", "p", nil).Once()
	store := opsStore(t)
	store.EXPECT().PersistModel("openai", "p").Once()
	deps := session.Deps{AgentBuilder: builder, SessionStore: store, QuickModelShortcuts: []session.QuickModelShortcut{{Provider: "openai", Model: "p", Reasoning: "low", FastMode: true}}}
	h := session.NewLane9Harness(t, deps, opsAgent(t, "old"))
	require.Error(t, h.C.ApplyProfile(h.Ctx, 2))
	require.NoError(t, h.C.ApplyProfile(h.Ctx, 1))
	ag := h.Agent(t)
	require.Same(t, next, ag)
	require.Equal(t, "low", ag.Reasoning)
	require.True(t, ag.FastModeEnabled())
	info := h.C.Info()
	require.Equal(t, "p", info.Model)
	require.Equal(t, "low", info.Reasoning)
}

func TestApplySettingsPersistsAndRefreshes(t *testing.T) {
	settings := sessionmocks.NewMockSettingsStore(t)
	settings.EXPECT().SetAutoSubagents(true).Return(nil).Once()
	refresh := sessionmocks.NewMockToolRefresher(t)
	refresh.EXPECT().SetWebSearchAvailable(false).Once()
	refresh.EXPECT().RefreshTools().Return(nil).Once()
	auto, threshold := false, 85
	deps := session.Deps{SettingsStore: settings, ToolRefresher: refresh, AutoSubagentsEnabled: &auto, AutoCompactThreshold: &threshold}
	h := session.NewLane9Harness(t, deps, opsAgent(t, "m"))
	bad := 101
	require.Error(t, h.C.ApplySettings(h.Ctx, session.SettingsPatch{AutoCompactThreshold: &bad}))
	on, off, ninety := true, false, 90
	require.NoError(t, h.C.ApplySettings(h.Ctx, session.SettingsPatch{AutoSubagentsEnabled: &on, WebSearchEnabled: &off, AutoCompactThreshold: &ninety}))
	// Assembly-owned pointers are updated in place.
	require.True(t, auto)
	require.Equal(t, 90, threshold)
}

func TestChangeCWDRejectsWithoutStore(t *testing.T) {
	h := session.NewLane9Harness(t, session.Deps{CWD: "/base"}, opsAgent(t, "m"))
	require.Error(t, h.C.ChangeCWD(h.Ctx, "child"))
	require.Equal(t, "/base", h.C.Info().CWD)
}

func TestSetJailPersists(t *testing.T) {
	settings := sessionmocks.NewMockSettingsStore(t)
	settings.EXPECT().SetJailByDefault(true).Return(nil).Once()
	jail := false
	h := session.NewLane9Harness(t, session.Deps{SettingsStore: settings, JailByDefault: &jail}, opsAgent(t, "m"))
	require.NoError(t, h.C.SetJail(h.Ctx, true))
	require.True(t, jail)
}

func TestSetTitleNormalizesAndPublishes(t *testing.T) {
	store := opsStore(t)
	store.EXPECT().PersistTitle("Hello").Return(nil).Once()
	h := session.NewLane9Harness(t, session.Deps{SessionStore: store}, opsAgent(t, "m"))
	events, err := h.C.Subscribe(h.Ctx, 0)
	require.NoError(t, err)
	require.NoError(t, h.C.SetTitle(h.Ctx, "  Hello  "))
	for {
		select {
		case ev := <-events:
			if ev.Kind == session.EvTitle {
				require.Equal(t, "Hello", ev.Data.(session.TitleEvent).Title)
				return
			}
		case <-h.Ctx.Done():
			t.Fatal(h.Ctx.Err())
		}
	}
}

func TestToggleSkillPinUsesSessionCWD(t *testing.T) {
	skills := sessionmocks.NewMockSkillStore(t)
	skills.EXPECT().ToggleSkillPin("/proj", "go", true).Return(nil).Once()
	h := session.NewLane9Harness(t, session.Deps{CWD: "/proj", SkillStore: skills}, opsAgent(t, "m"))
	require.NoError(t, h.C.ToggleSkillPin(h.Ctx, "go", true))
	h2 := session.NewLane9Harness(t, session.Deps{}, opsAgent(t, "m"))
	require.Error(t, h2.C.ToggleSkillPin(h2.Ctx, "go", false))
}

func TestReloadCredentialsRebuildsCurrentModel(t *testing.T) {
	old := opsAgent(t, "m")
	next := core.NewAgent(providermocks.NewMockClient(t), "m", "", nil)
	builder := sessionmocks.NewMockAgentBuilder(t)
	builder.EXPECT().BuildAgentFor("openai", "m").Return(next, "openai", "m", nil).Once()
	store := opsStore(t)
	store.EXPECT().PersistModel("openai", "m").Once()
	h := session.NewLane9Harness(t, session.Deps{Provider: "openai", Model: "m", AgentBuilder: builder, SessionStore: store}, old)
	require.NoError(t, h.C.ReloadCredentials(h.Ctx))
	require.Same(t, next, h.Agent(t))
	require.Equal(t, old.Messages(), next.Messages())
}

func TestRescueFallbackAdoptsModelAndRetries(t *testing.T) {
	old := opsAgent(t, "old")
	next := core.NewAgent(providermocks.NewMockClient(t), "gpt", "", nil)
	builder := sessionmocks.NewMockAgentBuilder(t)
	builder.EXPECT().BuildAgentForRescue("openai", "gpt").Return(next, "openai", "gpt", nil).Once()
	store := opsStore(t)
	store.EXPECT().PersistModel("openai", "gpt").Once()
	h := session.NewLane9Harness(t, session.Deps{AgentBuilder: builder, SessionStore: store}, old)
	retry := session.Input{Text: "again", Origin: session.InputOriginUser}
	h.RescueTurn(t, "openai/gpt", retry)
	select {
	case got := <-h.Retried:
		require.Equal(t, retry, got)
	case <-time.After(5 * time.Second):
		t.Fatal("rescue did not retry (awaitPending on actor loop?)")
	}
	require.Same(t, next, h.Agent(t))
	require.Equal(t, old.Messages(), next.Messages())
	require.Equal(t, "gpt", h.C.Info().Model)
}
