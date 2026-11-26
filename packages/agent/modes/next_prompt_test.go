package modes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"

	"github.com/stretchr/testify/require"
)

// Like the codemode setting tests, embed the unused base contract.
type nextPromptStoreRecorder struct {
	SettingsStore
	calls []bool
}

func (r *nextPromptStoreRecorder) SetNextPromptSuggestionsEnabled(enabled bool) error {
	r.calls = append(r.calls, enabled)
	return nil
}

func TestNextPromptSuggestionsSettingDefaultsEnabledAndToggles(t *testing.T) {
	store := &nextPromptStoreRecorder{}
	i := NewInteractive(InteractiveConfig{Terminal: &alertTestTerminal{}, SettingsStore: store})
	i.openSettingsDialog()
	var item *settingsItem
	for idx := range i.settingsDialog.items {
		if i.settingsDialog.items[idx].key == "next_prompt_suggestions_enabled" {
			item = &i.settingsDialog.items[idx]
			break
		}
	}
	require.NotNil(t, item, "next prompt suggestions setting should be listed")
	require.True(t, item.value, "next prompt suggestions should be enabled by default")

	i.ed.SetGhost("run the tests")
	require.Equal(t, "run the tests", i.ed.Ghost())
	i.applySettingToggle("next_prompt_suggestions_enabled", false)
	require.NotNil(t, i.cfg.NextPromptSuggestionsEnabled)
	require.False(t, *i.cfg.NextPromptSuggestionsEnabled)
	require.Empty(t, i.ed.Ghost(), "disabling must clear the shown suggestion")

	i.applySettingToggle("next_prompt_suggestions_enabled", true)
	require.True(t, *i.cfg.NextPromptSuggestionsEnabled)
	require.Equal(t, []bool{false, true}, store.calls)
}

// newPortNextPromptInteractive attaches an idle daemon session; tests feed
// turns through runNextPromptTurn and drain i.loopCalls themselves.
func newPortNextPromptInteractive(t *testing.T, enabled *bool) (*Interactive, *sessionmocks.MockPort, context.Context) {
	t.Helper()
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "np", Run: session.RunStateIdle}, Seq: 1}, nil).Once()
	port.EXPECT().Subscribe(mock.Anything, uint64(0x1)).Return(make(chan session.Event), nil).Once()
	i := NewInteractive(InteractiveConfig{Port: port, Terminal: &alertTestTerminal{}, NextPromptSuggestionsEnabled: enabled})
	i.rend = tui.NewRenderer(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Request goroutines derive from runCtx, so they end with the test.
	i.runCtx = ctx
	require.NoError(t, i.attachPort(ctx))
	return i, port, ctx
}

func runNextPromptTurn(ctx context.Context, i *Interactive, seq *uint64, queue []session.QueuedInput, end core.EvTurnEnd) {
	next := func() uint64 { *seq++; return *seq }
	i.handleSessionEvent(ctx, session.Event{Seq: next(), Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{ID: "np", Run: session.RunStateRunning}}})
	i.handleSessionEvent(ctx, session.Event{Seq: next(), Kind: session.EvAgent, Agent: end})
	if queue != nil {
		i.handleSessionEvent(ctx, session.Event{Seq: next(), Kind: session.EvQueue, Data: session.QueueEvent{Queue: queue}})
	}
	i.handleSessionEvent(ctx, session.Event{Seq: next(), Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{ID: "np", Run: session.RunStateIdle}}})
}

func drainLoopCall(t *testing.T, i *Interactive) {
	t.Helper()
	select {
	case fn := <-i.loopCalls:
		fn()
	case <-time.After(5 * time.Second):
		t.Fatal("no loop call delivered")
	}
}

func nextPromptInFlight(i *Interactive) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.nextPromptCancel != nil
}

func TestNextPromptShownAfterCleanTurn(t *testing.T) {
	i, port, ctx := newPortNextPromptInteractive(t, nil)
	port.EXPECT().PredictNextPrompt(mock.Anything).Return("run the tests", nil).Once()
	var seq uint64 = 1
	runNextPromptTurn(ctx, i, &seq, nil, core.EvTurnEnd{Stop: provider.StopEnd})
	drainLoopCall(t, i)
	require.Equal(t, "run the tests", i.ed.Ghost())
	require.False(t, nextPromptInFlight(i))
}

func TestNextPromptNotRequested(t *testing.T) {
	disabled := false
	cases := map[string]struct {
		enabled *bool
		end     core.EvTurnEnd
		queue   []session.QueuedInput
		draft   string
	}{
		"setting off":   {enabled: &disabled, end: core.EvTurnEnd{Stop: provider.StopEnd}},
		"cancelled":     {end: core.EvTurnEnd{Stop: provider.StopAborted, Err: context.Canceled}},
		"aborted":       {end: core.EvTurnEnd{Stop: provider.StopAborted}},
		"length":        {end: core.EvTurnEnd{Stop: provider.StopLength}},
		"errored":       {end: core.EvTurnEnd{Err: errors.New("boom")}},
		"queue pending": {end: core.EvTurnEnd{Stop: provider.StopEnd}, queue: []session.QueuedInput{{Input: session.Input{Text: "next"}}}},
		"input typed":   {end: core.EvTurnEnd{Stop: provider.StopEnd}, draft: "draft"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// No PredictNextPrompt expectation: any call fails the mock.
			i, _, ctx := newPortNextPromptInteractive(t, tc.enabled)
			i.ed.SetValue(tc.draft)
			var seq uint64 = 1
			runNextPromptTurn(ctx, i, &seq, tc.queue, tc.end)
			require.False(t, nextPromptInFlight(i))
			require.Empty(t, i.loopCalls)
		})
	}
}

// blockingPrediction makes PredictNextPrompt wait for cancellation and
// returns a channel closed once the request observed it.
func blockingPrediction(port *sessionmocks.MockPort) chan struct{} {
	cancelled := make(chan struct{})
	port.EXPECT().PredictNextPrompt(mock.Anything).RunAndReturn(func(ctx context.Context) (string, error) {
		<-ctx.Done()
		close(cancelled)
		return "late", nil
	}).Once()
	return cancelled
}

func waitClosed(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not happen", what)
	}
}

func TestNextPromptStaleAfterKeypress(t *testing.T) {
	i, port, ctx := newPortNextPromptInteractive(t, nil)
	cancelled := blockingPrediction(port)
	var seq uint64 = 1
	runNextPromptTurn(ctx, i, &seq, nil, core.EvTurnEnd{Stop: provider.StopEnd})
	i.mu.Lock()
	version := i.nextPromptVersion
	i.mu.Unlock()
	i.handleKey(ctx, tui.Key{Kind: tui.KeyRune, Rune: 'x'})
	waitClosed(t, cancelled, "request cancellation")
	i.ed.Clear()
	i.applyNextPrompt(version, "late")
	require.Empty(t, i.ed.Ghost(), "a result from before the keypress must be dropped")
}

func TestNextPromptStaleAfterSubmit(t *testing.T) {
	i, port, ctx := newPortNextPromptInteractive(t, nil)
	cancelled := blockingPrediction(port)
	submitted := make(chan struct{})
	port.EXPECT().Submit(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, session.Input) error { close(submitted); return nil }).Once()
	var seq uint64 = 1
	runNextPromptTurn(ctx, i, &seq, nil, core.EvTurnEnd{Stop: provider.StopEnd})
	i.mu.Lock()
	version := i.nextPromptVersion
	i.mu.Unlock()
	i.portSubmit(ctx, "hello", nil)
	waitClosed(t, cancelled, "request cancellation")
	waitClosed(t, submitted, "submit")
	i.applyNextPrompt(version, "late")
	require.Empty(t, i.ed.Ghost())
}

func TestNextPromptUnknownRequestIsSilent(t *testing.T) {
	i, port, ctx := newPortNextPromptInteractive(t, nil)
	finished := make(chan struct{})
	i.nextPromptFinished = func() { close(finished) }
	port.EXPECT().PredictNextPrompt(mock.Anything).Return("", fmt.Errorf("predict: %w", session.ErrUnknownRequest)).Once()
	var seq uint64 = 1
	runNextPromptTurn(ctx, i, &seq, nil, core.EvTurnEnd{Stop: provider.StopEnd})
	waitClosed(t, finished, "prediction goroutine")
	require.Empty(t, i.loopCalls, "an error must not post a result")
	require.Empty(t, i.ed.Ghost())
	i.mu.Lock()
	defer i.mu.Unlock()
	require.Empty(t, i.statusErr)
	require.Empty(t, i.statusOK)
}

func TestNextPromptTabAcceptsWithoutSubmitting(t *testing.T) {
	// No Submit/ApplyProfile expectation: Tab must neither send nor cycle.
	i, _ := newPortProfileInteractive(t, profileInfo(1, map[int]session.QuickModelShortcut{1: profSol, 2: profGPT5}))
	i.ed.SetGhost("run the tests")
	profKey(i, tui.Key{Kind: tui.KeyTab})
	require.Equal(t, "run the tests", i.ed.Value())
	require.Empty(t, i.ed.Ghost())
}

func TestNextPromptTabWithoutShownGhostCyclesProfile(t *testing.T) {
	i, port := newPortProfileInteractive(t, profileInfo(1, map[int]session.QuickModelShortcut{1: profSol, 2: profGPT5}))
	called := profExpectApply(port, 2, nil)
	i.ed.SetGhost("run the tests")
	// Typing drops the ghost.
	i.ed.Insert("hello")
	require.Empty(t, i.ed.Ghost())
	profKey(i, tui.Key{Kind: tui.KeyTab})
	profWait(t, called, "ApplyProfile")
	require.Equal(t, "hello", i.ed.Value())
}

func TestNextPromptEscDismissesWithoutArmingTree(t *testing.T) {
	i, _, _, _ := newTreePortInteractive(t, t.TempDir(), "/workspace/next-prompt-esc", []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "message"}}}})
	require.True(t, i.canArmSessionTreeEscape(), "precondition: a bare Escape could arm the tree gesture")
	i.ed.SetGhost("run the tests")
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc})
	require.Empty(t, i.ed.Ghost())
	require.True(t, i.ed.IsEmpty())
	i.mu.Lock()
	defer i.mu.Unlock()
	require.False(t, i.doubleEscape.armed, "the dismissing Escape must not arm the session-tree gesture")
}

func TestNextPromptEnterDoesNotSubmitGhost(t *testing.T) {
	// No Submit expectation: Enter on the empty input must not send.
	i, _ := newPortProfileInteractive(t, profileInfo(1, nil))
	i.ed.SetGhost("run the tests")
	profKey(i, tui.Key{Kind: tui.KeyEnter})
	require.Empty(t, i.ed.Ghost())
	require.True(t, i.ed.IsEmpty())
}

func startedNextPrompt(t *testing.T, i *Interactive) uint64 {
	t.Helper()
	require.True(t, nextPromptInFlight(i))
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.nextPromptVersion
}

func TestNextPromptViewKeysKeepGhostAndRequest(t *testing.T) {
	i, port, ctx := newPortNextPromptInteractive(t, nil)
	// The request may not reach the port before the test ends.
	port.EXPECT().PredictNextPrompt(mock.Anything).RunAndReturn(func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}).Maybe()
	var seq uint64 = 1
	runNextPromptTurn(ctx, i, &seq, nil, core.EvTurnEnd{Stop: provider.StopEnd})
	version := startedNextPrompt(t, i)
	i.ed.SetGhost("shown")
	for _, k := range []tui.Key{{Kind: tui.KeyPageUp}, {Kind: tui.KeyPageDown}, {Kind: tui.KeyMouseWheelUp}, {Kind: tui.KeyMouseWheelDown}, {Kind: tui.KeyCtrlO}} {
		i.handleKey(ctx, k)
		require.Equal(t, "shown", i.ed.Ghost(), "key %v dismissed the ghost", k.Kind)
		require.Equal(t, version, startedNextPrompt(t, i), "key %v invalidated the request", k.Kind)
	}
}

func TestNextPromptNewTurnDismissesGhost(t *testing.T) {
	i, port, ctx := newPortNextPromptInteractive(t, nil)
	cancelled := blockingPrediction(port)
	var seq uint64 = 1
	runNextPromptTurn(ctx, i, &seq, nil, core.EvTurnEnd{Stop: provider.StopEnd})
	version := startedNextPrompt(t, i)
	i.ed.SetGhost("shown")
	seq++
	i.handleSessionEvent(ctx, session.Event{Seq: seq, Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{ID: "np", Run: session.RunStateRunning}}})
	waitClosed(t, cancelled, "request cancellation")
	require.Empty(t, i.ed.Ghost())
	require.False(t, nextPromptInFlight(i))
	i.mu.Lock()
	require.NotEqual(t, version, i.nextPromptVersion)
	i.mu.Unlock()
}

func TestNextPromptSettingOffDropsLateResult(t *testing.T) {
	i, port, ctx := newPortNextPromptInteractive(t, nil)
	cancelled := blockingPrediction(port)
	var seq uint64 = 1
	runNextPromptTurn(ctx, i, &seq, nil, core.EvTurnEnd{Stop: provider.StopEnd})
	version := startedNextPrompt(t, i)
	i.applySettingToggle("next_prompt_suggestions_enabled", false)
	waitClosed(t, cancelled, "request cancellation")
	i.applyNextPrompt(version, "late")
	require.Empty(t, i.ed.Ghost())
}

func TestNextPromptLateResultDroppedWhileShellOrLoading(t *testing.T) {
	for name, set := range map[string]func(*Interactive){
		"shell":   func(i *Interactive) { i.shellRunning = true },
		"loading": func(i *Interactive) { i.sessionLoading = true },
	} {
		t.Run(name, func(t *testing.T) {
			i, port, ctx := newPortNextPromptInteractive(t, nil)
			release := make(chan struct{})
			port.EXPECT().PredictNextPrompt(mock.Anything).RunAndReturn(func(context.Context) (string, error) {
				<-release
				return "late", nil
			}).Once()
			var seq uint64 = 1
			runNextPromptTurn(ctx, i, &seq, nil, core.EvTurnEnd{Stop: provider.StopEnd})
			i.mu.Lock()
			set(i)
			i.mu.Unlock()
			close(release)
			drainLoopCall(t, i)
			require.Empty(t, i.ed.Ghost())
		})
	}
}

func TestNextPromptGhostRendersMuted(t *testing.T) {
	var out bytes.Buffer
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Terminal: &alertTestTerminal{}})
	i.rend = tui.NewRenderer(&out)
	i.rend.Resize(80, 24)
	i.ed.SetGhost("run the tests")
	i.publishEditorView()
	i.redraw()
	require.Contains(t, out.String(), tui.Dark.FGColor(tui.Dark.Muted, "run the tests"))
}
