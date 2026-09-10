package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

// turnRecoveryScriptClient replays a scripted sequence of provider turns,
// one per Stream call. A nil-content entry emits an assistant message with
// no content blocks; unknownStop entries exercise non-end stops.
type turnRecoveryScriptClient struct {
	mu    sync.Mutex
	calls int32
	turns []turnRecoveryTurn
}

type turnRecoveryTurn struct {
	content []provider.Content
	stop    provider.StopReason
	err     error
}

func (c *turnRecoveryScriptClient) Name() string { return "turn-recovery-script" }

func (c *turnRecoveryScriptClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	call := int(atomic.AddInt32(&c.calls, 1)) - 1
	turn := turnRecoveryTurn{stop: provider.StopEnd}
	if call < len(c.turns) {
		turn = c.turns[call]
	}
	out := make(chan provider.Event, 8)
	out <- provider.EventStart{Provider: c.Name(), Model: req.Model}
	out <- provider.EventUsage{Usage: provider.Usage{InputTokens: 1, OutputTokens: 1}}
	msg := provider.Message{Role: provider.RoleAssistant, Content: turn.content, Time: time.Now()}
	out <- provider.EventDone{Stop: turn.stop, Err: turn.err, Message: msg}
	close(out)
	return out, nil
}

func (c *turnRecoveryScriptClient) Calls() int32 { return atomic.LoadInt32(&c.calls) }

func textContent(text, phase string) provider.Content {
	return provider.TextBlock{Text: text, Phase: phase}
}

func recoveryEvents(sink *[]AgentEvent) func(AgentEvent) {
	return func(ev AgentEvent) {
		*sink = append(*sink, ev)
	}
}

func countEvents(events []AgentEvent, want func(AgentEvent) bool) int {
	n := 0
	for _, ev := range events {
		if want(ev) {
			n++
		}
	}
	return n
}

func isEvDone(ev AgentEvent) bool { _, ok := ev.(EvDone); return ok }

func isTurnEnd(stop provider.StopReason) func(AgentEvent) bool {
	return func(ev AgentEvent) bool {
		te, ok := ev.(EvTurnEnd)
		return ok && te.Stop == stop && te.Err == nil
	}
}

func lastAssistantMessage(t *testing.T, agent *Agent) provider.Message {
	t.Helper()
	msgs := agent.Messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == provider.RoleAssistant {
			return msgs[i]
		}
	}
	t.Fatal("no assistant message in transcript")
	return provider.Message{}
}

func recoverySynthetic(t *testing.T, agent *Agent) provider.Message {
	t.Helper()
	for _, m := range agent.Messages() {
		if m.Role == provider.RoleUser && m.Meta[turnRecoveryMetaKey] == "true" {
			return m
		}
	}
	t.Fatal("no recovery synthetic user message in transcript")
	return provider.Message{}
}

func runPrompt(t *testing.T, agent *Agent, events *[]AgentEvent) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return agent.Prompt(ctx, "start", nil, recoveryEvents(events))
}

// TestTurnRecoveryEmptyFinalAfterTool: a tool turn followed by an empty
// normal terminal response triggers one recovery and can then finish.
func TestTurnRecoveryEmptyFinalAfterTool(t *testing.T) {
	client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
		{content: []provider.Content{provider.ToolCallBlock{ID: "c1", Name: "result", Arguments: json.RawMessage(`{}`)}}, stop: provider.StopToolUse},
		{content: nil, stop: provider.StopEnd},
		{content: []provider.Content{textContent("done", "")}, stop: provider.StopEnd},
	}}
	agent := NewAgent(client, "test-model", "system", Registry{
		"result": &resultTool{result: ToolResult{Content: []provider.Content{provider.TextBlock{Text: "ok"}}}},
	})
	var events []AgentEvent
	if err := runPrompt(t, agent, &events); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := client.Calls(); got != 3 {
		t.Fatalf("provider calls = %d, want 3", got)
	}
	if got := countEvents(events, func(ev AgentEvent) bool {
		r, ok := ev.(EvTurnRecovery)
		return ok && r.Reason == TurnRecoveryReasonMissingAnswer && r.Attempt == 1
	}); got != 1 {
		t.Fatalf("EvTurnRecovery count = %d, want 1 missing_answer", got)
	}
	if got := lastAssistantMessage(t, agent).Content[0].(provider.TextBlock).Text; got != "done" {
		t.Fatalf("final text = %q, want done", got)
	}
	synth := recoverySynthetic(t, agent)
	if !strings.Contains(synth.Content[0].(provider.TextBlock).Text, "without a final answer") {
		t.Fatalf("recovery text = %q", synth.Content[0].(provider.TextBlock).Text)
	}
	// Tool-result pairing survives the recovery round trip.
	var toolResults int
	for _, m := range agent.Messages() {
		if m.Role == provider.RoleTool {
			toolResults++
		}
	}
	if toolResults != 1 {
		t.Fatalf("tool messages = %d, want 1", toolResults)
	}
}

// TestTurnRecoveryCommentaryOnly: explicitly commentary-only normal
// completion triggers one recovery; the commentary reason is reported.
func TestTurnRecoveryCommentaryOnly(t *testing.T) {
	client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
		{content: []provider.Content{textContent("working on it", provider.TextPhaseCommentary)}, stop: provider.StopEnd},
		{content: []provider.Content{textContent("result", "")}, stop: provider.StopEnd},
	}}
	agent := NewAgent(client, "test-model", "system", Registry{})
	var events []AgentEvent
	if err := runPrompt(t, agent, &events); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := client.Calls(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	if got := countEvents(events, func(ev AgentEvent) bool {
		r, ok := ev.(EvTurnRecovery)
		return ok && r.Reason == TurnRecoveryReasonCommentaryOnly && r.Attempt == 1
	}); got != 1 {
		t.Fatalf("commentary_only EvTurnRecovery count = %d, want 1", got)
	}
	if got := countEvents(events, isEvDone); got != 1 {
		t.Fatalf("EvDone count = %d, want exactly 1", got)
	}
}

// TestTurnRecoveryFinalAnswerNoRecovery covers the no-recovery cases: a
// final answer, an unknown/absent phase, image output, and commentary-
// then-final likewise complete without extra requests.
func TestTurnRecoveryFinalAnswerNoRecovery(t *testing.T) {
	cases := map[string]turnRecoveryTurn{
		"final answer":        {content: []provider.Content{textContent("here is the result", provider.TextPhaseFinal)}},
		"unknown phase":       {content: []provider.Content{textContent("answer", "draft")}},
		"absent phase":        {content: []provider.Content{textContent("answer", "")}},
		"refusal":             {content: []provider.Content{textContent("I cannot do that.", "")}},
		"clarification":       {content: []provider.Content{textContent("Which file do you mean?", "")}},
		"image output":        {content: []provider.Content{provider.ImageBlock{MimeType: "image/png", Data: []byte{1, 2, 3}}}},
		"commentary then fin": {content: []provider.Content{textContent("progress", provider.TextPhaseCommentary), textContent("result", provider.TextPhaseFinal)}},
		"commentary then pla": {content: []provider.Content{textContent("progress", provider.TextPhaseCommentary), textContent("result", "")}},
	}
	for name, turn := range cases {
		t.Run(name, func(t *testing.T) {
			turn.stop = provider.StopEnd
			client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{turn}}
			agent := NewAgent(client, "test-model", "system", Registry{})
			var events []AgentEvent
			if err := runPrompt(t, agent, &events); err != nil {
				t.Fatalf("Prompt returned %v", err)
			}
			if got := client.Calls(); got != 1 {
				t.Fatalf("provider calls = %d, want 1 (no recovery)", got)
			}
			if got := countEvents(events, func(ev AgentEvent) bool {
				_, ok := ev.(EvTurnRecovery)
				return ok
			}); got != 0 {
				t.Fatalf("EvTurnRecovery count = %d, want 0", got)
			}
		})
	}
}

// TestAgentTurnRecoveryReasoningOnlyThenAnswer: a reasoning-only terminal
// message carries no answer and recovers as missing_answer.
func TestAgentTurnRecoveryReasoningOnlyThenAnswer(t *testing.T) {
	client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
		{content: []provider.Content{provider.ReasoningBlock{Summary: "thinking"}}, stop: provider.StopEnd},
		{content: []provider.Content{textContent("result", "")}, stop: provider.StopEnd},
	}}
	agent := NewAgent(client, "test-model", "system", Registry{})
	var events []AgentEvent
	if err := runPrompt(t, agent, &events); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := client.Calls(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	if got := countEvents(events, func(ev AgentEvent) bool {
		r, ok := ev.(EvTurnRecovery)
		return ok && r.Reason == TurnRecoveryReasonMissingAnswer
	}); got != 1 {
		t.Fatalf("missing_answer EvTurnRecovery count = %d, want 1", got)
	}
}

// TestAgentTurnRecoveryWhitespaceThenAnswer: a whitespace-only terminal
// message recovers as missing_answer and can finish on the next turn.
func TestAgentTurnRecoveryWhitespaceThenAnswer(t *testing.T) {
	client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
		{content: []provider.Content{textContent("   \n\t ", "")}, stop: provider.StopEnd},
		{content: []provider.Content{textContent("result", "")}, stop: provider.StopEnd},
	}}
	agent := NewAgent(client, "test-model", "system", Registry{})
	var events []AgentEvent
	if err := runPrompt(t, agent, &events); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := client.Calls(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	if got := countEvents(events, func(ev AgentEvent) bool {
		r, ok := ev.(EvTurnRecovery)
		return ok && r.Reason == TurnRecoveryReasonMissingAnswer
	}); got != 1 {
		t.Fatalf("missing_answer EvTurnRecovery count = %d, want 1", got)
	}
}

// TestAgentTurnRecoveryTwoIncompleteEndings: a second eligible incomplete
// StopEnd returns sentinel ErrIncompleteTurn with exactly-once terminal
// notification and no second model EvTurnEnd for the host decision.
func TestAgentTurnRecoveryTwoIncompleteEndings(t *testing.T) {
	client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
		{content: nil, stop: provider.StopEnd},
		{content: []provider.Content{textContent("still working", provider.TextPhaseCommentary)}, stop: provider.StopEnd},
	}}
	agent := NewAgent(client, "test-model", "system", Registry{})
	var events []AgentEvent
	err := runPrompt(t, agent, &events)
	if !errors.Is(err, ErrIncompleteTurn) {
		t.Fatalf("Prompt error = %v, want ErrIncompleteTurn", err)
	}
	if got := client.Calls(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (bounded)", got)
	}
	if got := countEvents(events, func(ev AgentEvent) bool {
		_, ok := ev.(EvTurnRecovery)
		return ok
	}); got != 1 {
		t.Fatalf("EvTurnRecovery count = %d, want exactly 1", got)
	}
	if got := countEvents(events, isEvDone); got != 1 {
		t.Fatalf("EvDone count = %d, want exactly 1", got)
	}
	if got := countEvents(events, isTurnEnd(provider.StopEnd)); got != 2 {
		t.Fatalf("model StopEnd EvTurnEnd count = %d, want 2 (no second model turn for the host decision)", got)
	}
}

// TestAgentTurnRecoveryToolsBetweenIncomplete: calls stay bounded even when
// tools execute between the two incomplete endings.
func TestAgentTurnRecoveryToolsBetweenIncomplete(t *testing.T) {
	client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
		{content: nil, stop: provider.StopEnd},
		{content: []provider.Content{provider.ToolCallBlock{ID: "c1", Name: "result", Arguments: json.RawMessage(`{}`)}}, stop: provider.StopToolUse},
		{content: nil, stop: provider.StopEnd},
	}}
	agent := NewAgent(client, "test-model", "system", Registry{
		"result": &resultTool{result: ToolResult{Content: []provider.Content{provider.TextBlock{Text: "ok"}}}},
	})
	var events []AgentEvent
	err := runPrompt(t, agent, &events)
	if !errors.Is(err, ErrIncompleteTurn) {
		t.Fatalf("Prompt error = %v, want ErrIncompleteTurn", err)
	}
	if got := client.Calls(); got != 3 {
		t.Fatalf("provider calls = %d, want 3", got)
	}
	if got := countEvents(events, func(ev AgentEvent) bool {
		_, ok := ev.(EvTurnRecovery)
		return ok
	}); got != 1 {
		t.Fatalf("EvTurnRecovery count = %d, want 1 (no reset by tool use)", got)
	}
	if got := countEvents(events, isEvDone); got != 1 {
		t.Fatalf("EvDone count = %d, want 1", got)
	}
}

// TestAgentTurnRecoveryQueuedInputPriority: user input queued during an
// incomplete turn is answered first; recovery must not create an unbounded
// loop for later queued input.
func TestAgentTurnRecoveryQueuedInputPriority(t *testing.T) {
	client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
		{content: nil, stop: provider.StopEnd},
		{content: []provider.Content{textContent("queued answer", "")}, stop: provider.StopEnd},
	}}
	agent := NewAgent(client, "test-model", "system", Registry{})
	var events []AgentEvent
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var once sync.Once
	err := agent.Prompt(ctx, "start", nil, func(ev AgentEvent) {
		events = append(events, ev)
		if te, ok := ev.(EvTurnEnd); ok && te.Stop == provider.StopEnd && te.Err == nil {
			once.Do(func() {
				if !agent.QueueMessage("follow-up", nil) {
					t.Error("QueueMessage returned false")
				}
			})
		}
		if _, ok := ev.(EvTurnRecovery); ok {
			t.Error("recovery must not fire while queued input is pending")
		}
	})
	if err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := client.Calls(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	var sawQueued bool
	for _, m := range agent.Messages() {
		if m.Role == provider.RoleUser && strings.Contains(extractText(m), "follow-up") && m.Meta[turnRecoveryMetaKey] != "true" {
			sawQueued = true
		}
	}
	if !sawQueued {
		t.Fatal("queued input was not delivered as a user message")
	}
}

// TestAgentTurnRecoveryCancellation: no recovery request follows a
// cancellation; the cancellation error surfaces instead.
func TestAgentTurnRecoveryCancellation(t *testing.T) {
	client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
		{content: nil, stop: provider.StopEnd},
	}}
	agent := NewAgent(client, "test-model", "system", Registry{})
	ctx, cancel := context.WithCancel(context.Background())
	var events []AgentEvent
	cancel()
	err := agent.Prompt(ctx, "start", nil, recoveryEvents(&events))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Prompt error = %v, want context.Canceled", err)
	}
	if got := client.Calls(); got != 1 {
		t.Fatalf("provider calls = %d, want 1 (no recovery after cancel)", got)
	}
	if got := countEvents(events, func(ev AgentEvent) bool {
		_, ok := ev.(EvTurnRecovery)
		return ok
	}); got != 0 {
		t.Fatalf("EvTurnRecovery count = %d, want 0", got)
	}
}

// TestAgentTurnRecoveryDeniedToolNoRecovery: a denied tool invocation must
// not gain a recovery prompt urging more actions.
func TestAgentTurnRecoveryDeniedToolNoRecovery(t *testing.T) {
	client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
		{content: []provider.Content{provider.ToolCallBlock{ID: "c1", Name: "result", Arguments: json.RawMessage(`{}`)}}, stop: provider.StopToolUse},
		{content: nil, stop: provider.StopEnd},
	}}
	agent := NewAgent(client, "test-model", "system", Registry{
		"result": &resultTool{result: ToolResult{Content: []provider.Content{provider.TextBlock{Text: "ok"}}}},
	})
	agent.BeforeToolExecute = func(provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return false, "tool call refused by user", nil
	}
	var events []AgentEvent
	if err := runPrompt(t, agent, &events); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := client.Calls(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	if got := countEvents(events, func(ev AgentEvent) bool {
		_, ok := ev.(EvTurnRecovery)
		return ok
	}); got != 0 {
		t.Fatalf("EvTurnRecovery count = %d, want 0 after denial", got)
	}
	var sawRecoverySynthetic bool
	for _, m := range agent.Messages() {
		if m.Meta[turnRecoveryMetaKey] == "true" {
			sawRecoverySynthetic = true
		}
	}
	if sawRecoverySynthetic {
		t.Fatal("recovery synthetic message appended after denial")
	}
}

// TestAgentTurnRecoveryDeniedExecuteNoRecovery: a permission refusal
// returned from tool Execute (not the BeforeToolExecute hook) is also a
// denial. The model sees the refusal text as the tool result, and a later
// incomplete normal stop must not gain a recovery prompt urging more work.
func TestAgentTurnRecoveryDeniedExecuteNoRecovery(t *testing.T) {
	client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
		{content: []provider.Content{provider.ToolCallBlock{ID: "c1", Name: "result", Arguments: json.RawMessage(`{}`)}}, stop: provider.StopToolUse},
		{content: nil, stop: provider.StopEnd},
	}}
	agent := NewAgent(client, "test-model", "system", Registry{
		"result": &resultTool{err: &ToolDeniedError{Reason: "permission denied: bash command \"rm\" is not in allowlist"}},
	})
	var events []AgentEvent
	if err := runPrompt(t, agent, &events); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := client.Calls(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	if got := countEvents(events, func(ev AgentEvent) bool {
		_, ok := ev.(EvTurnRecovery)
		return ok
	}); got != 0 {
		t.Fatalf("EvTurnRecovery count = %d, want 0 after Execute refusal", got)
	}
	for _, m := range agent.Messages() {
		if m.Meta[turnRecoveryMetaKey] == "true" {
			t.Fatal("recovery synthetic message appended after Execute refusal")
		}
	}
	// The refusal text itself must still reach the model as the tool
	// result so it can propose a different action.
	found := false
	for _, m := range agent.Messages() {
		if m.Role != provider.RoleTool {
			continue
		}
		for _, c := range m.Content {
			if tr, ok := c.(provider.ToolResultBlock); ok && tr.IsError {
				for _, inner := range tr.Content {
					if tb, ok := inner.(provider.TextBlock); ok && strings.Contains(tb.Text, "permission denied") {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("refusal text missing from tool result")
	}
}

// TestAgentTurnRecoveryDenialLatchIsSticky: the denial latch is
// intentionally fail-closed for the whole top-level invocation. An early
// unrelated denial suppresses later recovery too, so a silent ending
// after a denial succeeds without urging more actions rather than
// recovering or erroring. This pins the conservative semantic.
func TestAgentTurnRecoveryDenialLatchIsSticky(t *testing.T) {
	client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
		{content: []provider.Content{provider.ToolCallBlock{ID: "c1", Name: "result", Arguments: json.RawMessage(`{}`)}}, stop: provider.StopToolUse},
		{content: nil, stop: provider.StopEnd},
	}}
	agent := NewAgent(client, "test-model", "system", Registry{
		"result": &resultTool{result: ToolResult{Content: []provider.Content{provider.TextBlock{Text: "ok"}}}},
	})
	agent.BeforeToolExecute = func(provider.ToolCallBlock) (bool, string, json.RawMessage) {
		return false, "tool call refused by user", nil
	}
	var events []AgentEvent
	if err := runPrompt(t, agent, &events); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := countEvents(events, func(ev AgentEvent) bool {
		_, ok := ev.(EvTurnRecovery)
		return ok
	}); got != 0 {
		t.Fatalf("EvTurnRecovery count = %d, want 0: early denial suppresses later recovery", got)
	}
}

// TestAgentTurnRecoveryGuardDenial likewise covers the BeforeTurn guard:
// a guard-blocked turn is not a missing answer.
func TestAgentTurnRecoveryGuardDenial(t *testing.T) {
	client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
		{content: []provider.Content{textContent("answer", "")}, stop: provider.StopEnd},
	}}
	agent := NewAgent(client, "test-model", "system", Registry{})
	agent.BeforeTurn = func(step int) (bool, string) {
		if step > 1 {
			return false, "blocked at step 2"
		}
		return true, ""
	}
	var events []AgentEvent
	if err := runPrompt(t, agent, &events); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	if got := client.Calls(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

// TestAgentTurnRecoveryLimitsNoRecovery: output and context limits never
// recover.
func TestAgentTurnRecoveryLimitsNoRecovery(t *testing.T) {
	for _, stop := range []provider.StopReason{provider.StopLength, provider.StopAborted, provider.StopError} {
		t.Run(string(stop), func(t *testing.T) {
			client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
				{content: nil, stop: stop},
			}}
			agent := NewAgent(client, "test-model", "system", Registry{})
			var events []AgentEvent
			if err := runPrompt(t, agent, &events); err != nil {
				t.Fatalf("Prompt returned %v", err)
			}
			if got := client.Calls(); got != 1 {
				t.Fatalf("provider calls = %d, want 1", got)
			}
			if got := countEvents(events, func(ev AgentEvent) bool {
				_, ok := ev.(EvTurnRecovery)
				return ok
			}); got != 0 {
				t.Fatalf("EvTurnRecovery count = %d, want 0 for stop %q", got, stop)
			}
		})
	}
}

// TestAgentTurnRecoveryRespectsMaxSteps: the recovery continuation runs
// inside the existing step budget and does not reset it. With MaxSteps=1
// the recovery is marked but the budget is exhausted, so the existing
// typed limit error surfaces; with MaxSteps=2 two incomplete endings
// still terminate with ErrIncompleteTurn.
func TestAgentTurnRecoveryRespectsMaxSteps(t *testing.T) {
	t.Run("budget exhausted", func(t *testing.T) {
		client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
			{content: nil, stop: provider.StopEnd},
		}}
		agent := NewAgent(client, "test-model", "system", Registry{})
		agent.MaxSteps = 1
		var events []AgentEvent
		err := runPrompt(t, agent, &events)
		if !errors.Is(err, ErrMaxSteps) {
			t.Fatalf("Prompt error = %v, want ErrMaxSteps", err)
		}
		if got := client.Calls(); got != 1 {
			t.Fatalf("provider calls = %d, want 1 (no reset)", got)
		}
	})
	t.Run("two incomplete within budget", func(t *testing.T) {
		client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
			{content: nil, stop: provider.StopEnd},
			{content: nil, stop: provider.StopEnd},
		}}
		agent := NewAgent(client, "test-model", "system", Registry{})
		agent.MaxSteps = 2
		var events []AgentEvent
		err := runPrompt(t, agent, &events)
		if !errors.Is(err, ErrIncompleteTurn) {
			t.Fatalf("Prompt error = %v, want ErrIncompleteTurn", err)
		}
		if got := client.Calls(); got != 2 {
			t.Fatalf("provider calls = %d, want 2", got)
		}
	})
}

// TestAgentTurnRecoverySyntheticPersistence: the recovery message is
// appended through AppendUserContext (firing OnMessageAppended for session
// durability) and replays to the provider on the next request.
func TestAgentTurnRecoverySyntheticPersistence(t *testing.T) {
	client := &turnRecoveryScriptClient{turns: []turnRecoveryTurn{
		{content: []provider.Content{provider.ToolCallBlock{ID: "c1", Name: "result", Arguments: json.RawMessage(`{}`)}}, stop: provider.StopToolUse},
		{content: nil, stop: provider.StopEnd},
		{content: []provider.Content{textContent("done", "")}, stop: provider.StopEnd},
	}}
	agent := NewAgent(client, "test-model", "system", Registry{
		"result": &resultTool{result: ToolResult{Content: []provider.Content{provider.TextBlock{Text: "ok"}}}},
	})
	var appended []provider.Message
	agent.OnMessageAppended = func(m provider.Message) { appended = append(appended, m) }
	var events []AgentEvent
	if err := runPrompt(t, agent, &events); err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
	var sawSynthetic bool
	for _, m := range appended {
		if m.Meta[turnRecoveryMetaKey] == "true" {
			sawSynthetic = true
			if m.Role != provider.RoleUser || len(m.Content) != 1 {
				t.Fatalf("recovery appended message = %#v, want single-block user message", m)
			}
		}
	}
	if !sawSynthetic {
		t.Fatal("OnMessageAppended never fired for the recovery message")
	}
	// The transcript keeps tool-result pairing and the recovery marker in
	// order. An empty terminal message carries no blocks, so oneTurn does
	// not append it: user, assistant tool call, tool result, synthetic,
	// final answer.
	msgs := agent.Messages()
	if len(msgs) != 5 {
		t.Fatalf("transcript messages = %d, want 5", len(msgs))
	}
	if msgs[1].Role != provider.RoleAssistant || msgs[2].Role != provider.RoleTool {
		t.Fatalf("tool pairing broken: roles = %q, %q", msgs[1].Role, msgs[2].Role)
	}
	if msgs[3].Meta[turnRecoveryMetaKey] != "true" {
		t.Fatalf("message 3 meta = %#v, want recovery marker", msgs[3].Meta)
	}
}
