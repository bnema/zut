package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

type repetitionTestClient struct {
	calls         atomic.Int32
	assistantOnly bool
}

func (c *repetitionTestClient) Name() string { return "repetition-test" }

func (c *repetitionTestClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	call := c.calls.Add(1)
	id := fmt.Sprintf("call-%d", call)
	out := make(chan provider.Event, 4)
	out <- provider.EventStart{Provider: c.Name(), Model: req.Model}
	if c.assistantOnly {
		out <- provider.EventTextDelta{Delta: "I am still checking."}
		out <- provider.EventDone{Stop: provider.StopContinue, Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.TextBlock{Text: "I am still checking."}},
		}}
	} else {
		out <- provider.EventToolStart{ID: id, Name: "repeat"}
		out <- provider.EventToolEnd{ID: id}
		out <- provider.EventDone{Stop: provider.StopToolUse, Message: provider.Message{
			Role: provider.RoleAssistant,
			Content: []provider.Content{provider.ToolCallBlock{
				ID: id, Name: "repeat", Arguments: json.RawMessage(`{"b":2,"a":1}`),
			}},
		}}
	}
	close(out)
	return out, nil
}

type repetitionTestTool struct {
	calls  atomic.Int32
	differ bool
}

func (*repetitionTestTool) Name() string            { return "repeat" }
func (*repetitionTestTool) Description() string     { return "repeats a test operation" }
func (*repetitionTestTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *repetitionTestTool) Execute(context.Context, json.RawMessage, func(string)) (ToolResult, error) {
	call := t.calls.Add(1)
	result := "unchanged"
	if t.differ {
		result = fmt.Sprintf("result %d", call)
	}
	return ToolResult{Content: []provider.Content{provider.TextBlock{Text: result}}}, nil
}

func TestAgentStopsAfterWarningForRepeatedToolCallAndResult(t *testing.T) {
	client := &repetitionTestClient{}
	tool := &repetitionTestTool{}
	agent := NewAgent(client, "model", "system", Registry{"repeat": tool})
	var warnings, stops int
	if err := agent.Prompt(context.Background(), "inspect the project", nil, func(event AgentEvent) {
		guard, ok := event.(EvRepetitionGuard)
		if !ok {
			return
		}
		switch guard.Stage {
		case RepetitionGuardWarning:
			warnings++
			if guard.Count != repetitionWarnThreshold {
				t.Errorf("warning count = %d, want %d", guard.Count, repetitionWarnThreshold)
			}
		case RepetitionGuardStopped:
			stops++
			if guard.Count != repetitionStopThreshold {
				t.Errorf("stop count = %d, want %d", guard.Count, repetitionStopThreshold)
			}
		}
	}); !errors.Is(err, ErrRepetitiveLoop) {
		t.Fatalf("Prompt error = %v, want ErrRepetitiveLoop", err)
	}
	if got := client.calls.Load(); got != int32(repetitionStopThreshold) {
		t.Fatalf("provider calls = %d, want %d", got, repetitionStopThreshold)
	}
	if got := warnings; got != 1 {
		t.Fatalf("warning events = %d, want 1", got)
	}
	if got := stops; got != 1 {
		t.Fatalf("stop events = %d, want 1", got)
	}

	messages := agent.Messages()
	var warningFound bool
	for _, message := range messages {
		if message.Meta[RepetitionGuardMetaKey] == "true" {
			warningFound = true
			if message.Role != provider.RoleUser || extractText(message) == "" {
				t.Fatalf("malformed hidden warning message: %#v", message)
			}
		}
	}
	if !warningFound {
		t.Fatal("warning was not added to the transcript")
	}
	if len(messages) < 3 {
		t.Fatalf("transcript has %d messages, want tool-call history", len(messages))
	}
	if got := messages[len(messages)-1].Role; got != provider.RoleTool {
		t.Fatalf("last transcript role = %q, want paired tool result", got)
	}
	if got := messages[len(messages)-2].Content[0].(provider.ToolCallBlock).Name; got != "repeat" {
		t.Fatalf("repeated tool name = %q, want repeat", got)
	}
	if !strings.Contains(repetitionGuardPromptFromTranscript(messages), "tool \"repeat\"") {
		t.Fatal("warning prompt does not identify the repeated tool")
	}
}

func repetitionGuardPromptFromTranscript(messages []provider.Message) string {
	for _, message := range messages {
		if message.Meta[RepetitionGuardMetaKey] == "true" {
			return extractText(message)
		}
	}
	return ""
}

func TestToolCallResultFingerprintCanonicalizesJSONArgumentKeyOrder(t *testing.T) {
	first := provider.ToolCallBlock{Name: "repeat", Arguments: json.RawMessage(`{"b":2,"a":1}`)}
	second := provider.ToolCallBlock{Name: "repeat", Arguments: json.RawMessage(`{"a":1,"b":2}`)}
	result := provider.ToolResultBlock{Content: []provider.Content{provider.TextBlock{Text: "same"}}}
	if firstFingerprint, secondFingerprint := toolCallResultFingerprint(first, result), toolCallResultFingerprint(second, result); firstFingerprint != secondFingerprint {
		t.Fatalf("fingerprints differ for equivalent JSON args: %q != %q", firstFingerprint, secondFingerprint)
	}
}

func TestAgentDoesNotStopWhenRepeatedToolOutputChanges(t *testing.T) {
	client := &repetitionTestClient{}
	tool := &repetitionTestTool{differ: true}
	agent := NewAgent(client, "model", "system", Registry{"repeat": tool})
	agent.MaxSteps = repetitionStopThreshold + 1
	var guardEvents int
	err := agent.Prompt(context.Background(), "inspect the project", nil, func(event AgentEvent) {
		if _, ok := event.(EvRepetitionGuard); ok {
			guardEvents++
		}
	})
	if errors.Is(err, ErrRepetitiveLoop) {
		t.Fatalf("Prompt error = %v, unexpected loop detection for changing results", err)
	}
	if guardEvents != 0 {
		t.Fatalf("repetition guard events = %d, want 0", guardEvents)
	}
}

func TestAgentCompactionRollbackPreservesRepetitionCount(t *testing.T) {
	client := &repetitionTestClient{}
	agent := NewAgent(client, "model", "system", Registry{})
	agent.DisableRepetitionGuard = true
	for i := 0; i < repetitionWarnThreshold-1; i++ {
		agent.observeRepetition("repeated", RepetitionKindAssistantMessage, "")
	}
	before := agent.repetition.patterns["repeated"].count

	agent.SetMessages([]provider.Message{{Role: provider.RoleUser}})
	agent.RestoreMessages(nil)
	if got := agent.repetition.patterns["repeated"].count; got != before {
		t.Fatalf("repetition count after transcript rollback = %d, want %d", got, before)
	}
}

func TestAgentTranscriptReplacementResetsRepetitionGuard(t *testing.T) {
	client := &repetitionTestClient{}
	agent := NewAgent(client, "model", "system", Registry{})
	agent.DisableRepetitionGuard = true
	for i := 0; i < repetitionStopThreshold; i++ {
		agent.observeRepetition("stopped", RepetitionKindAssistantMessage, "")
	}
	agent.repetition.stopped = true
	agent.repetition.stoppedDecision = repetitionDecision{count: repetitionStopThreshold}

	agent.SetMessages(nil)
	agent.ResetRepetitionGuard()
	agent.DisableRepetitionGuard = false
	agent.MaxSteps = 1
	if err := agent.Continue(context.Background(), nil); !errors.Is(err, ErrMaxSteps) {
		t.Fatalf("Continue after transcript replacement error = %v, want ErrMaxSteps after one request", err)
	}
	if got := client.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestAgentResetRepetitionGuardAllowsContinueAfterStop(t *testing.T) {
	client := &repetitionTestClient{}
	agent := NewAgent(client, "model", "system", Registry{})
	agent.DisableRepetitionGuard = true
	agent.MaxSteps = 1
	for i := 0; i < repetitionStopThreshold; i++ {
		agent.observeRepetition("stopped", RepetitionKindAssistantMessage, "")
	}
	agent.repetition.stopped = true
	agent.repetition.stoppedDecision = repetitionDecision{toolName: "repeat", count: repetitionStopThreshold}
	if agent.repetitionStopped() {
		t.Fatal("repetition guard should be disabled")
	}

	agent.ResetRepetitionGuard()
	agent.DisableRepetitionGuard = false
	if err := agent.Continue(context.Background(), nil); !errors.Is(err, ErrMaxSteps) {
		t.Fatalf("Continue after reset error = %v, want ErrMaxSteps after one request", err)
	}
	if got := client.calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

func TestAgentRepetitionCountSurvivesContinue(t *testing.T) {
	client := &repetitionTestClient{}
	tool := &repetitionTestTool{}
	agent := NewAgent(client, "model", "system", Registry{"repeat": tool})
	agent.MaxSteps = 4
	if err := agent.Prompt(context.Background(), "inspect the project", nil, nil); !errors.Is(err, ErrMaxSteps) {
		t.Fatalf("Prompt error = %v, want ErrMaxSteps after four calls", err)
	}

	var warningCount, stoppedCount int
	err := agent.Continue(context.Background(), func(event AgentEvent) {
		guard, ok := event.(EvRepetitionGuard)
		if !ok {
			return
		}
		if guard.Stage == RepetitionGuardWarning {
			warningCount = guard.Count
		}
		if guard.Stage == RepetitionGuardStopped {
			stoppedCount = guard.Count
		}
	})
	if !errors.Is(err, ErrRepetitiveLoop) {
		t.Fatalf("Continue error = %v, want ErrRepetitiveLoop", err)
	}
	if warningCount != repetitionWarnThreshold || stoppedCount != repetitionStopThreshold {
		t.Fatalf("warning/stop counts = %d/%d, want %d/%d", warningCount, stoppedCount, repetitionWarnThreshold, repetitionStopThreshold)
	}
	if got := client.calls.Load(); got != int32(repetitionStopThreshold) {
		t.Fatalf("provider calls = %d, want %d", got, repetitionStopThreshold)
	}
}

func TestRepetitionGuardWindowEvictsOldPatterns(t *testing.T) {
	agent := NewAgent(nil, "model", "system", nil)
	fingerprint := strings.Repeat("a", 64)
	for range repetitionWarnThreshold {
		decision := agent.observeRepetition(fingerprint, RepetitionKindToolCall, "read")
		if decision.stage == RepetitionGuardWarning {
			break
		}
	}
	for index := 0; index < repetitionWindowSize; index++ {
		agent.observeRepetition(fmt.Sprintf("other-%d", index), RepetitionKindAssistantMessage, "")
	}
	for index := 0; index < repetitionWarnThreshold-1; index++ {
		if decision := agent.observeRepetition(fingerprint, RepetitionKindToolCall, "read"); decision.stage != "" {
			t.Fatalf("repetition %d after eviction reported stage %q", index+1, decision.stage)
		}
	}
	decision := agent.observeRepetition(fingerprint, RepetitionKindToolCall, "read")
	if decision.stage != RepetitionGuardWarning || decision.count != repetitionWarnThreshold {
		t.Fatalf("post-eviction decision = %#v, want warning at %d", decision, repetitionWarnThreshold)
	}
}

func TestAgentDisableRepetitionGuardAllowsPolling(t *testing.T) {
	client := &repetitionTestClient{}
	tool := &repetitionTestTool{}
	agent := NewAgent(client, "model", "system", Registry{"repeat": tool})
	agent.DisableRepetitionGuard = true
	agent.MaxSteps = repetitionStopThreshold + 1
	if err := agent.Prompt(context.Background(), "poll", nil, nil); !errors.Is(err, ErrMaxSteps) {
		t.Fatalf("Prompt error = %v, want MaxSteps with the guard disabled", err)
	}
	if got := client.calls.Load(); got != repetitionStopThreshold+1 {
		t.Fatalf("provider calls = %d, want %d", got, repetitionStopThreshold+1)
	}
}

func TestAgentStopsAfterWarningForRepeatedAssistantMessage(t *testing.T) {
	client := &repetitionTestClient{assistantOnly: true}
	agent := NewAgent(client, "model", "system", Registry{})
	var warnings, stops int
	err := agent.Prompt(context.Background(), "continue the goal", nil, func(event AgentEvent) {
		guard, ok := event.(EvRepetitionGuard)
		if !ok {
			return
		}
		if guard.Kind != RepetitionKindAssistantMessage {
			t.Errorf("guard kind = %q, want repeated assistant message", guard.Kind)
		}
		if guard.Stage == RepetitionGuardWarning {
			warnings++
		}
		if guard.Stage == RepetitionGuardStopped {
			stops++
		}
	})
	if !errors.Is(err, ErrRepetitiveLoop) {
		t.Fatalf("Prompt error = %v, want ErrRepetitiveLoop", err)
	}
	if got := client.calls.Load(); got != int32(repetitionStopThreshold) {
		t.Fatalf("provider calls = %d, want %d", got, repetitionStopThreshold)
	}
	if warnings != 1 || stops != 1 {
		t.Fatalf("warning/stop events = %d/%d, want 1/1", warnings, stops)
	}
}
