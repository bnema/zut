package session_test

import (
	"errors"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func profileSlots(set map[int]session.QuickModelShortcut) []session.QuickModelShortcut {
	out := make([]session.QuickModelShortcut, 9)
	for n, p := range set {
		out[n-1] = p
	}
	return out
}

func profileStore(t *testing.T, slots []session.QuickModelShortcut) *sessionmocks.MockProfileStore {
	s := sessionmocks.NewMockProfileStore(t)
	s.EXPECT().Profiles().RunAndReturn(func() []session.QuickModelShortcut { return slots }).Maybe()
	return s
}

func swapBuilder(t *testing.T, p, m string) *sessionmocks.MockAgentBuilder {
	b := sessionmocks.NewMockAgentBuilder(t)
	b.EXPECT().BuildAgentFor(p, m).Return(core.NewAgent(providermocks.NewMockClient(t), m, "", nil), p, m, nil).Maybe()
	return b
}

func TestApplyProfileEmptySlot(t *testing.T) {
	h := session.NewLane9Harness(t, session.Deps{Profiles: profileStore(t, profileSlots(nil))}, opsAgent(t, "m"))
	require.ErrorIs(t, h.C.ApplyProfile(h.Ctx, 3), session.ErrProfileEmpty)
	require.Error(t, h.C.ApplyProfile(h.Ctx, 10))
}

func TestApplyProfileActivatesAndPersistsDefault(t *testing.T) {
	want := session.QuickModelShortcut{Provider: "openai", Model: "m2", Reasoning: "low", FastMode: true}
	store := profileStore(t, profileSlots(map[int]session.QuickModelShortcut{2: want}))
	store.EXPECT().Save(2, want, true).Return(nil).Once()
	sess := opsStore(t)
	sess.EXPECT().PersistModel("openai", "m2").Once()
	h := session.NewLane9Harness(t, session.Deps{Profiles: store, AgentBuilder: swapBuilder(t, "openai", "m2"), SessionStore: sess}, opsAgent(t, "m"))
	require.NoError(t, h.C.ApplyProfile(h.Ctx, 2))
	info := h.C.Info()
	require.Equal(t, 2, info.ActiveProfile)
	require.Equal(t, "m2", info.Model)
	require.True(t, info.FastMode)
	require.Len(t, info.Profiles, 9)
}

func TestApplyProfileSaveFailureDetaches(t *testing.T) {
	want := session.QuickModelShortcut{Provider: "openai", Model: "m2"}
	store := profileStore(t, profileSlots(map[int]session.QuickModelShortcut{1: want}))
	store.EXPECT().Save(1, want, true).Return(errors.New("disk")).Once()
	sess := opsStore(t)
	sess.EXPECT().PersistModel("openai", "m2").Once()
	h := session.NewLane9Harness(t, session.Deps{Profiles: store, AgentBuilder: swapBuilder(t, "openai", "m2"), SessionStore: sess}, opsAgent(t, "m"))
	err := h.C.ApplyProfile(h.Ctx, 1)
	require.ErrorContains(t, err, "model switched; profile not saved")
	require.Equal(t, 0, h.C.Info().ActiveProfile)
	require.Equal(t, "m2", h.C.Info().Model)
}

func TestClearActiveProfileKeepsModel(t *testing.T) {
	live := session.QuickModelShortcut{Provider: "openai", Model: "m"}
	store := profileStore(t, profileSlots(map[int]session.QuickModelShortcut{4: live}))
	store.EXPECT().Save(4, session.QuickModelShortcut{}, false).Return(nil).Once()
	h := session.NewLane9Harness(t, session.Deps{Provider: "openai", Model: "m", Profiles: store, ActiveProfile: 4}, opsAgent(t, "m"))
	require.Equal(t, 4, h.C.Info().ActiveProfile)
	require.NoError(t, h.C.ClearProfile(h.Ctx, 4))
	require.Equal(t, 0, h.C.Info().ActiveProfile)
	require.Equal(t, "m", h.C.Info().Model)
}

func TestAutosaveModelReasoningFast(t *testing.T) {
	store := profileStore(t, profileSlots(nil))
	store.EXPECT().Save(1, session.QuickModelShortcut{Provider: "openai", Model: "m2"}, false).Return(nil).Once()
	store.EXPECT().Save(1, session.QuickModelShortcut{Provider: "openai", Model: "m2", Reasoning: "high"}, false).Return(nil).Once()
	store.EXPECT().Save(1, session.QuickModelShortcut{Provider: "openai", Model: "m2", Reasoning: "high", FastMode: true}, false).Return(nil).Once()
	sess := opsStore(t)
	sess.EXPECT().PersistModel("openai", "m2").Once()
	refresh := sessionmocks.NewMockToolRefresher(t)
	refresh.EXPECT().RefreshTools().Return(nil).Once()
	// SettingsStore has no expectations: autosaves never touch plain settings.
	deps := session.Deps{Provider: "openai", Model: "m", Profiles: store, ActiveProfile: 1, AgentBuilder: swapBuilder(t, "openai", "m2"), SessionStore: sess, SettingsStore: sessionmocks.NewMockSettingsStore(t), ToolRefresher: refresh}
	h := session.NewLane9Harness(t, deps, opsAgent(t, "m"))
	require.Equal(t, 1, h.C.Info().ActiveProfile, "unassigned active slot stays active")
	require.NoError(t, h.C.SetModel(h.Ctx, "openai", "m2"))
	require.NoError(t, h.C.SetReasoning(h.Ctx, "high"))
	require.NoError(t, h.C.SetFastMode(h.Ctx, true))
	require.Equal(t, 1, h.C.Info().ActiveProfile)
}

func TestFastModePersistedWithoutActiveProfile(t *testing.T) {
	settings := sessionmocks.NewMockSettingsStore(t)
	settings.EXPECT().SetFastMode(true).Return(nil).Once()
	h := session.NewLane9Harness(t, session.Deps{Provider: "openai", SettingsStore: settings}, opsAgent(t, "m"))
	require.NoError(t, h.C.SetFastMode(h.Ctx, true))
	require.True(t, h.C.Info().FastMode)
}

func TestActiveProfileMismatchDetachesAtStart(t *testing.T) {
	store := profileStore(t, profileSlots(map[int]session.QuickModelShortcut{2: {Provider: "openai", Model: "other"}}))
	h := session.NewLane9Harness(t, session.Deps{Provider: "openai", Model: "m", Profiles: store, ActiveProfile: 2}, opsAgent(t, "m"))
	require.Equal(t, 0, h.C.Info().ActiveProfile)
}

func TestRescueDetachesActiveProfile(t *testing.T) {
	store := profileStore(t, profileSlots(nil))
	builder := sessionmocks.NewMockAgentBuilder(t)
	builder.EXPECT().BuildAgentForRescue("openai", "gpt").Return(core.NewAgent(providermocks.NewMockClient(t), "gpt", "", nil), "openai", "gpt", nil).Once()
	sess := opsStore(t)
	sess.EXPECT().PersistModel("openai", "gpt").Once()
	h := session.NewLane9Harness(t, session.Deps{Profiles: store, ActiveProfile: 1, AgentBuilder: builder, SessionStore: sess}, opsAgent(t, "m"))
	require.Equal(t, 1, h.C.Info().ActiveProfile)
	h.RescueTurn(t, "openai/gpt", session.Input{Text: "x"})
	<-h.Retried
	require.Equal(t, 0, h.C.Info().ActiveProfile)
	store.AssertNotCalled(t, "Save", mock.Anything, mock.Anything, mock.Anything)
}

func TestInfoDeepCopiesProfiles(t *testing.T) {
	store := profileStore(t, profileSlots(map[int]session.QuickModelShortcut{1: {Provider: "openai", Model: "m"}}))
	h := session.NewLane9Harness(t, session.Deps{Profiles: store}, opsAgent(t, "m"))
	info := h.C.Info()
	info.Profiles[0].Model = "mutated"
	require.Equal(t, "m", h.C.Info().Profiles[0].Model)
}

func TestAssignProfileDuringTurnSavesWithoutActivating(t *testing.T) {
	store := profileStore(t, profileSlots(nil))
	p := session.QuickModelShortcut{Provider: "openai", Model: "unlisted-x", Reasoning: "HI"}
	store.EXPECT().Save(3, session.QuickModelShortcut{Provider: "openai", Model: "unlisted-x", Reasoning: "high"}, false).Return(nil).Once()
	h := session.NewLane9Harness(t, session.Deps{Profiles: store}, opsAgent(t, "m"))
	h.SetBusy(t)
	require.NoError(t, h.C.AssignProfile(h.Ctx, 3, p, false))
	require.Equal(t, 0, h.C.Info().ActiveProfile)
	require.ErrorIs(t, h.C.AssignProfile(h.Ctx, 3, p, true), session.ErrBusy)
	require.ErrorIs(t, h.C.ApplyProfile(h.Ctx, 3), session.ErrBusy)
}

func TestActiveProfileClampedReasoningStaysAttached(t *testing.T) {
	// gpt-5-mini tops out at high: a "max" slot runs at high and still matches.
	store := profileStore(t, profileSlots(map[int]session.QuickModelShortcut{3: {Provider: "openai", Model: "gpt-5-mini", Reasoning: "max"}}))
	h := session.NewLane9Harness(t, session.Deps{Provider: "openai", Model: "gpt-5-mini", Reasoning: "high", Profiles: store, ActiveProfile: 3}, opsAgent(t, "gpt-5-mini"))
	require.Equal(t, 3, h.C.Info().ActiveProfile)
}
