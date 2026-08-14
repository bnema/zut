package modes

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

type noProgressGoalClient struct {
	mu    sync.Mutex
	calls int
	hits  chan int
}

func (c *noProgressGoalClient) Name() string { return "goal-no-progress-test" }

func (c *noProgressGoalClient) Stream(_ context.Context, _ provider.Request) (<-chan provider.Event, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()
	c.hits <- call
	out := make(chan provider.Event, 1)
	out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: "Continuing autonomously."}},
	}}
	close(out)
	return out, nil
}

func (c *noProgressGoalClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func TestGoalContinuationDoesNotResendNoProgressMessagesIndefinitely(t *testing.T) {
	var mu sync.Mutex
	var goal *core.SessionGoal
	current := func() *core.SessionGoal {
		mu.Lock()
		defer mu.Unlock()
		return cloneSessionGoal(goal)
	}
	persist := func(next *core.SessionGoal) error {
		mu.Lock()
		goal = cloneSessionGoal(next)
		mu.Unlock()
		return nil
	}
	client := &noProgressGoalClient{hits: make(chan int, 3)}
	agent := core.NewAgent(client, "test", "", core.Registry{tools.UpdateGoalToolName: &tools.UpdateGoalTool{}})
	interactive := NewInteractive(InteractiveConfig{
		Agent:              agent,
		CurrentGoal:        current,
		PersistGoal:        persist,
		PersistGoalRuntime: persist,
	})
	interactive.runCtx = context.Background()
	interactive.runGoalCommand(context.Background(), "/goal finish the work", []string{"/goal", "finish", "the", "work"})

	for want := 1; want <= maxConsecutiveGoalNoProgressTurns; want++ {
		select {
		case got := <-client.hits:
			if got != want {
				t.Fatalf("provider call = %d, want %d", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("provider call %d did not start", want)
		}
	}
	waitInteractiveIdle(t, interactive)
	if got := current(); got == nil || got.Status != core.GoalStalled {
		t.Fatalf("goal after repeated no-progress responses = %#v", got)
	}
	if got := client.count(); got != maxConsecutiveGoalNoProgressTurns {
		t.Fatalf("provider call count = %d, want %d", got, maxConsecutiveGoalNoProgressTurns)
	}
}

func TestGoalRuntimeStallsAfterRepeatedNoProgress(t *testing.T) {
	var mu sync.Mutex
	goal := &core.SessionGoal{ID: "goal-1", Objective: "finish the work", Status: core.GoalActive}
	current := func() *core.SessionGoal {
		mu.Lock()
		defer mu.Unlock()
		return cloneSessionGoal(goal)
	}
	persistRuntime := func(next *core.SessionGoal) error {
		mu.Lock()
		goal = cloneSessionGoal(next)
		mu.Unlock()
		return nil
	}
	interactive := NewInteractive(InteractiveConfig{
		CurrentGoal:        current,
		PersistGoal:        persistRuntime,
		PersistGoalRuntime: persistRuntime,
	})

	first, err := interactive.startGoalRun(current())
	if err != nil || first == nil {
		t.Fatalf("start first goal run = (%v, %v), want run without error", first, err)
	}
	if !interactive.finishGoalRun() {
		t.Fatal("first non-progress run should receive one corrective continuation")
	}
	if got := current(); got.ConsecutiveNoProgressTurns != 1 || got.Status != core.GoalActive || got.ContinuationID != "" {
		t.Fatalf("first run state = %#v", got)
	}

	second, err := interactive.startGoalRun(current())
	if err != nil || second == nil {
		t.Fatalf("start second goal run = (%v, %v), want run without error", second, err)
	}
	if interactive.finishGoalRun() {
		t.Fatal("second non-progress run scheduled another continuation")
	}
	got := current()
	if got.Status != core.GoalStalled || got.ConsecutiveNoProgressTurns != maxConsecutiveGoalNoProgressTurns {
		t.Fatalf("stalled goal = %#v", got)
	}
	if got.Reason == "" || got.ContinuationID != "" {
		t.Fatalf("stalled goal diagnostics = %#v", got)
	}
}

func TestGoalRuntimeBudgetIsOptionalAndEnforcedWhenSet(t *testing.T) {
	for _, test := range []struct {
		name       string
		budget     *uint64
		wantStatus core.GoalStatus
		wantNext   bool
	}{
		{name: "unlimited", wantStatus: core.GoalActive, wantNext: true},
		{name: "limited", budget: uint64ptr(100), wantStatus: core.GoalBudgetLimited},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			goal := &core.SessionGoal{ID: "goal-1", Objective: "finish the work", Status: core.GoalActive, TokenBudget: test.budget}
			current := func() *core.SessionGoal {
				mu.Lock()
				defer mu.Unlock()
				return cloneSessionGoal(goal)
			}
			persist := func(next *core.SessionGoal) error {
				mu.Lock()
				goal = cloneSessionGoal(next)
				mu.Unlock()
				return nil
			}
			interactive := NewInteractive(InteractiveConfig{CurrentGoal: current, PersistGoal: persist, PersistGoalRuntime: persist})
			run, err := interactive.startGoalRun(current())
			if err != nil || run == nil {
				t.Fatalf("start goal run = (%v, %v)", run, err)
			}
			interactive.observeGoalRun(core.EvToolCall{Name: "bash"})
			interactive.observeGoalRun(core.EvUsage{Usage: provider.Usage{InputTokens: 40, OutputTokens: 60}})
			if got := interactive.finishGoalRun(); got != test.wantNext {
				t.Fatalf("finish goal run = %t, want %t", got, test.wantNext)
			}
			got := current()
			if got.Status != test.wantStatus || got.TokensUsed != 100 {
				t.Fatalf("goal = %#v", got)
			}
		})
	}
}

func TestGoalRuntimeLimitsPersistedExhaustedBudgetBeforeRun(t *testing.T) {
	budget := uint64(100)
	var mu sync.Mutex
	goal := &core.SessionGoal{ID: "goal-1", Objective: "finish the work", Status: core.GoalActive, TokenBudget: &budget, TokensUsed: budget}
	current := func() *core.SessionGoal {
		mu.Lock()
		defer mu.Unlock()
		return cloneSessionGoal(goal)
	}
	persist := func(next *core.SessionGoal) error {
		mu.Lock()
		goal = cloneSessionGoal(next)
		mu.Unlock()
		return nil
	}
	interactive := NewInteractive(InteractiveConfig{CurrentGoal: current, PersistGoal: persist})
	if !interactive.limitGoalBeforeRun(current()) {
		t.Fatal("exhausted budget was not limited before starting a new provider turn")
	}
	if got := current(); got.Status != core.GoalBudgetLimited || got.ContinuationID != "" {
		t.Fatalf("limited goal = %#v", got)
	}
}

func TestGoalRuntimeRejectsStaleRun(t *testing.T) {
	var mu sync.Mutex
	goal := &core.SessionGoal{ID: "goal-1", Objective: "first goal", Status: core.GoalActive}
	current := func() *core.SessionGoal {
		mu.Lock()
		defer mu.Unlock()
		return cloneSessionGoal(goal)
	}
	persist := func(next *core.SessionGoal) error {
		mu.Lock()
		goal = cloneSessionGoal(next)
		mu.Unlock()
		return nil
	}
	interactive := NewInteractive(InteractiveConfig{CurrentGoal: current, PersistGoal: persist, PersistGoalRuntime: persist})
	run, err := interactive.startGoalRun(current())
	if err != nil || run == nil {
		t.Fatalf("start goal run = (%v, %v)", run, err)
	}
	if err := persist(&core.SessionGoal{ID: "goal-2", Objective: "new goal", Status: core.GoalActive}); err != nil {
		t.Fatal(err)
	}
	if interactive.finishGoalRun() {
		t.Fatal("stale run scheduled a continuation")
	}
	if got := current(); got.ID != "goal-2" || got.ContinuationID != "" || got.TokensUsed != 0 {
		t.Fatalf("stale run changed current goal: %#v", got)
	}
}

func TestGoalRuntimeRecoversAbandonedLease(t *testing.T) {
	var mu sync.Mutex
	goal := &core.SessionGoal{ID: "goal-1", Objective: "finish the work", Status: core.GoalActive, ContinuationID: "abandoned"}
	current := func() *core.SessionGoal {
		mu.Lock()
		defer mu.Unlock()
		return cloneSessionGoal(goal)
	}
	persist := func(next *core.SessionGoal) error {
		mu.Lock()
		goal = cloneSessionGoal(next)
		mu.Unlock()
		return nil
	}
	_ = NewInteractive(InteractiveConfig{CurrentGoal: current, PersistGoalRuntime: persist})
	if got := current(); got.ContinuationID != "" {
		t.Fatalf("abandoned lease was not cleared: %#v", got)
	}
}

func uint64ptr(value uint64) *uint64 { return &value }
