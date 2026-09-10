package modes

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// interruptTestClient records outbound requests and answers each turn with a
// short assistant message so end-of-turn dispatch runs deterministically.
// Tests pause the goal inside onGoalContinuation so the scheduler never
// schedules a second continuation beyond the return under test. The first
// request can optionally block until the test cancels it, exercising the
// real Esc cancellation lifecycle.
type interruptTestClient struct {
	requests chan provider.Request

	mu                 sync.Mutex
	calls              int
	onGoalContinuation func()
	blockFirst         chan struct{}
	firstStarted       chan struct{}
}

func (c *interruptTestClient) Name() string { return "goal-interrupt-test" }

func (c *interruptTestClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	blockFirst := c.blockFirst
	firstStarted := c.firstStarted
	c.mu.Unlock()
	c.requests <- req
	if _, ok := tailMessageWithMeta(req, goalContinueMetaKey); ok {
		c.mu.Lock()
		onGoal := c.onGoalContinuation
		c.mu.Unlock()
		if onGoal != nil {
			onGoal()
		}
	}
	out := make(chan provider.Event, 1)
	if call == 1 && blockFirst != nil {
		if firstStarted != nil {
			close(firstStarted)
		}
		go func() {
			defer close(out)
			select {
			case <-ctx.Done():
				out <- provider.EventDone{Stop: provider.StopAborted, Err: ctx.Err()}
			case <-blockFirst:
				out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
					Role:    provider.RoleAssistant,
					Content: []provider.Content{provider.TextBlock{Text: "done"}},
				}}
			}
		}()
		return out, nil
	}
	out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: "done"}},
	}}
	close(out)
	return out, nil
}

func (c *interruptTestClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type interruptGoalStore struct {
	mu   sync.Mutex
	goal *core.SessionGoal
}

func (s *interruptGoalStore) current() *core.SessionGoal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSessionGoal(s.goal)
}

func (s *interruptGoalStore) persist(next *core.SessionGoal) error {
	s.mu.Lock()
	s.goal = cloneSessionGoal(next)
	s.mu.Unlock()
	return nil
}

func newInterruptTestHarness(goal *core.SessionGoal) (*Interactive, *interruptTestClient, *interruptGoalStore) {
	store := &interruptGoalStore{goal: cloneSessionGoal(goal)}
	client := &interruptTestClient{requests: make(chan provider.Request, 8)}
	agent := core.NewAgent(client, "test-model", "", goalToolRegistry())
	interactive := NewInteractive(InteractiveConfig{
		Agent:              agent,
		CurrentGoal:        store.current,
		PersistGoal:        store.persist,
		PersistGoalRuntime: store.persist,
	})
	interactive.runCtx = context.Background()
	return interactive, client, store
}

func lastMessageWithMeta(req provider.Request, key string) (provider.Message, bool) {
	for idx := len(req.Messages) - 1; idx >= 0; idx-- {
		if req.Messages[idx].Meta[key] == "true" {
			return req.Messages[idx], true
		}
	}
	return provider.Message{}, false
}

// tailMessageWithMeta reports whether the latest outbound message carries
// the key. Unlike lastMessageWithMeta it ignores earlier history rows, so a
// user turn following a goal continuation is not mistaken for one.
func tailMessageWithMeta(req provider.Request, key string) (provider.Message, bool) {
	if len(req.Messages) == 0 {
		return provider.Message{}, false
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Meta[key] == "true" {
		return last, true
	}
	return provider.Message{}, false
}

func TestEscapeKeepsActiveGoalActiveAndArmsReturn(t *testing.T) {
	interactive, _, store := newInterruptTestHarness(&core.SessionGoal{ID: "goal-1", Objective: "finish the work", Status: core.GoalActive})
	interactive.mu.Lock()
	interactive.busy = true
	interactive.cancelTurn = func() {}
	interactive.mu.Unlock()

	if done := interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("Esc during a turn should not exit")
	}
	if got := store.current(); got == nil || got.Status != core.GoalActive {
		t.Fatalf("goal after Esc = %#v, want active", got)
	}
	interactive.mu.Lock()
	state := interactive.interruptedGoalReturn
	interactive.mu.Unlock()
	if state == nil || !state.nextPromptArmed || state.goalID != "goal-1" {
		t.Fatalf("interrupted return state = %#v, want armed return for goal-1", state)
	}
}

func TestEscapeWithoutActiveGoalArmsNothing(t *testing.T) {
	interactive, _, _ := newInterruptTestHarness(nil)
	interactive.mu.Lock()
	interactive.busy = true
	interactive.cancelTurn = func() {}
	interactive.mu.Unlock()

	if done := interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("Esc during a turn should not exit")
	}
	interactive.mu.Lock()
	state := interactive.interruptedGoalReturn
	interactive.mu.Unlock()
	if state != nil {
		t.Fatalf("interrupted return state = %#v, want nil without an active goal", state)
	}
}

func TestInterruptReassessmentMessageRequiresActiveGoal(t *testing.T) {
	interactive, _, _ := newInterruptTestHarness(&core.SessionGoal{ID: "goal-1", Objective: "finish the work", Status: core.GoalActive, MissionID: "mission-1"})
	message, ok := interactive.goalInterruptReassessmentMessage()
	if !ok {
		t.Fatal("reassessment message unavailable for an active goal")
	}
	if message.Meta[goalInterruptReassessMetaKey] != "true" {
		t.Fatalf("reassessment message meta = %#v, want interrupt key", message.Meta)
	}
	text := userMessageText(message)
	for _, clause := range []string{
		"Handle the latest user message first",
		"continue the goal autonomously without waiting",
		"Call update_goal only for a genuine complete, blocked, or superseded transition",
		"finish the work",
		"goal_id goal-1",
		"mission_id mission-1",
	} {
		if !strings.Contains(text, clause) {
			t.Fatalf("reassessment message missing %q: %q", clause, text)
		}
	}

	paused, _, _ := newInterruptTestHarness(&core.SessionGoal{ID: "goal-1", Objective: "finish the work", Status: core.GoalPaused})
	if _, ok := paused.goalInterruptReassessmentMessage(); ok {
		t.Fatal("paused goal should not build a reassessment message")
	}
}

func TestInterruptReassessmentMessageHiddenFromTranscript(t *testing.T) {
	message := provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "interrupted; handle this first"}},
		Meta:    map[string]string{goalInterruptReassessMetaKey: "true"},
	}
	if !isHiddenTranscriptMessage(message) {
		t.Fatal("interrupt reassessment message should be hidden")
	}
	visible := provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "ordinary user input"}},
	}
	if isHiddenTranscriptMessage(visible) {
		t.Fatal("ordinary user input should remain visible")
	}
	filtered := filterHiddenTranscriptMessages([]provider.Message{message, visible})
	if len(filtered) != 1 || userMessageText(filtered[0]) != "ordinary user input" {
		t.Fatalf("filtered transcript = %#v, want only the ordinary message", filtered)
	}
}

func TestInterruptedGoalResumesOnceAfterNextUserTurn(t *testing.T) {
	interactive, client, store := newInterruptTestHarness(&core.SessionGoal{ID: "goal-1", MissionID: "mission-1", Objective: "finish the work", Status: core.GoalActive})
	client.mu.Lock()
	client.blockFirst = make(chan struct{})
	client.firstStarted = make(chan struct{})
	client.mu.Unlock()
	// Start a real goal turn through the scheduler, then Esc-cancel it while
	// the provider stream is blocked. The cancellation unwinds through the
	// genuine end-of-turn path.
	if !interactive.requestGoalContinuationIfIdle(context.Background()) {
		t.Fatal("goal continuation did not start")
	}
	goalReq := receiveRequest(t, client.requests)
	if _, ok := lastMessageWithMeta(goalReq, goalContinueMetaKey); !ok {
		t.Fatalf("initial request tail = %#v, want goal continuation", goalReq.Messages)
	}
	select {
	case <-client.firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("goal turn did not reach the provider")
	}
	if done := interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("Esc during a turn should not exit")
	}
	waitInteractiveIdle(t, interactive)
	if got := store.current(); got == nil || got.Status != core.GoalActive {
		t.Fatalf("goal after Esc = %#v, want active", got)
	}
	// Esc must yield to user input: no autonomous request may start before
	// the next ordinary prompt.
	select {
	case extra := <-client.requests:
		t.Fatalf("goal restarted before the next user prompt: %#v", extra.Messages)
	case <-time.After(200 * time.Millisecond):
	}
	client.mu.Lock()
	client.onGoalContinuation = func() {
		// Settle the goal after the single return under test so the
		// scheduler does not schedule further continuations.
		_ = store.persist(&core.SessionGoal{ID: "goal-1", MissionID: "mission-1", Objective: "finish the work", Status: core.GoalPaused})
	}
	client.mu.Unlock()

	interactive.startTurnRequest(context.Background(), "quick question", nil, false, false)

	userReq := receiveRequest(t, client.requests)
	hidden, ok := lastMessageWithMeta(userReq, goalInterruptReassessMetaKey)
	if !ok {
		t.Fatalf("user request has no hidden reassessment message: %#v", userReq.Messages)
	}
	if !strings.Contains(userMessageText(hidden), "Handle the latest user message first") {
		t.Fatalf("hidden reassessment text = %q", userMessageText(hidden))
	}
	if got := userMessageText(userReq.Messages[len(userReq.Messages)-1]); !strings.HasSuffix(got, "quick question") {
		t.Fatalf("user prompt tail = %q, want the latest user message first", got)
	}

	returnReq := receiveRequest(t, client.requests)
	if _, ok := lastMessageWithMeta(returnReq, goalContinueMetaKey); !ok {
		t.Fatalf("expected one automatic goal continuation, got %#v", returnReq.Messages)
	}
	waitInteractiveIdle(t, interactive)

	select {
	case extra := <-client.requests:
		t.Fatalf("unexpected fourth provider request: %#v", extra.Messages)
	case <-time.After(200 * time.Millisecond):
	}
	if got := client.count(); got != 3 {
		t.Fatalf("provider call count = %d, want cancelled goal turn plus user turn plus one goal return", got)
	}
	if got := store.current(); got == nil || got.Status != core.GoalPaused {
		t.Fatalf("goal after resumed turn = %#v, want paused test settlement", got)
	}
}

func TestInterruptedGoalStaysPausedAfterExplicitPause(t *testing.T) {
	interactive, client, store := newInterruptTestHarness(&core.SessionGoal{ID: "goal-1", Objective: "finish the work", Status: core.GoalActive})
	interactive.mu.Lock()
	interactive.busy = true
	interactive.cancelTurn = func() {}
	interactive.mu.Unlock()
	if done := interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("Esc during a turn should not exit")
	}

	interactive.runGoalCommand(context.Background(), "/goal pause", []string{"/goal", "pause"})
	if got := store.current(); got == nil || got.Status != core.GoalPaused {
		t.Fatalf("goal after /goal pause = %#v, want paused", got)
	}

	interactive.startTurnRequest(context.Background(), "follow-up", nil, false, false)
	_ = receiveRequest(t, client.requests)
	waitInteractiveIdle(t, interactive)
	select {
	case extra := <-client.requests:
		t.Fatalf("paused goal resumed unexpectedly: %#v", extra.Messages)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestInterruptedGoalClearedOrReplacedNeverResumes(t *testing.T) {
	t.Run("cleared", func(t *testing.T) {
		interactive, client, store := newInterruptTestHarness(&core.SessionGoal{ID: "goal-1", Objective: "finish the work", Status: core.GoalActive})
		interactive.mu.Lock()
		interactive.busy = true
		interactive.cancelTurn = func() {}
		interactive.mu.Unlock()
		if done := interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
			t.Fatal("Esc during a turn should not exit")
		}
		interactive.runGoalCommand(context.Background(), "/goal clear", []string{"/goal", "clear"})
		if got := store.current(); got != nil {
			t.Fatalf("goal after clear = %#v, want nil", got)
		}
		interactive.startTurnRequest(context.Background(), "follow-up", nil, false, false)
		_ = receiveRequest(t, client.requests)
		waitInteractiveIdle(t, interactive)
		select {
		case extra := <-client.requests:
			t.Fatalf("cleared goal resumed unexpectedly: %#v", extra.Messages)
		case <-time.After(200 * time.Millisecond):
		}
	})

	t.Run("replaced", func(t *testing.T) {
		interactive, client, store := newInterruptTestHarness(&core.SessionGoal{ID: "goal-1", Objective: "finish the work", Status: core.GoalActive})
		client.onGoalContinuation = func() {
			_ = store.persist(&core.SessionGoal{ID: "goal-2", Objective: "do something else", Status: core.GoalPaused})
		}
		interactive.mu.Lock()
		interactive.busy = true
		interactive.cancelTurn = func() {}
		interactive.mu.Unlock()
		if done := interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
			t.Fatal("Esc during a turn should not exit")
		}
		// Replace the goal with a distinct persisted identity so the stale
		// Esc return cannot match it.
		if err := store.persist(&core.SessionGoal{ID: "goal-2", Objective: "do something else", Status: core.GoalActive}); err != nil {
			t.Fatal(err)
		}
		interactive.mu.Lock()
		interactive.busy = false
		interactive.mu.Unlock()
		interactive.startTurnRequest(context.Background(), "follow-up", nil, false, false)
		userReq := receiveRequest(t, client.requests)
		if _, ok := lastMessageWithMeta(userReq, goalInterruptReassessMetaKey); ok {
			t.Fatalf("stale interruption leaked into replaced goal: %#v", userReq.Messages)
		}
		waitInteractiveIdle(t, interactive)
		// Drain the replacement goal's own continuation so the test ends idle
		// without asserting on its scheduling here.
		select {
		case <-client.requests:
			waitInteractiveIdle(t, interactive)
		case <-time.After(200 * time.Millisecond):
		}
	})
}

func TestEscapePreservesNoProgressAllowance(t *testing.T) {
	// An Esc-cancelled corrective continuation must not consume the
	// no-progress allowance or stall the still-active goal.
	interactive, client, store := newInterruptTestHarness(&core.SessionGoal{
		ID:                         "goal-1",
		Objective:                  "finish the work",
		Status:                     core.GoalActive,
		ConsecutiveNoProgressTurns: maxConsecutiveGoalNoProgressTurns - 1,
	})
	client.mu.Lock()
	client.blockFirst = make(chan struct{})
	client.firstStarted = make(chan struct{})
	client.mu.Unlock()
	if !interactive.requestGoalContinuationIfIdle(context.Background()) {
		t.Fatal("goal continuation did not start")
	}
	_ = receiveRequest(t, client.requests)
	select {
	case <-client.firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("goal turn did not reach the provider")
	}
	if done := interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("Esc during a turn should not exit")
	}
	waitInteractiveIdle(t, interactive)
	got := store.current()
	if got == nil || got.Status != core.GoalActive {
		t.Fatalf("goal after Esc = %#v, want active", got)
	}
	if got.ConsecutiveNoProgressTurns != maxConsecutiveGoalNoProgressTurns-1 {
		t.Fatalf("no-progress turns = %d, want %d", got.ConsecutiveNoProgressTurns, maxConsecutiveGoalNoProgressTurns-1)
	}
}

func TestExplicitResumeAfterEscapeStartsGoalImmediately(t *testing.T) {
	interactive, client, store := newInterruptTestHarness(&core.SessionGoal{ID: "goal-1", Objective: "finish the work", Status: core.GoalActive})
	interactive.mu.Lock()
	interactive.busy = true
	interactive.cancelTurn = func() {}
	interactive.mu.Unlock()
	if done := interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("Esc during a turn should not exit")
	}
	interactive.mu.Lock()
	interactive.busy = false
	interactive.mu.Unlock()

	interactive.runGoalCommand(context.Background(), "/goal pause", []string{"/goal", "pause"})
	if got := store.current(); got == nil || got.Status != core.GoalPaused {
		t.Fatalf("goal after pause = %#v, want paused", got)
	}
	// Re-arm an interruption against the paused goal is impossible, so arm
	// directly: resume must clear it and start the goal without waiting for
	// an ordinary prompt.
	interactive.mu.Lock()
	interactive.interruptedGoalReturn = &interruptedGoalState{goalID: "goal-1", nextPromptArmed: true}
	interactive.mu.Unlock()
	interactive.runGoalCommand(context.Background(), "/goal resume", []string{"/goal", "resume"})
	if got := store.current(); got == nil || got.Status != core.GoalActive {
		t.Fatalf("goal after resume = %#v, want active", got)
	}
	goalReq := receiveRequest(t, client.requests)
	if _, ok := lastMessageWithMeta(goalReq, goalContinueMetaKey); !ok {
		t.Fatalf("resume request tail = %#v, want goal continuation", goalReq.Messages)
	}
	waitInteractiveIdle(t, interactive)
}

func TestExplicitReplacementAfterEscapeStartsNewGoal(t *testing.T) {
	interactive, client, store := newInterruptTestHarness(&core.SessionGoal{ID: "goal-1", Objective: "finish the work", Status: core.GoalActive})
	interactive.mu.Lock()
	interactive.busy = true
	interactive.cancelTurn = func() {}
	interactive.mu.Unlock()
	if done := interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("Esc during a turn should not exit")
	}
	interactive.mu.Lock()
	interactive.busy = false
	interactive.mu.Unlock()

	interactive.runGoalCommand(context.Background(), "/goal do something else", []string{"/goal", "do"})
	got := store.current()
	if got == nil || got.Status != core.GoalActive || got.Objective != "do something else" {
		t.Fatalf("goal after replacement = %#v, want the new active objective", got)
	}
	goalReq := receiveRequest(t, client.requests)
	if _, ok := lastMessageWithMeta(goalReq, goalContinueMetaKey); !ok {
		t.Fatalf("replacement request tail = %#v, want goal continuation", goalReq.Messages)
	}
	if text := userMessageText(goalReq.Messages[len(goalReq.Messages)-1]); !strings.Contains(text, "do something else") {
		t.Fatalf("replacement continuation text = %q, want the new objective", text)
	}
	waitInteractiveIdle(t, interactive)
}

func TestInterruptedGoalCancelledOwningTurnKeepsArmedReturn(t *testing.T) {
	interactive, client, store := newInterruptTestHarness(&core.SessionGoal{ID: "goal-1", Objective: "finish the work", Status: core.GoalActive})
	client.mu.Lock()
	client.blockFirst = make(chan struct{})
	client.firstStarted = make(chan struct{})
	client.mu.Unlock()
	if !interactive.requestGoalContinuationIfIdle(context.Background()) {
		t.Fatal("goal continuation did not start")
	}
	_ = receiveRequest(t, client.requests)
	select {
	case <-client.firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("goal turn did not reach the provider")
	}
	if done := interactive.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}); done {
		t.Fatal("Esc during a turn should not exit")
	}
	waitInteractiveIdle(t, interactive)
	// The cancelled goal turn unwinds without restarting autonomy, but the
	// armed return for the next ordinary prompt survives cleanup.
	if got := store.current(); got == nil || got.Status != core.GoalActive {
		t.Fatalf("goal after Esc = %#v, want active", got)
	}
	interactive.mu.Lock()
	state := interactive.interruptedGoalReturn
	interactive.mu.Unlock()
	if state == nil || !state.nextPromptArmed {
		t.Fatalf("armed return after cancellation = %#v, want next-prompt intent", state)
	}
	select {
	case extra := <-client.requests:
		t.Fatalf("goal restarted before the next user prompt: %#v", extra.Messages)
	case <-time.After(200 * time.Millisecond):
	}
}
