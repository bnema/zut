package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bnema/zut/packages/agent/scheduler"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// TestBotSessionPersistsPlanMutation guards GO-002 for the bot mode: the bot
// session must persist a plan mutation through the tool-result commit path so
// the next resume seeds from the file instead of an empty core state.
func TestBotSessionPersistsPlanMutation(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	ag := core.NewAgent(nil, "model", "", nil)

	sess, _, err := openOrCreateSessionForBot(Args{}, Resolved{Provider: "provider", Model: "model"}, ag, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if ag.CommitToolResult == nil {
		t.Fatal("bot session did not wire CommitToolResult")
	}

	want := []core.PlanStep{{Step: "one", Status: core.PlanPending}}
	if err := ag.CommitToolResult("plan-call", planMutationResult(want)); err != nil {
		t.Fatal(err)
	}
	if sess.Meta.Plan == nil {
		t.Fatal("plan mutation did not write SessionMeta.Plan")
	}

	path := sess.Path
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := core.OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Meta.Plan == nil {
		t.Fatal("reopened bot session lost the persisted plan")
	}
}

// TestScheduledSessionPersistsPlanMutation guards GO-002 for the scheduled mode:
// a scheduled turn that calls plan must persist the mutation to the session it
// reconstructed.
func TestScheduledSessionDisablesRepetitionGuard(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	sess, err := core.NewSession(home, cwd, "openai", "gpt-5", "test")
	if err != nil {
		t.Fatal(err)
	}
	id, path := sess.Meta.ID, sess.Path
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		chunk := map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "done"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 8, "completion_tokens": 2},
		}
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	}))
	defer server.Close()

	args := Args{Provider: "openai", Model: "gpt-5", BaseURL: server.URL, APIKey: "test-key"}
	base, err := Resolve(args, true)
	if err != nil {
		t.Fatal(err)
	}
	managedPath, err := core.ImportSession(path, home, cwd, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := runScheduledSession(context.Background(), scheduler.Task{ID: "task-guard", SessionID: id, Message: "scheduled task"}, args, base, nil, nil); err != nil {
		t.Fatal(err)
	}
	reopened, messages, err := core.OpenSession(managedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, message := range messages {
		if message.Meta[core.RepetitionGuardMetaKey] == "true" {
			t.Fatalf("scheduled run unexpectedly injected repetition warning: %#v", message)
		}
	}
}

func TestScheduledSessionPersistsPlanMutation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	server := scheduledPlanProvider(t)
	defer server.Close()

	sess, err := core.NewSession(home, t.TempDir(), "openai", "gpt-5", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "scheduled task"}},
	}); err != nil {
		t.Fatal(err)
	}
	id := sess.Meta.ID
	path := sess.Path
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	args := Args{Provider: "openai", Model: "gpt-5", BaseURL: server.URL, APIKey: "test-key"}
	base, err := Resolve(args, true)
	if err != nil {
		t.Fatal(err)
	}
	task := scheduler.Task{ID: "task-1", SessionID: id, Message: "scheduled task"}
	if err := runScheduledSession(context.Background(), task, args, base, nil, nil); err != nil {
		t.Fatal(err)
	}

	reopened, _, err := core.OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Meta.Plan == nil {
		t.Fatal("scheduled session did not persist the plan mutation")
	}
}

// scheduledPlanProvider answers the first request with a plan tool call and the
// follow-up with a final answer, matching the OpenAI streaming shape the
// provider parses.
func scheduledPlanProvider(t *testing.T) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "text/event-stream")
		if calls.Add(1) == 1 {
			arguments := `{"action":"set","steps":[{"step":"one","status":"pending"}]}`
			chunk := map[string]any{
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index":    0,
						"id":       "plan-call-1",
						"type":     "function",
						"function": map[string]string{"name": "plan", "arguments": arguments},
					}}},
					"finish_reason": "tool_calls",
				}},
				"usage": map[string]int{"prompt_tokens": 8, "completion_tokens": 2},
			}
			data, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
			return
		}
		chunk := map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "done"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 8, "completion_tokens": 2},
		}
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	}))
}
