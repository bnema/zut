package modes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// turnRecoveryModeClient replays one assistant message per Stream call so
// mode tests can drive the core recovery allowance without timing sleeps.
type turnRecoveryModeClient struct {
	mu    sync.Mutex
	calls int
	turns [][]provider.Content
}

func (c *turnRecoveryModeClient) Name() string { return "turn-recovery-mode-test" }

func (c *turnRecoveryModeClient) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	c.mu.Lock()
	call := c.calls
	c.calls++
	c.mu.Unlock()
	var content []provider.Content
	if call < len(c.turns) {
		content = c.turns[call]
	}
	out := make(chan provider.Event, 4)
	out <- provider.EventStart{Provider: c.Name(), Model: "test-model"}
	out <- provider.EventUsage{Usage: provider.Usage{InputTokens: 3, OutputTokens: 5}}
	out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
		Role:    provider.RoleAssistant,
		Content: content,
		Time:    time.Now(),
	}}
	close(out)
	return out, nil
}

func (c *turnRecoveryModeClient) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func commentaryContent(text string) []provider.Content {
	return []provider.Content{provider.TextBlock{Text: text, Phase: provider.TextPhaseCommentary}}
}

func answerContent(text string) []provider.Content {
	return []provider.Content{provider.TextBlock{Text: text}}
}

func TestTurnRecoveryModeRendersOneRecoveryStatus(t *testing.T) {
	client := &turnRecoveryModeClient{turns: [][]provider.Content{
		commentaryContent("still working on it"),
		answerContent("finished"),
	}}
	ag := core.NewAgent(client, "test-model", "", nil)
	i := newNotesTestInteractive()
	i.cfg.Agent = ag
	i.agent = ag

	i.handleEvent(core.EvTurnRecovery{Reason: core.TurnRecoveryReasonCommentaryOnly, Attempt: 1})

	if len(i.extNotes) != 1 {
		t.Fatalf("extNotes = %d, want exactly one recovery status", len(i.extNotes))
	}
	if got := stripANSIBytes(i.extNotes[0]); !strings.Contains(got, "Continuing: no final answer received.") {
		t.Fatalf("recovery status = %q, want exact recovery line", got)
	}
}

func TestTurnRecoveryPrintSurfacesRecoveryAndCompletes(t *testing.T) {
	client := &turnRecoveryModeClient{turns: [][]provider.Content{
		commentaryContent("still working on it"),
		answerContent("finished"),
	}}
	ag := core.NewAgent(client, "test-model", "", nil)
	var out bytes.Buffer
	usage, err := RunPrint(context.Background(), ag, "work", nil, &out)
	if err != nil {
		t.Fatalf("RunPrint error = %v", err)
	}
	_ = usage
	if out.String() != "finished\n" {
		t.Fatalf("print output = %q, want recovered final answer", out.String())
	}
	if client.Calls() != 2 {
		t.Fatalf("provider calls = %d, want 2 (one bounded recovery)", client.Calls())
	}
}

func TestTurnRecoveryStreamKeepsTranscriptOnStdout(t *testing.T) {
	client := &turnRecoveryModeClient{turns: [][]provider.Content{
		nil,
		answerContent("finished"),
	}}
	ag := core.NewAgent(client, "test-model", "", nil)
	var out, diag bytes.Buffer
	if err := RunStreamWithDiag(context.Background(), ag, "work", nil, &out, &diag); err != nil {
		t.Fatalf("RunStreamWithDiag error = %v", err)
	}
	if !strings.Contains(out.String(), "finished") {
		t.Fatalf("stdout = %q, want recovered answer text", out.String())
	}
	if !strings.Contains(diag.String(), "Continuing: no final answer received.") {
		t.Fatalf("diag = %q, want one recovery status", diag.String())
	}
	if strings.Contains(out.String(), "Continuing: no final answer received.") {
		t.Fatalf("stdout = %q, recovery status must not pollute the transcript", out.String())
	}
}

func TestTurnRecoveryExhaustionSurfacesIncompleteError(t *testing.T) {
	client := &turnRecoveryModeClient{turns: [][]provider.Content{
		nil,
		nil,
	}}
	ag := core.NewAgent(client, "test-model", "", nil)
	var out bytes.Buffer
	_, err := RunPrint(context.Background(), ag, "work", nil, &out)
	if !errors.Is(err, core.ErrIncompleteTurn) {
		t.Fatalf("RunPrint error = %v, want ErrIncompleteTurn via errors.Is", err)
	}
	if out.String() != "" {
		t.Fatalf("print output = %q, exhaustion must not report success text", out.String())
	}
	if client.Calls() != 2 {
		t.Fatalf("provider calls = %d, want exactly 2 (no unbounded retry)", client.Calls())
	}
}

func TestTurnRecoveryGoalDoesNotScheduleDuplicateWork(t *testing.T) {
	var mu sync.Mutex
	goal := &core.SessionGoal{ID: "goal-1", Status: core.GoalActive, Objective: "finish the work"}
	current := func() *core.SessionGoal {
		mu.Lock()
		defer mu.Unlock()
		cp := *goal
		return &cp
	}
	persist := func(next *core.SessionGoal) error {
		mu.Lock()
		goal = next
		mu.Unlock()
		return nil
	}
	client := &turnRecoveryModeClient{turns: [][]provider.Content{
		commentaryContent("still working on it"),
		answerContent("finished"),
	}}
	ag := core.NewAgent(client, "test-model", "", goalToolRegistry())
	i := NewInteractive(InteractiveConfig{
		Agent:              ag,
		CurrentGoal:        current,
		PersistGoal:        persist,
		PersistGoalRuntime: persist,
	})
	i.runCtx = context.Background()
	run, err := i.startGoalRun(current())
	if err != nil {
		t.Fatalf("startGoalRun error = %v", err)
	}
	if run == nil {
		t.Fatal("startGoalRun did not start a run")
	}
	var events []core.AgentEvent
	if err := ag.Prompt(context.Background(), "work", nil, func(ev core.AgentEvent) {
		events = append(events, ev)
		i.observeGoalRun(ev)
		i.handleEvent(ev)
	}); err != nil {
		t.Fatalf("Prompt error = %v", err)
	}
	recoveries := 0
	for _, ev := range events {
		if _, ok := ev.(core.EvTurnRecovery); ok {
			recoveries++
		}
	}
	if recoveries != 1 {
		t.Fatalf("recovery events = %d, want exactly one inner continuation", recoveries)
	}
	// The inner continuation must not start a second goal run: the run
	// started above is still the single owner, and finishing it once must
	// record accounting without scheduling another continuation here.
	i.mu.Lock()
	owner := i.goalRun
	i.mu.Unlock()
	if owner != run {
		t.Fatal("inner continuation replaced the goal run: want the single owner started above")
	}
	if owner.goalID != "goal-1" {
		t.Fatalf("goal run goalID = %q, want %q", owner.goalID, "goal-1")
	}
	if !i.finishGoalRun(false) {
		t.Fatalf("finishGoalRun = false, want accounting for the single owner run")
	}
	mu.Lock()
	used := goal.TokensUsed
	mu.Unlock()
	if used == 0 {
		t.Fatalf("goal tokens used = 0, want usage recorded once for the single run")
	}
}

func TestTurnRecoveryExhaustionKeepsGoalBlocked(t *testing.T) {
	var mu sync.Mutex
	goal := &core.SessionGoal{ID: "goal-1", Status: core.GoalActive, Objective: "finish the work"}
	current := func() *core.SessionGoal {
		mu.Lock()
		defer mu.Unlock()
		cp := *goal
		return &cp
	}
	var persisted []*core.SessionGoal
	persist := func(next *core.SessionGoal) error {
		mu.Lock()
		cp := *next
		persisted = append(persisted, &cp)
		goal = &cp
		mu.Unlock()
		return nil
	}
	client := &turnRecoveryModeClient{turns: [][]provider.Content{nil, nil}}
	ag := core.NewAgent(client, "test-model", "", goalToolRegistry())
	i := NewInteractive(InteractiveConfig{
		Agent:              ag,
		CurrentGoal:        current,
		PersistGoal:        persist,
		PersistGoalRuntime: persist,
	})
	err := ag.Prompt(context.Background(), "work", nil, func(core.AgentEvent) {})
	if !errors.Is(err, core.ErrIncompleteTurn) {
		t.Fatalf("Prompt error = %v, want ErrIncompleteTurn", err)
	}
	// Exhaustion follows established blocking semantics: the goal is marked
	// blocked with the incomplete-turn cause, never auto-cleared or rerun.
	i.updateActiveGoal(core.GoalBlocked, "turn ended with an error: "+core.ErrIncompleteTurn.Error())
	mu.Lock()
	status := goal.Status
	mu.Unlock()
	if status != core.GoalBlocked {
		t.Fatalf("goal status = %v, want blocked after exhaustion", status)
	}
	if len(persisted) == 0 {
		t.Fatal("goal exhaustion was not persisted")
	}
}

func TestTurnRecoveryEventJSONEncoding(t *testing.T) {
	encoded := EventToJSON(core.EvTurnRecovery{Reason: core.TurnRecoveryReasonMissingAnswer, Attempt: 1})
	raw, err := json.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("turn_recovery event is not JSON: %v", err)
	}
	if decoded["type"] != "turn_recovery" {
		t.Fatalf("type = %#v, want turn_recovery", decoded["type"])
	}
	if decoded["reason"] != core.TurnRecoveryReasonMissingAnswer {
		t.Fatalf("reason = %#v, want missing_answer", decoded["reason"])
	}
	if decoded["attempt"] != float64(1) {
		t.Fatalf("attempt = %#v, want 1", decoded["attempt"])
	}
}
