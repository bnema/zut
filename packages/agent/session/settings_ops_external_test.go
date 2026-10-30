package session_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
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
	s.EXPECT().InitialPlanSnapshots().Return(nil).Maybe()
	s.EXPECT().CurrentGoal().Return(nil).Maybe()
	s.EXPECT().CurrentPlan().Return(nil).Maybe()
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

func TestRescueDetachedFails(t *testing.T) {
	h := session.NewLane9Harness(t, session.Deps{AgentBuilder: sessionmocks.NewMockAgentBuilder(t)}, opsAgent(t, "m"))
	h.SetConfirm(t, nil, session.ErrNotAttached)
	_, err := h.Rescue(errors.New("overflow"))
	require.ErrorIs(t, err, session.ErrNotAttached)
}

func TestRescueTurnDetachedPublishesNote(t *testing.T) {
	h := session.NewLane9Harness(t, session.Deps{AgentBuilder: sessionmocks.NewMockAgentBuilder(t)}, opsAgent(t, "m"))
	h.SetConfirm(t, nil, session.ErrNotAttached)
	events, err := h.C.Subscribe(h.Ctx, 0)
	require.NoError(t, err)
	h.StartRescueTurn(t, errors.New("overflow"), session.Input{Text: "x"})
	for {
		select {
		case ev := <-events:
			if ev.Kind == session.EvNote {
				require.Contains(t, ev.Data.(session.NoteEvent).Note.Text, session.ErrNotAttached.Error())
				return
			}
		case <-h.Ctx.Done():
			t.Fatal(h.Ctx.Err())
		}
	}
}

func waitInfo(t *testing.T, h *session.Lane9Harness, events <-chan session.Event) session.Info {
	t.Helper()
	for {
		select {
		case ev := <-events:
			if ev.Kind == session.EvInfo {
				return ev.Data.(session.InfoEvent).Info
			}
		case <-h.Ctx.Done():
			t.Fatal(h.Ctx.Err())
		}
	}
}

func TestChangeCWDRefreshFailureStillApplied(t *testing.T) {
	store := opsStore(t)
	store.EXPECT().ChangeCWD("/base/child").Return(nil).Once()
	refresh := sessionmocks.NewMockToolRefresher(t)
	boom := errors.New("boom")
	refresh.EXPECT().RefreshTools().Return(boom).Once()
	h := session.NewLane9Harness(t, session.Deps{CWD: "/base", SessionStore: store, ToolRefresher: refresh}, opsAgent(t, "m"))
	events, err := h.C.Subscribe(h.Ctx, 0)
	require.NoError(t, err)
	err = h.C.ChangeCWD(h.Ctx, "child")
	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "applied")
	require.ErrorContains(t, err, "refresh tools")
	require.Equal(t, "/base/child", waitInfo(t, h, events).CWD)
}

func TestSetJailRefreshFailureStillApplied(t *testing.T) {
	settings := sessionmocks.NewMockSettingsStore(t)
	settings.EXPECT().SetJailByDefault(true).Return(nil).Once()
	refresh := sessionmocks.NewMockToolRefresher(t)
	boom := errors.New("boom")
	refresh.EXPECT().RefreshTools().Return(boom).Once()
	jail := false
	h := session.NewLane9Harness(t, session.Deps{SettingsStore: settings, ToolRefresher: refresh, JailByDefault: &jail}, opsAgent(t, "m"))
	events, err := h.C.Subscribe(h.Ctx, 0)
	require.NoError(t, err)
	err = h.C.SetJail(h.Ctx, true)
	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "applied")
	waitInfo(t, h, events)
	require.True(t, jail)
}

func TestApplySettingsPartialStoreFailure(t *testing.T) {
	settings := sessionmocks.NewMockSettingsStore(t)
	settings.EXPECT().SetAutoSubagents(true).Return(nil).Once()
	boom := errors.New("disk full")
	settings.EXPECT().SetJailByDefault(true).Return(boom).Once()
	auto, jail := false, false
	h := session.NewLane9Harness(t, session.Deps{SettingsStore: settings, AutoSubagentsEnabled: &auto, JailByDefault: &jail}, opsAgent(t, "m"))
	on := true
	require.ErrorIs(t, h.C.ApplySettings(h.Ctx, session.SettingsPatch{AutoSubagentsEnabled: &on, JailByDefault: &on}), boom)
	// The persisted field is live; the failed one is untouched.
	require.True(t, auto)
	require.False(t, jail)
}

func TestApplySettingsRejectsUnsupportedThreshold(t *testing.T) {
	threshold := 85
	h := session.NewLane9Harness(t, session.Deps{AutoCompactThreshold: &threshold}, opsAgent(t, "m"))
	for _, v := range []int{-1, 1, 50, 75, 100} {
		err := h.C.ApplySettings(h.Ctx, session.SettingsPatch{AutoCompactThreshold: &v})
		require.ErrorContains(t, err, "must be one of 0 (off), 70, 80, 85, 90, 95")
	}
	require.Equal(t, 85, threshold)
	for _, v := range []int{0, 70, 80, 85, 90, 95} {
		require.NoError(t, h.C.ApplySettings(h.Ctx, session.SettingsPatch{AutoCompactThreshold: &v}))
		require.Equal(t, v, threshold)
	}
}

func TestAppendShellContextAppendsTaggedUserContext(t *testing.T) {
	store := opsStore(t)
	store.EXPECT().FlushSession().Once()
	ag := opsAgent(t, "m")
	h := session.NewLane9Harness(t, session.Deps{SessionStore: store}, ag)
	events, err := h.C.Subscribe(h.Ctx, 0)
	require.NoError(t, err)
	require.NoError(t, h.C.AppendShellContext(h.Ctx, "$ ls\n\nfile"))
	msgs := ag.Messages()
	last := msgs[len(msgs)-1]
	require.Equal(t, provider.RoleUser, last.Role)
	require.Equal(t, "true", last.Meta[session.ShellEscapeMetaKey])
	require.Equal(t, []provider.Content{provider.TextBlock{Text: "$ ls\n\nfile"}}, last.Content)
	for {
		select {
		case ev := <-events:
			if um, ok := ev.Agent.(core.EvUserMessage); ok && ev.Kind == session.EvAgent {
				require.Equal(t, "true", um.Message.Meta[session.ShellEscapeMetaKey])
				return
			}
		case <-h.Ctx.Done():
			t.Fatal(h.Ctx.Err())
		}
	}
}

func TestAppendShellContextRejectsOversizedText(t *testing.T) {
	h := session.NewLane9Harness(t, session.Deps{}, opsAgent(t, "m"))
	require.ErrorContains(t, h.C.AppendShellContext(h.Ctx, strings.Repeat("x", session.MaxShellContextBytes+1)), "limit")
}
