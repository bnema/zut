package modes

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agenttools "github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func goalToolRegistry() core.Registry {
	return core.Registry{agenttools.UpdateGoalToolName: &agenttools.UpdateGoalTool{}}
}

type goalContinuationClient struct {
	mu       sync.Mutex
	requests []provider.Request
	onCall   func(call int)
}

func (c *goalContinuationClient) Name() string { return "goal-continuation" }

func (c *goalContinuationClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	call := len(c.requests)
	onCall := c.onCall
	c.mu.Unlock()
	if onCall != nil {
		onCall(call)
	}
	out := make(chan provider.Event, 1)
	out <- provider.EventDone{
		Stop: provider.StopEnd,
		Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.TextBlock{Text: "working"}},
		},
	}
	close(out)
	return out, nil
}

func TestActiveGoalStartsAnotherTurnWhenThreadBecomesIdle(t *testing.T) {
	var mu sync.Mutex
	var goal *core.SessionGoal
	currentGoal := func() *core.SessionGoal {
		mu.Lock()
		defer mu.Unlock()
		return cloneSessionGoal(goal)
	}
	persistGoal := func(next *core.SessionGoal) error {
		mu.Lock()
		goal = cloneSessionGoal(next)
		mu.Unlock()
		return nil
	}
	client := &goalContinuationClient{onCall: func(call int) {
		if call == 2 {
			_ = persistGoal(&core.SessionGoal{Objective: "finish issue 97", Status: core.GoalPaused})
		}
	}}
	turnFlushed := make(chan struct{}, 2)
	interactive := NewInteractive(InteractiveConfig{
		Agent:       core.NewAgent(client, "model", "system", goalToolRegistry()),
		CurrentGoal: currentGoal,
		PersistGoal: persistGoal,
		FlushSession: func() {
			turnFlushed <- struct{}{}
		},
	})
	interactive.runCtx = context.Background()
	interactive.runSlash(context.Background(), "/goal finish issue 97")
	for turn := 1; turn <= 2; turn++ {
		select {
		case <-turnFlushed:
		case <-time.After(2 * time.Second):
			t.Fatalf("turn %d did not finish", turn)
		}
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(client.requests))
	}
	messages := client.requests[1].Messages
	if len(messages) == 0 || messages[len(messages)-1].Meta[goalContinueMetaKey] != "true" {
		t.Fatalf("continuation request tail = %#v", messages)
	}
}

func TestReplacementGoalContinuesAfterStaleTurn(t *testing.T) {
	var mu sync.Mutex
	goal := &core.SessionGoal{Objective: "replacement objective", Status: core.GoalActive}
	currentGoal := func() *core.SessionGoal {
		mu.Lock()
		defer mu.Unlock()
		return cloneSessionGoal(goal)
	}
	persistGoal := func(next *core.SessionGoal) error {
		mu.Lock()
		goal = cloneSessionGoal(next)
		mu.Unlock()
		return nil
	}
	client := &goalContinuationClient{onCall: func(call int) {
		if call == 1 {
			_ = persistGoal(&core.SessionGoal{Objective: "replacement objective", Status: core.GoalPaused})
		}
	}}
	turnFlushed := make(chan struct{}, 1)
	interactive := NewInteractive(InteractiveConfig{
		Agent:       core.NewAgent(client, "model", "system", goalToolRegistry()),
		CurrentGoal: currentGoal,
		PersistGoal: persistGoal,
		FlushSession: func() {
			turnFlushed <- struct{}{}
		},
	})
	interactive.runCtx = context.Background()
	interactive.mu.Lock()
	interactive.busy = true
	interactive.mu.Unlock()
	stale := provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "original objective"}},
		Meta:    map[string]string{goalContinueMetaKey: "true"},
	}

	interactive.startGoalContinuation(context.Background(), stale)
	select {
	case <-turnFlushed:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement goal did not start")
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(client.requests))
	}
	tail := client.requests[0].Messages[len(client.requests[0].Messages)-1]
	if got := userMessageText(tail); !strings.Contains(got, "replacement objective") || strings.Contains(got, "original objective") {
		t.Fatalf("continuation message = %q", got)
	}
}

func TestToolResultPersistenceRunsOutsideTUILock(t *testing.T) {
	var transitionMu sync.RWMutex
	callbackStarted := make(chan struct{})
	allowPersistence := make(chan struct{})
	transitionHeld := make(chan struct{})
	transitionDone := make(chan struct{})
	handled := make(chan struct{})
	agent := core.NewAgent(nil, "model", "system", goalToolRegistry())
	agent.CommitToolResult = func(_ string, _ core.ToolResult) error {
		close(callbackStarted)
		<-allowPersistence
		transitionMu.RLock()
		transitionMu.RUnlock()
		return nil
	}
	interactive := NewInteractive(InteractiveConfig{Agent: agent})

	go func() {
		_ = agent.CommitToolResult("tool", core.ToolResult{})
		interactive.handleEvent(core.EvToolResult{ID: "tool", Result: core.ToolResult{}})
		close(handled)
	}()
	select {
	case <-callbackStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("tool-result callback did not start")
	}
	go func() {
		transitionMu.Lock()
		close(transitionHeld)
		interactive.ApplySessionAgent(core.NewAgent(nil, "model", "system", goalToolRegistry()), "provider", "model")
		transitionMu.Unlock()
		close(transitionDone)
	}()
	select {
	case <-transitionHeld:
	case <-time.After(2 * time.Second):
		t.Fatal("session transition did not acquire its lock")
	}
	select {
	case <-transitionDone:
	case <-time.After(2 * time.Second):
		close(allowPersistence)
		t.Fatal("session transition deadlocked with tool-result persistence")
	}
	close(allowPersistence)
	select {
	case <-handled:
	case <-time.After(2 * time.Second):
		t.Fatal("tool-result callback did not finish")
	}
}

func TestGoalBadgeUsesDurableStatusAfterPersistenceFailure(t *testing.T) {
	goal := &core.SessionGoal{Objective: "finish issue 97", Status: core.GoalActive}
	interactive := NewInteractive(InteractiveConfig{
		Agent:       core.NewAgent(nil, "model", "system", goalToolRegistry()),
		CurrentGoal: func() *core.SessionGoal { return cloneSessionGoal(goal) },
	})
	// Core converts a goal update to an error result when durable persistence
	// fails, so the presentation layer must leave the active badge unchanged.
	result := core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: "tool result state could not be persisted"}},
		IsError: true,
	}

	interactive.handleEvent(core.EvToolResult{ID: "goal", Result: result})
	interactive.mu.Lock()
	defer interactive.mu.Unlock()
	if interactive.goalStatus != core.GoalActive {
		t.Fatalf("goal badge = %q, want durable active status", interactive.goalStatus)
	}
}

func TestGoalSlashCommandStartsAndControlsGoal(t *testing.T) {
	var goal *core.SessionGoal
	interactive := NewInteractive(InteractiveConfig{
		Agent:       core.NewAgent(nil, "model", "system", goalToolRegistry()),
		CurrentGoal: func() *core.SessionGoal { return cloneSessionGoal(goal) },
		PersistGoal: func(next *core.SessionGoal) error {
			goal = cloneSessionGoal(next)
			return nil
		},
	})
	interactive.busy = true // exercise controls without launching a provider turn

	interactive.runSlash(context.Background(), "/goal finish issue 97")
	if goal == nil || goal.Status != core.GoalActive || goal.Objective != "finish issue 97" {
		t.Fatalf("started goal = %#v", goal)
	}
	if interactive.goalStatus != core.GoalActive {
		t.Fatalf("started badge = %q", interactive.goalStatus)
	}

	interactive.runSlash(context.Background(), "/goal pause")
	if goal.Status != core.GoalPaused {
		t.Fatalf("paused goal = %#v", goal)
	}
	if interactive.goalStatus != core.GoalPaused {
		t.Fatalf("paused badge = %q", interactive.goalStatus)
	}
	interactive.runSlash(context.Background(), "/goal resume")
	if goal.Status != core.GoalActive {
		t.Fatalf("resumed goal = %#v", goal)
	}
	if interactive.goalStatus != core.GoalActive {
		t.Fatalf("resumed badge = %q", interactive.goalStatus)
	}
	interactive.runSlash(context.Background(), "/goal clear")
	if goal != nil {
		t.Fatalf("cleared goal = %#v", goal)
	}
	if interactive.goalStatus != "" {
		t.Fatalf("cleared badge = %q", interactive.goalStatus)
	}
	interactive.runSlash(context.Background(), "/goal pause deployment safely")
	if goal == nil || goal.Status != core.GoalActive || goal.Objective != "pause deployment safely" {
		t.Fatalf("multiword control-like objective = %#v", goal)
	}
	goal.Status = core.GoalBlocked
	goal.Reason = "waiting for input"
	interactive.runSlash(context.Background(), "/goal")
	if !strings.Contains(interactive.statusOK, "waiting for input") {
		t.Fatalf("goal status = %q", interactive.statusOK)
	}
}

func TestGoalControlRequiresSessionPersistence(t *testing.T) {
	interactive := NewInteractive(InteractiveConfig{})
	interactive.runSlash(context.Background(), "/goal pause")
	if !strings.Contains(interactive.statusErr, "session persistence is unavailable") {
		t.Fatalf("status error = %q", interactive.statusErr)
	}
}

func TestGoalCommandDoesNotMutateLiveStateWhenPersistenceFails(t *testing.T) {
	goal := &core.SessionGoal{Objective: "finish issue 97", Status: core.GoalActive}
	interactive := NewInteractive(InteractiveConfig{
		Agent:       core.NewAgent(nil, "model", "system", goalToolRegistry()),
		CurrentGoal: func() *core.SessionGoal { return goal },
		PersistGoal: func(*core.SessionGoal) error { return errors.New("disk full") },
	})
	interactive.busy = true

	interactive.runSlash(context.Background(), "/goal pause")
	if goal.Status != core.GoalActive {
		t.Fatalf("live goal status = %q, want active", goal.Status)
	}
	if interactive.goalStatus != core.GoalActive {
		t.Fatalf("goal badge = %q, want active", interactive.goalStatus)
	}
}

func TestGoalSlashCommandRequiresUpdateGoalTool(t *testing.T) {
	var persisted bool
	interactive := NewInteractive(InteractiveConfig{
		Agent:       core.NewAgent(nil, "model", "system", nil),
		CurrentGoal: func() *core.SessionGoal { return nil },
		PersistGoal: func(*core.SessionGoal) error {
			persisted = true
			return nil
		},
	})

	interactive.runSlash(context.Background(), "/goal finish issue 97")
	if persisted {
		t.Fatal("goal was persisted without the update_goal tool")
	}
	if !strings.Contains(interactive.statusErr, "update_goal is unavailable") {
		t.Fatalf("status error = %q", interactive.statusErr)
	}
}

func TestGoalContinuationRequiresActiveIdleGoalAndYieldsToQueuedUser(t *testing.T) {
	goal := &core.SessionGoal{Objective: "finish issue 97", Status: core.GoalActive}
	agent := core.NewAgent(nil, "model", "system", goalToolRegistry())
	interactive := &Interactive{
		agent: agent,
		cfg:   InteractiveConfig{CurrentGoal: func() *core.SessionGoal { return cloneSessionGoal(goal) }},
	}

	message, ok := interactive.goalContinuationIfIdle()
	if !ok {
		t.Fatal("active idle goal did not request continuation")
	}
	if message.Meta[goalContinueMetaKey] != "true" || message.Role != provider.RoleUser {
		t.Fatalf("continuation message = %#v", message)
	}
	if !isHiddenTranscriptMessage(message) {
		t.Fatal("goal continuation was rendered as a user message")
	}

	agent.QueueMessage("user follow-up")
	if _, ok := interactive.goalContinuationIfIdle(); ok {
		t.Fatal("goal continuation bypassed queued user work")
	}
	agent.DrainQueuedMessages()
	goal.Status = core.GoalPaused
	if _, ok := interactive.goalContinuationIfIdle(); ok {
		t.Fatal("paused goal requested continuation")
	}
}

func cloneSessionGoal(goal *core.SessionGoal) *core.SessionGoal {
	if goal == nil {
		return nil
	}
	clone := *goal
	return &clone
}
