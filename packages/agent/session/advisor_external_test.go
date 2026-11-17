package session_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestSetAdvisorPersistsRefreshesAndReportsStatus(t *testing.T) {
	adv := sessionmocks.NewMockAdvisorStore(t)
	adv.EXPECT().AdvisorStatus().Return(session.AdvisorStatus{Mode: "auto"}, nil).Once()
	spec := "anthropic/claude-opus-4-7:high"
	enabled := session.AdvisorStatus{Enabled: true, Model: spec, Mode: "auto"}
	adv.EXPECT().SetAdvisor(session.AdvisorChange{Model: &spec}).Return(enabled, nil).Once()
	refresh := sessionmocks.NewMockToolRefresher(t)
	refresh.EXPECT().RefreshTools().Return(nil).Once()
	h := session.NewLane9Harness(t, session.Deps{Advisor: adv, ToolRefresher: refresh}, opsAgent(t, "m"))
	require.Equal(t, &session.AdvisorStatus{Mode: "auto"}, h.C.Info().Advisor)

	got, err := h.C.SetAdvisor(h.Ctx, session.AdvisorChange{Model: &spec})
	require.NoError(t, err)
	require.Equal(t, enabled, got)
	require.Equal(t, &enabled, h.C.Info().Advisor)
}

func TestSetAdvisorValidationFailureKeepsStatusAndTools(t *testing.T) {
	adv := sessionmocks.NewMockAdvisorStore(t)
	initial := session.AdvisorStatus{Mode: "auto"}
	adv.EXPECT().AdvisorStatus().Return(initial, nil).Once()
	on := true
	adv.EXPECT().SetAdvisor(session.AdvisorChange{Enabled: &on}).Return(session.AdvisorStatus{}, errors.New("no advisor model configured")).Once()
	refresh := sessionmocks.NewMockToolRefresher(t) // RefreshTools must not run
	h := session.NewLane9Harness(t, session.Deps{Advisor: adv, ToolRefresher: refresh}, opsAgent(t, "m"))
	_, err := h.C.SetAdvisor(h.Ctx, session.AdvisorChange{Enabled: &on})
	require.ErrorContains(t, err, "no advisor model configured")
	require.Equal(t, &initial, h.C.Info().Advisor)
}

func TestSetAdvisorRefreshFailureReportsAppliedStatus(t *testing.T) {
	adv := sessionmocks.NewMockAdvisorStore(t)
	adv.EXPECT().AdvisorStatus().Return(session.AdvisorStatus{Mode: "auto"}, nil).Once()
	off := false
	disabled := session.AdvisorStatus{Model: "a/b", Mode: "auto"}
	adv.EXPECT().SetAdvisor(session.AdvisorChange{Enabled: &off}).Return(disabled, nil).Once()
	refresh := sessionmocks.NewMockToolRefresher(t)
	refresh.EXPECT().RefreshTools().Return(errors.New("boom")).Once()
	h := session.NewLane9Harness(t, session.Deps{Advisor: adv, ToolRefresher: refresh}, opsAgent(t, "m"))
	_, err := h.C.SetAdvisor(h.Ctx, session.AdvisorChange{Enabled: &off})
	require.ErrorContains(t, err, "setting applied, but refresh tools")
	require.Equal(t, &disabled, h.C.Info().Advisor)
}

func TestSetAdvisorWithoutBackend(t *testing.T) {
	h := session.NewLane9Harness(t, session.Deps{}, opsAgent(t, "m"))
	require.Nil(t, h.C.Info().Advisor)
	_, err := h.C.SetAdvisor(h.Ctx, session.AdvisorChange{})
	require.ErrorContains(t, err, "advisor settings unavailable")
}

func TestAdvisorEffectiveFollowsLiveAgent(t *testing.T) {
	adv := sessionmocks.NewMockAdvisorStore(t)
	adv.EXPECT().AdvisorStatus().Return(session.AdvisorStatus{Enabled: true, Model: "anthropic/claude-opus-4-7", Mode: "auto"}, nil).Once()
	native := opsAgent(t, "claude-sonnet-4-6")
	native.SetServerAdvisor(&provider.NativeAdvisor{Model: "claude-opus-4-7"})
	client := opsAgent(t, "gpt-5")
	client.SetTools(core.Registry{tools.AdvisorToolName: &tools.AdvisorTool{}})
	builder := sessionmocks.NewMockAgentBuilder(t)
	built := make(chan struct{})
	// SetModel's replaceAgent builds the initial agent to keep its
	// transcript; Info alone must never trigger that lazy build.
	builder.EXPECT().BuildAgent().RunAndReturn(func() (*core.Agent, string, string, error) {
		close(built)
		return opsAgent(t, "m"), "p", "m", nil
	}).Once()
	builder.EXPECT().BuildAgentFor("anthropic", "claude-sonnet-4-6").Return(native, "anthropic", "claude-sonnet-4-6", nil).Once()
	builder.EXPECT().BuildAgentFor("openai", "gpt-5").Return(client, "openai", "gpt-5", nil).Once()
	store := opsStore(t)
	store.EXPECT().PersistModel(mock.Anything, mock.Anything).Times(2)
	ctx, cancel := context.WithCancel(context.Background())
	c := session.NewController(ctx, session.Deps{Advisor: adv, AgentBuilder: builder, SessionStore: store})
	t.Cleanup(func() { cancel(); <-c.Done() })
	require.Empty(t, c.Info().Advisor.Effective)
	select {
	case <-built:
		t.Fatal("Info built the agent")
	default:
	}

	require.NoError(t, c.SetModel(ctx, "anthropic", "claude-sonnet-4-6"))
	require.Equal(t, "native", c.Info().Advisor.Effective)
	require.NoError(t, c.SetModel(ctx, "openai", "gpt-5"))
	require.Equal(t, "client", c.Info().Advisor.Effective)
}

// Config is shared across sessions: a later refresh picks up another
// session's /advisor change in Info.
func TestAdvisorStatusResyncsAfterToolRefresh(t *testing.T) {
	adv := sessionmocks.NewMockAdvisorStore(t)
	adv.EXPECT().AdvisorStatus().Return(session.AdvisorStatus{Mode: "auto"}, nil).Once()
	changed := session.AdvisorStatus{Enabled: true, Model: "anthropic/claude-opus-4-7:high", Mode: "auto"}
	adv.EXPECT().AdvisorStatus().Return(changed, nil)
	refreshed := make(chan struct{}, 2)
	refresh := sessionmocks.NewMockToolRefresher(t)
	refresh.EXPECT().RefreshTools().RunAndReturn(func() error { refreshed <- struct{}{}; return nil }).Once()
	ctx, cancel := context.WithCancel(context.Background())
	c := session.NewController(ctx, session.Deps{Advisor: adv, ToolRefresher: refresh})
	t.Cleanup(func() { cancel(); <-c.Done() })
	require.False(t, c.Info().Advisor.Enabled)

	c.RequestToolRefresh()
	<-refreshed
	// Actor barrier: the refresh flush finished before this settings call.
	require.NoError(t, c.SetTitle(ctx, "barrier"))
	require.True(t, c.Info().Advisor.Enabled)
	require.Equal(t, changed.Model, c.Info().Advisor.Model)
}

// A max_tokens-only change in the shared config is picked up at turn end:
// the session refreshes, and until the refresh re-decides, the stale native
// advisor (which the per-request guard drops) is not reported as native.
func TestAdvisorMaxTokensChangeRefreshesAtTurnEnd(t *testing.T) {
	decided := session.AdvisorStatus{Enabled: true, Model: "anthropic/claude-opus-4-7:high", Mode: "auto"}
	changed := decided
	changed.MaxTokens = 4096
	adv := sessionmocks.NewMockAdvisorStore(t)
	adv.EXPECT().AdvisorStatus().Return(decided, nil).Once()
	adv.EXPECT().AdvisorStatus().Return(changed, nil)

	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("anthropic").Maybe()
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, provider.Request) (<-chan provider.Event, error) {
		ch := make(chan provider.Event, 1)
		ch <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "ok"}}}}
		close(ch)
		return ch, nil
	})
	ag := core.NewAgent(client, "claude-sonnet-4-6", "system", core.Registry{})
	ag.SetServerAdvisor(&provider.NativeAdvisor{Model: "claude-opus-4-7"})
	builder := sessionmocks.NewMockAgentBuilder(t)
	builder.EXPECT().BuildAgent().Return(ag, "anthropic", "claude-sonnet-4-6", nil).Once()

	refreshed := make(chan struct{}, 1)
	refresh := sessionmocks.NewMockToolRefresher(t)
	// The refresh re-decides native mode against the new settings.
	refresh.EXPECT().RefreshTools().RunAndReturn(func() error {
		ag.SetServerAdvisor(&provider.NativeAdvisor{Model: "claude-opus-4-7", MaxTokens: 4096})
		refreshed <- struct{}{}
		return nil
	}).Once()

	ctx, cancel := context.WithCancel(context.Background())
	c := session.NewController(ctx, session.Deps{Advisor: adv, AgentBuilder: builder, ToolRefresher: refresh})
	t.Cleanup(func() { cancel(); <-c.Done() })

	require.NoError(t, c.Submit(ctx, session.Input{Text: "hi", Mode: session.SubmitModeAuto, Origin: session.InputOriginUser}))
	select {
	case <-refreshed:
	case <-time.After(5 * time.Second):
		t.Fatal("max_tokens change did not trigger a refresh at turn end")
	}
	require.NoError(t, c.SetTitle(ctx, "barrier"))
	info := c.Info().Advisor
	require.Equal(t, 4096, info.MaxTokens)
	require.Equal(t, session.AdvisorEffectiveNative, info.Effective)
}

func TestAdvisorEffectiveNotNativeWhenStale(t *testing.T) {
	adv := sessionmocks.NewMockAdvisorStore(t)
	adv.EXPECT().AdvisorStatus().Return(session.AdvisorStatus{Enabled: true, Model: "anthropic/claude-opus-4-7:high", Mode: "auto", MaxTokens: 4096}, nil).Once()
	stale := opsAgent(t, "claude-sonnet-4-6")
	stale.SetServerAdvisor(&provider.NativeAdvisor{Model: "claude-opus-4-7"})
	builder := sessionmocks.NewMockAgentBuilder(t)
	builder.EXPECT().BuildAgent().Return(opsAgent(t, "m"), "p", "m", nil).Once()
	builder.EXPECT().BuildAgentFor("anthropic", "claude-sonnet-4-6").Return(stale, "anthropic", "claude-sonnet-4-6", nil).Once()
	store := opsStore(t)
	store.EXPECT().PersistModel(mock.Anything, mock.Anything).Once()
	ctx, cancel := context.WithCancel(context.Background())
	c := session.NewController(ctx, session.Deps{Advisor: adv, AgentBuilder: builder, SessionStore: store})
	t.Cleanup(func() { cancel(); <-c.Done() })
	require.NoError(t, c.SetModel(ctx, "anthropic", "claude-sonnet-4-6"))
	require.Empty(t, c.Info().Advisor.Effective)
}
