package modes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	toolspkg "github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/auth"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type alertTestTerminal struct {
	mu   sync.Mutex
	data []byte
}

func (t *alertTestTerminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	return len(p), nil
}
func (t *alertTestTerminal) WriteString(s string) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, s...)
	return len(s), nil
}
func (t *alertTestTerminal) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.data)
}

func (*alertTestTerminal) Size() (int, int) { return 80, 24 }
func (*alertTestTerminal) OnResize(func())  {}
func (*alertTestTerminal) EnterRaw() (func() error, error) {
	return func() error { return nil }, nil
}
func (*alertTestTerminal) ReadByte() (byte, error) { return 0, io.EOF }
func (*alertTestTerminal) PeekByteTimeout(time.Duration) (byte, bool, error) {
	return 0, false, nil
}
func (*alertTestTerminal) SetNonblock(bool) error { return nil }

func TestInteractiveAlertUsesSharedTerminalPolicy(t *testing.T) {
	term := &alertTestTerminal{}
	i := &Interactive{cfg: InteractiveConfig{Terminal: term}}

	i.Alert("question-ext", extproto.AlertRequest{Kind: extproto.AlertKindBell, Reason: "question_ready"})
	if got := term.String(); got != "\a" {
		t.Fatalf("alert output = %q, want standalone BEL", got)
	}
}

func TestInteractiveAlertRespectsDisabledPolicy(t *testing.T) {
	term := &alertTestTerminal{}
	disabled := false
	i := &Interactive{cfg: InteractiveConfig{Terminal: term, TerminalAlertsEnabled: &disabled}}

	i.Alert("question-ext", extproto.AlertRequest{Kind: extproto.AlertKindBell, Reason: "question_ready"})
	if got := term.String(); got != "" {
		t.Fatalf("disabled alert output = %q, want empty", got)
	}
}

// newPortAlertInteractive attaches an idle daemon session; tests then feed
// the daemon's turn events through handleSessionEvent.
func newPortAlertInteractive(t *testing.T, term *alertTestTerminal) (*Interactive, context.Context) {
	t.Helper()
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "alert", Run: session.RunStateIdle}, Seq: 1}, nil).Once()
	port.EXPECT().Subscribe(mock.Anything, uint64(0x1)).Return(make(chan session.Event), nil).Once()
	i := NewInteractive(InteractiveConfig{Port: port, Terminal: term})
	i.rend = tui.NewRenderer(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, i.attachPort(ctx))
	return i, ctx
}

// runPortTurn plays one daemon turn: running Info, the agent events, idle Info.
func runPortTurn(ctx context.Context, i *Interactive, seq *uint64, queue []session.QueuedInput, agentEvents ...core.AgentEvent) {
	next := func() uint64 { *seq++; return *seq }
	i.handleSessionEvent(ctx, session.Event{Seq: next(), Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{ID: "alert", Run: session.RunStateRunning}}})
	for _, ev := range agentEvents {
		i.handleSessionEvent(ctx, session.Event{Seq: next(), Kind: session.EvAgent, Agent: ev})
	}
	if queue != nil {
		i.handleSessionEvent(ctx, session.Event{Seq: next(), Kind: session.EvQueue, Data: session.QueueEvent{Queue: queue}})
	}
	i.handleSessionEvent(ctx, session.Event{Seq: next(), Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{ID: "alert", Run: session.RunStateIdle}}})
}

func TestCancelledMainTurnDoesNotBell(t *testing.T) {
	for _, end := range []core.EvTurnEnd{
		{Stop: provider.StopAborted},
		{Stop: provider.StopError, Err: context.Canceled},
	} {
		term := &alertTestTerminal{}
		i, ctx := newPortAlertInteractive(t, term)
		seq := uint64(1)
		runPortTurn(ctx, i, &seq, nil, core.EvTurnStart{}, end)
		i.redraw()
		require.Empty(t, term.String(), "cancelled main turn must not ring (%+v)", end)
	}
}

func TestMainTurnEmitsBellAfterVisibleResponse(t *testing.T) {
	term := &alertTestTerminal{}
	i, ctx := newPortAlertInteractive(t, term)
	seq := uint64(1)
	reply := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "answer"}}}
	runPortTurn(ctx, i, &seq, nil,
		core.EvTurnStart{}, core.EvAssistantStart{}, core.EvTextDelta{Delta: "answer"},
		core.EvAssistantMessage{Message: reply}, core.EvTurnEnd{Stop: provider.StopEnd}, core.EvDone{})
	i.mu.Lock()
	for i.stream.Active() {
		// Paced text is still typing: the bell must wait for the final frame.
		i.mu.Unlock()
		i.redraw()
		require.Empty(t, term.String(), "bell rang before the response finished painting")
		i.mu.Lock()
		i.stream.Tick()
	}
	i.mu.Unlock()
	i.redraw()
	require.Equal(t, "\a", term.String())
	i.redraw()
	require.Equal(t, "\a", term.String(), "bell must ring once per turn")
}

func TestPortMainTurnAlertPolicy(t *testing.T) {
	tests := []struct {
		name  string
		end   core.EvTurnEnd
		queue []session.QueuedInput
		want  string
	}{
		{name: "error", end: core.EvTurnEnd{Stop: provider.StopError, Err: errors.New("boom")}, want: "\a"},
		{name: "length", end: core.EvTurnEnd{Stop: provider.StopLength}, want: "\a"},
		{name: "queued follow-up", end: core.EvTurnEnd{Stop: provider.StopEnd}, queue: []session.QueuedInput{{ID: "1", Input: session.Input{Text: "next"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			term := &alertTestTerminal{}
			i, ctx := newPortAlertInteractive(t, term)
			seq := uint64(1)
			runPortTurn(ctx, i, &seq, tt.queue, core.EvTurnStart{}, tt.end)
			i.redraw()
			require.Equal(t, tt.want, term.String())
		})
	}
	t.Run("disabled", func(t *testing.T) {
		term := &alertTestTerminal{}
		i, ctx := newPortAlertInteractive(t, term)
		disabled := false
		i.cfg.TerminalAlertsEnabled = &disabled
		seq := uint64(1)
		runPortTurn(ctx, i, &seq, nil, core.EvTurnStart{}, core.EvTurnEnd{Stop: provider.StopEnd})
		i.redraw()
		require.Empty(t, term.String())
	})
	t.Run("rescue pending", func(t *testing.T) {
		// The daemon holds a recoverable failure as a rescue request
		// instead of ending the turn; it needs the user now.
		term := &alertTestTerminal{}
		i, ctx := newPortAlertInteractive(t, term)
		i.handleSessionEvent(ctx, session.Event{Seq: 2, Kind: session.EvPending, Data: session.PendingEvent{Pending: session.PendingRequest{ID: "r1", Kind: "rescue", Rescue: &session.RescueOptions{Error: "boom", Candidates: []string{"openai/gpt-x"}}}}})
		require.Equal(t, "\a", term.String())
	})
	t.Run("idle info without a turn", func(t *testing.T) {
		term := &alertTestTerminal{}
		i, ctx := newPortAlertInteractive(t, term)
		seq := uint64(1)
		runPortTurn(ctx, i, &seq, nil)
		i.redraw()
		require.Empty(t, term.String())
	})
}

func TestMainAlertWaitsForPacedText(t *testing.T) {
	term := &alertTestTerminal{}
	i := NewInteractive(InteractiveConfig{Terminal: term})
	i.rend = tui.NewRenderer(io.Discard)
	i.stream.Start(-1, 0)
	i.stream.Push("final answer")
	i.stream.Finish()
	i.pendingAlert = &extproto.AlertRequest{Kind: extproto.AlertKindBell, Reason: "agent_done"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go i.runStreamPacer(ctx)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		i.mu.Lock()
		ready := !i.stream.Active() && i.pendingAlert != nil
		i.mu.Unlock()
		if ready {
			i.redraw()
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := term.String(); got != "\a" {
		t.Fatalf("paced alert output = %q, want BEL after final redraw", got)
	}
	i.mu.Lock()
	on, alert := i.stream.Active(), i.pendingAlert
	i.mu.Unlock()
	if on || alert != nil {
		t.Fatalf("stream state after alert: on=%v alert=%v", on, alert)
	}
}

func TestMainAlertReasonPolicy(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name       string
		ctx        context.Context
		err        error
		turnErr    error
		stop       provider.StopReason
		awaiting   bool
		queued     bool
		rescue     bool
		recovering bool
		auto       bool
		want       alertReason
	}{
		{name: "normal", ctx: context.Background(), stop: provider.StopEnd, want: "agent_done"},
		{name: "length", ctx: context.Background(), stop: provider.StopLength, want: "response_truncated"},
		{name: "returned error", ctx: context.Background(), err: errors.New("boom"), stop: provider.StopError, want: "agent_error"},
		{name: "guard error", ctx: context.Background(), stop: provider.StopError, want: "agent_error"},
		{name: "rescue", ctx: context.Background(), err: errors.New("temporary"), stop: provider.StopError, rescue: true, want: "rescue_required"},
		{name: "cancelled", ctx: cancelled, stop: provider.StopAborted},
		{name: "aborted", ctx: context.Background(), stop: provider.StopAborted},
		{name: "queued", ctx: context.Background(), stop: provider.StopEnd, queued: true},
		{name: "recovering", ctx: context.Background(), stop: provider.StopError, recovering: true},
		{name: "auto compacting", ctx: context.Background(), stop: provider.StopEnd, auto: true},
		{name: "startup pre", ctx: context.Background(), stop: provider.StopEnd, awaiting: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mainAlertReason(tt.ctx, tt.err, tt.turnErr, tt.stop, tt.awaiting, tt.queued, tt.rescue, tt.recovering, tt.auto)
			if got != tt.want {
				t.Fatalf("mainAlertReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTerminalAlertsSettingDefaultsEnabledAndToggles(t *testing.T) {
	i := NewInteractive(InteractiveConfig{Terminal: &alertTestTerminal{}})
	i.openSettingsDialog()
	var item *settingsItem
	for idx := range i.settingsDialog.items {
		if i.settingsDialog.items[idx].key == "terminal_alerts_enabled" {
			item = &i.settingsDialog.items[idx]
			break
		}
	}
	if item == nil || !item.value {
		t.Fatal("terminal alerts setting should be present and enabled by default")
	}

	i.applySettingToggle("terminal_alerts_enabled", false)
	if i.cfg.TerminalAlertsEnabled == nil || *i.cfg.TerminalAlertsEnabled {
		t.Fatal("terminal alerts setting did not disable")
	}
	i.applySettingToggle("terminal_alerts_enabled", true)
	if i.cfg.TerminalAlertsEnabled == nil || !*i.cfg.TerminalAlertsEnabled {
		t.Fatal("terminal alerts setting did not re-enable")
	}
}

func TestConfirmationAlertsAtEnqueue(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, side := range []bool{false, true} {
			term := &alertTestTerminal{}
			i := &Interactive{cfg: InteractiveConfig{Terminal: term, TerminalAlertsEnabled: &enabled}, confirmDialog: newConfirmDialog(), dirty: make(chan struct{}, 1)}
			call := core.ToolCallConfirmation{ID: "call", Name: "bash"}
			if side {
				i.btwDialog = &btwDialog{active: true, generation: 1, turns: []btwTurn{{Tools: []tui.ToolCallView{{ID: "call", Name: "bash"}}}}}
				call.Origin = btwOrigin(1)
			}
			done := make(chan core.ConfirmDecision, 1)
			go func() { done <- i.ConfirmToolCall(call) }()
			<-i.dirty
			want := ""
			if enabled {
				want = "\a"
			}
			if got := term.String(); got != want {
				t.Fatalf("enabled=%v side=%v: %q, want %q", enabled, side, got, want)
			}
			i.confirmDialog.CancelAll("test")
			<-done
			if side {
				call.Origin = btwOrigin(2)
				if i.ConfirmToolCall(call).Allow {
					t.Fatal("stale request allowed")
				}
				if term.String() != want {
					t.Fatal("stale request rang")
				}
			}
		}
	}
}

func TestLoginActionAlertDeduplicated(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, manual := range []bool{true, false} {
			term := &alertTestTerminal{}
			i := &Interactive{cfg: InteractiveConfig{Terminal: term, TerminalAlertsEnabled: &enabled}, dialog: &loginDialog{step: loginStepWaiting}}
			i.showLoginAction(manual, "https://example.test/login")
			i.handleAuthEvent(auth.Event{Kind: "started", URL: "https://example.test/login"})
			i.dialog.Render(tui.Theme{}, 80)
			i.dialog.Render(tui.Theme{}, 80)
			i.dialog.Close()
			i.showLoginAction(manual, "https://example.test/other")
			want := ""
			if enabled {
				want = "\a"
			}
			if term.String() != want {
				t.Fatalf("manual=%v enabled=%v: %q", manual, enabled, term.String())
			}
		}
	}
}

func TestLoadedGoalsDoNotAlert(t *testing.T) {
	for _, status := range []core.GoalStatus{core.GoalBlocked, core.GoalPaused} {
		term := &alertTestTerminal{}
		i := newQueuePortInteractive(t, sessionmocks.NewMockPort(t), session.Snapshot{
			Info: session.Info{Goal: &session.GoalSummary{Status: string(status)}},
		}, InteractiveConfig{Terminal: term})
		require.Equal(t, status, i.goalStatus)
		if term.String() != "" {
			t.Fatal("loading a session rang")
		}
	}
}

// Reuse the scripted two-request client, but return a goal transition from
// its tool so the real /btw agent sink must route an EvToolResult.
type alertGoalTool struct {
	btwEchoTool
	status core.GoalStatus
}

func (t *alertGoalTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	return core.ToolResult{Details: toolspkg.GoalUpdate{Status: t.status, Reason: "needs help"}}, nil
}

func TestBtwGoalUpdateDoesNotAlertMainSession(t *testing.T) {
	for _, status := range []core.GoalStatus{core.GoalBlocked, core.GoalPaused} {
		term := &alertTestTerminal{}
		client := &btwToolTestClient{done: make(chan struct{})}
		ag := core.NewAgent(client, "test-model", "", core.Registry{"echo": &alertGoalTool{status: status}})
		i := NewInteractive(InteractiveConfig{Agent: ag, Terminal: term, CWD: t.TempDir(), Theme: tui.Dark})
		i.goalStatus = core.GoalActive
		i.openBtwDialog([]string{"update goal"})
		timeout := time.NewTimer(5 * time.Second)
		defer timeout.Stop()
		for i.btwDialog.Loading() {
			select {
			case <-i.dirty:
			case <-timeout.C:
				t.Fatal("side-chat turn did not complete")
			}
		}
		i.btwDialog.mu.Lock()
		tools := i.btwDialog.turns[0].Tools
		if len(tools) != 1 || !tools[0].Done || tools[0].Error {
			t.Errorf("side-chat tool did not complete: %+v", tools)
		}
		i.btwDialog.mu.Unlock()
		i.mu.Lock()
		gotStatus := i.goalStatus
		i.mu.Unlock()
		if gotStatus != core.GoalActive {
			t.Fatalf("main goal changed to %s", gotStatus)
		}
		if term.String() != "" {
			t.Fatalf("side-chat goal rang: %q", term.String())
		}
	}
}
