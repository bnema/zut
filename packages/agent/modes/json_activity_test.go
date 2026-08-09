package modes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestRunJSONCompletionUserMessageIsValidJSONL(t *testing.T) {
	message := core.EvUserMessage{Message: provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "[auto-subagents update] worker finished"}},
	}}
	var line bytes.Buffer
	if err := json.NewEncoder(&line).Encode(EventToJSON(message)); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(line.Bytes()), &decoded); err != nil {
		t.Fatalf("completion event is not JSON: %v", err)
	}
	if decoded["type"] != "user_message" {
		t.Fatalf("event type = %#v, want user_message", decoded["type"])
	}
}

type failingJSONWriter struct{ err error }

func (w failingJSONWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRunJSONPropagatesOutputErrors(t *testing.T) {
	providerErr := errors.New("provider unavailable")
	writerErr := errors.New("stdout unavailable")
	ag := core.NewAgent(&streamTestClient{err: providerErr}, "test-model", "", nil)
	_, err := RunJSONWithContextRecovery(context.Background(), ag, "hello", nil, failingJSONWriter{err: writerErr}, nil)
	if !errors.Is(err, providerErr) || !errors.Is(err, writerErr) {
		t.Fatalf("RunJSONWithContextRecovery error = %v, want provider and output errors", err)
	}
}

func TestRunJSONFailureEmitsOneErrorObject(t *testing.T) {
	client := &streamTestClient{err: errors.New("provider unavailable")}
	ag := core.NewAgent(client, "test-model", "", nil)
	var out bytes.Buffer
	_, err := RunJSONWithContextRecovery(context.Background(), ag, "hello", nil, &out, nil)
	if err == nil {
		t.Fatal("RunJSONWithContextRecovery unexpectedly succeeded")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	errorObjects := 0
	for _, line := range lines {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("invalid JSONL line %q: %v", line, err)
		}
		if decoded["type"] == "error" {
			errorObjects++
		}
	}
	if errorObjects != 1 {
		t.Fatalf("error objects = %d, want one; output=%q", errorObjects, out.String())
	}
}

func TestEventToJSONActivityLifecycle(t *testing.T) {
	request := EventToJSON(core.EvRequestStarted{
		Provider:    "anthropic",
		Model:       "claude-test",
		Scope:       core.RetryScopeProvider,
		Attempt:     2,
		MaxAttempts: 3,
	})
	if got, want := request["type"], "request_started"; got != want {
		t.Fatalf("type = %q, want %q", got, want)
	}
	for key, want := range map[string]any{
		"provider":     "anthropic",
		"model":        "claude-test",
		"scope":        "provider",
		"attempt":      2,
		"max_attempts": 3,
	} {
		if got := request[key]; got != want {
			t.Errorf("request[%q] = %#v, want %#v", key, got, want)
		}
	}

	retry := EventToJSON(core.EvRetryScheduled{
		Scope: core.RetryScopeAgent, Attempt: 2, MaxAttempts: 4, Delay: 1500 * time.Millisecond,
	})
	if got, want := retry["delay_ms"], int64(1500); got != want {
		t.Fatalf("delay_ms = %#v, want %#v", got, want)
	}
	if _, exists := retry["delay"]; exists {
		t.Fatal("retry event must not serialize a duration")
	}

	tool := EventToJSON(core.EvToolExecutionStarted{ID: "call-1", Name: "read"})
	if got, want := tool["type"], "tool_execution_started"; got != want {
		t.Fatalf("type = %q, want %q", got, want)
	}
	if tool["id"] != "call-1" || tool["name"] != "read" {
		t.Fatalf("tool event = %#v", tool)
	}
}
