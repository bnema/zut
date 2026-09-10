package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestResidentChildRunnerDefaultsToFiniteMaxSteps(t *testing.T) {
	for _, tc := range []struct {
		name     string
		maxSteps int
		want     int
	}{
		{name: "unlimited parent defaults to bounded child", maxSteps: 0, want: 50},
		{name: "explicit parent limit preserved", maxSteps: 30, want: 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("content-type", "text/event-stream")
				writeOpenAIChunk(t, w, map[string]any{
					"choices": []any{map[string]any{
						"index": 0,
						"delta": map[string]any{"tool_calls": []any{map[string]any{
							"index": 0, "id": "call-loop", "type": "function",
							"function": map[string]string{"name": "read", "arguments": `{"path":"x"}`},
						}}},
						"finish_reason": "tool_calls",
					}},
					"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 1},
				})
			}))
			defer server.Close()

			spec := subagents.ResidentChildSpec{
				ID: "maxsteps-child", SessionID: "maxsteps-session", Provider: "openai", Model: "gpt-4o",
				Tools: []string{"read"},
			}
			journal, err := subagents.OpenResidentJournal(t.TempDir(), spec.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = journal.Close() })
			if err := journal.Accept(spec, "review"); err != nil {
				t.Fatal(err)
			}
			runner, err := newResidentChildRunner(Args{
				Provider: "openai", Model: "gpt-4o", APIKey: "synthetic", BaseURL: server.URL,
				MaxSteps: tc.maxSteps, CWD: t.TempDir(), NoContextFiles: true, NoSkill: true,
			}, spec, journal)
			if err != nil {
				t.Fatal(err)
			}
			err = runner(t.Context(), "review")
			if !errors.Is(err, core.ErrMaxSteps) {
				t.Fatalf("runner error = %v, want step-limit stop", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%d", tc.want)) {
				t.Fatalf("runner error = %v, want limit %d", err, tc.want)
			}
			if got := requests.Load(); got != int32(tc.want) {
				t.Fatalf("provider requests = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestResidentChildRunnerInjectsContextReminderIntoNextRequest(t *testing.T) {
	var firstUsage atomic.Int64
	var captured []string
	var mu sync.Mutex
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		number := requests.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		encoded, _ := json.Marshal(body["messages"])
		mu.Lock()
		captured = append(captured, string(encoded))
		mu.Unlock()
		w.Header().Set("content-type", "text/event-stream")
		usage := map[string]int{"prompt_tokens": 10, "completion_tokens": 1}
		text := "second result"
		if number == 1 {
			text = "first result"
			usage = map[string]int{"prompt_tokens": int(firstUsage.Load()), "completion_tokens": 1}
		}
		writeOpenAIChunk(t, w, map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": text}, "finish_reason": "stop"}},
			"usage":   usage,
		})
	}))
	defer server.Close()

	args := Args{Provider: "openai", Model: "gpt-4o", APIKey: "synthetic", BaseURL: server.URL, CWD: t.TempDir(), NoContextFiles: true, NoSkill: true}
	resolved, err := Resolve(args, true)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ContextWindow <= 0 {
		t.Fatalf("resolved context window = %d, want positive", resolved.ContextWindow)
	}
	// Cross the 85% threshold on the first turn.
	firstUsage.Store(int64(resolved.ContextWindow * 86 / 100))
	spec := subagents.ResidentChildSpec{
		ID: "nudge-child", SessionID: "nudge-session", Provider: "openai", Model: "gpt-4o",
		Tools: []string{"read"},
	}
	journal, err := subagents.OpenResidentJournal(t.TempDir(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	if err := journal.Accept(spec, "review"); err != nil {
		t.Fatal(err)
	}
	runner, err := newResidentChildRunner(args, spec, journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner(t.Context(), "review"); err != nil {
		t.Fatal(err)
	}
	if err := runner(t.Context(), "follow up"); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("provider requests = %d, want 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("captured requests = %d, want 2", len(captured))
	}
	if strings.Contains(captured[0], "Context usage is at") {
		t.Fatalf("first request carries a reminder before any usage: %s", captured[0])
	}
	want := fmt.Sprintf("Context usage is at %d%%", firstUsage.Load()*100/int64(resolved.ContextWindow))
	if !strings.Contains(captured[1], want) {
		t.Fatalf("second request missing %q: %s", want, captured[1])
	}
	// The reminder travels as developer context in the live request; the
	// durable transcript keeps only finalized history.
	messages, err := subagents.ReadResidentTranscriptMessages(journal.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, message := range messages {
		for _, content := range message.Content {
			if block, ok := content.(provider.TextBlock); ok {
				texts = append(texts, block.Text)
			}
		}
	}
	joined := strings.Join(texts, "\n")
	for _, want := range []string{"first result", "second result"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("journal history missing %q: %q", want, joined)
		}
	}
	if strings.Contains(joined, "Context usage is at") {
		t.Fatalf("journal transcript carries the reminder: %q", joined)
	}
}

func TestResidentChildRegistryUsesExactToolListAndForbidsDelegation(t *testing.T) {
	catalogue := core.Registry{
		"read":           nil,
		"bash":           nil,
		"subagent_spawn": nil,
		"update_goal":    nil,
	}
	registry, err := residentChildRegistry(catalogue, []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	if len(registry) != 1 {
		t.Fatalf("registry = %#v", registry)
	}
	for _, name := range []string{"subagent_spawn", "update_goal", "missing"} {
		_, err := residentChildRegistry(catalogue, []string{name})
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("residentChildRegistry(%q) error = %v", name, err)
		}
	}
}

func TestConfigureResidentContextNudgeRemindsOnceAtThreshold(t *testing.T) {
	agent := core.NewAgent(nil, "model", "system", core.Registry{"read": nil})
	configureResidentContextNudge(agent, 1_000)

	check := func(usage provider.Usage, wantReminder bool) string {
		t.Helper()
		agent.SeedLastTurnUsage(usage)
		allowed, reason, contextText := agent.BeforeTurnContext(t.Context(), 1)
		if !allowed || reason != "" {
			t.Fatalf("nudge blocked turn: allowed=%t reason=%q", allowed, reason)
		}
		if wantReminder && !strings.Contains(contextText, "Context usage is at ") {
			t.Fatalf("missing reminder for usage %#v: %q", usage, contextText)
		}
		if !wantReminder && contextText != "" {
			t.Fatalf("unexpected reminder for usage %#v: %q", usage, contextText)
		}
		return contextText
	}

	// Below the threshold: silent, including cache-heavy usage.
	check(provider.Usage{InputTokens: 849}, false)
	check(provider.Usage{InputTokens: 400, CacheReadTokens: 400, CacheWriteTokens: 49}, false)
	// First crossing: exactly one reminder carrying the computed percent.
	first := check(provider.Usage{InputTokens: 850}, true)
	if !strings.Contains(first, "85%") {
		t.Fatalf("reminder percent = %q, want 85%%", first)
	}
	// Later turns stay silent even as usage grows.
	check(provider.Usage{InputTokens: 970}, false)
}

func TestConfigureResidentContextNudgeHandlesDirectJumpAndUnknownWindow(t *testing.T) {
	agent := core.NewAgent(nil, "model", "system", core.Registry{"read": nil})
	configureResidentContextNudge(agent, 1_000)
	agent.SeedLastTurnUsage(provider.Usage{InputTokens: 970})
	allowed, _, contextText := agent.BeforeTurnContext(t.Context(), 1)
	if !allowed || !strings.Contains(contextText, "97%") {
		t.Fatalf("jump reminder = allowed=%t context=%q, want one 97%% reminder", allowed, contextText)
	}

	unknown := core.NewAgent(nil, "model", "system", core.Registry{"read": nil})
	configureResidentContextNudge(unknown, 0)
	unknown.SeedLastTurnUsage(provider.Usage{InputTokens: 970})
	if allowed, reason, contextText := unknown.BeforeTurnContext(t.Context(), 1); !allowed || reason != "" || contextText != "" {
		t.Fatalf("unknown window context = allowed=%t reason=%q context=%q, want silent", allowed, reason, contextText)
	}
}

func TestResidentChildRunnerKeepsToolsAcrossTurns(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber := requests.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("content-type", "text/event-stream")
		if requestNumber == 1 {
			if got := requestMaxTokens(t, body); got != 16_384 {
				t.Errorf("initial max_tokens = %d, want 16384", got)
			}
			writeOpenAIChunk(t, w, map[string]any{
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index": 0, "id": "call-1", "type": "function",
						"function": map[string]string{"name": "read", "arguments": `{"path":"x"}`},
					}}},
					"finish_reason": "tool_calls",
				}},
				"usage": map[string]int{"prompt_tokens": 900, "completion_tokens": 0},
			})
			return
		}
		if tools, ok := body["tools"].([]any); !ok || len(tools) == 0 {
			t.Errorf("near-limit request tools = %#v, want registered tools", body["tools"])
		}
		if got := requestMaxTokens(t, body); got <= 100 {
			t.Errorf("near-limit max_tokens = %d, want model output limit", got)
		}
		writeOpenAIChunk(t, w, map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "final result"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 0, "completion_tokens": 50},
		})
	}))
	defer server.Close()

	runner, journal := newResidentTestRunner(t, server.URL)
	if err := runner(t.Context(), "review"); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("provider requests = %d, want 2", got)
	}
	if summary := latestResidentAssistantText(t, journal); summary != "final result" {
		t.Fatalf("latest summary = %q", summary)
	}
}

func TestResidentChildRunnerRunsHighUsageFollowUpWithoutBudget(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if got := requestMaxTokens(t, body); got != 16_384 {
			t.Errorf("initial max_tokens = %d, want 16384", got)
		}
		w.Header().Set("content-type", "text/event-stream")
		writeOpenAIChunk(t, w, map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "best available result"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 1_200, "completion_tokens": 0},
		})
	}))
	defer server.Close()

	runner, journal := newResidentTestRunner(t, server.URL)
	if err := runner(t.Context(), "review"); err != nil {
		t.Fatalf("runner error = %v, want terminal result", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want 1", got)
	}
	if summary := latestResidentAssistantText(t, journal); summary != "best available result" {
		t.Fatalf("latest summary = %q", summary)
	}
	// Without a cumulative budget, a high-usage follow-up runs normally.
	if err := runner(t.Context(), "continue"); err != nil {
		t.Fatalf("follow-up error = %v, want success", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("provider requests = %d, want 2", got)
	}
}

func TestResidentChildRunnerResumesWithRetainedHistory(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := requests.Add(1)
		w.Header().Set("content-type", "text/event-stream")
		if request == 1 {
			writeOpenAIChunk(t, w, map[string]any{
				"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "first result"}, "finish_reason": "stop"}},
				"usage":   map[string]int{"prompt_tokens": 1200, "completion_tokens": 0},
			})
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		encoded, _ := json.Marshal(body["messages"])
		if !strings.Contains(string(encoded), "first result") {
			t.Errorf("resume lost prior messages: %s", encoded)
		}
		writeOpenAIChunk(t, w, map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "recovered result"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 20, "completion_tokens": 10},
		})
	}))
	defer server.Close()
	cwd := t.TempDir()
	manager := subagents.NewResidentManager(t.TempDir(), func(spec subagents.ResidentChildSpec, journal *subagents.ResidentJournal) (subagents.ResidentTurnRunner, error) {
		return newResidentChildRunner(Args{Provider: "openai", Model: "gpt-4o", APIKey: "synthetic", BaseURL: server.URL, CWD: cwd, NoContextFiles: true, NoSkill: true}, spec, journal)
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	completed := make(chan subagents.ResidentCompletion, 2)
	manager.SetCompletionObserver(func(c subagents.ResidentCompletion) { completed <- c })
	spec := subagents.ResidentChildSpec{ID: "resume-child", SessionID: "session", Provider: "openai", Model: "gpt-4o"}
	if _, err := manager.Spawn(t.Context(), spec, "review"); err != nil {
		t.Fatal(err)
	}
	for turn := 0; turn < 2; turn++ {
		select {
		case completion := <-completed:
			if completion.Err != nil {
				t.Fatal(completion.Err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no completion")
		}
		if turn == 0 {
			if err := manager.Resume(t.Context(), spec.ID, "follow up using previous results"); err != nil {
				t.Fatal(err)
			}
		}
	}
	snapshot, _ := manager.SnapshotFor(spec.ID)
	if requests.Load() != 2 || snapshot.Usage.InputTokens != 1220 {
		t.Fatalf("requests=%d, snapshot=%#v", requests.Load(), snapshot)
	}
}

func newResidentTestRunner(t *testing.T, baseURL string) (subagents.ResidentTurnRunner, *subagents.ResidentJournal) {
	t.Helper()
	spec := subagents.ResidentChildSpec{
		ID: "resident-child", SessionID: "resident-session", Provider: "openai", Model: "gpt-4o",
		Tools: []string{"read"},
	}
	journal, err := subagents.OpenResidentJournal(t.TempDir(), spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	if err := journal.Accept(spec, "review"); err != nil {
		t.Fatal(err)
	}
	runner, err := newResidentChildRunner(Args{
		Provider: "openai", Model: "gpt-4o", APIKey: "synthetic", BaseURL: baseURL,
		CWD: t.TempDir(), NoContextFiles: true, NoSkill: true,
	}, spec, journal)
	if err != nil {
		t.Fatal(err)
	}
	return runner, journal
}

func requestMaxTokens(t *testing.T, body map[string]any) int {
	t.Helper()
	value, ok := body["max_tokens"].(float64)
	if !ok {
		t.Errorf("request max_tokens = %#v", body["max_tokens"])
		return 0
	}
	return int(value)
}

func latestResidentAssistantText(t *testing.T, journal *subagents.ResidentJournal) string {
	t.Helper()
	messages, err := subagents.ReadResidentTranscriptMessages(journal.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != provider.RoleAssistant {
			continue
		}
		var text strings.Builder
		for _, content := range messages[index].Content {
			if block, ok := content.(provider.TextBlock); ok {
				text.WriteString(block.Text)
			}
		}
		return text.String()
	}
	return ""
}

func writeOpenAIChunk(t *testing.T, w http.ResponseWriter, chunk map[string]any) {
	t.Helper()
	data, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
}

func TestResidentChildArgsPreserveDurableProfileInheritance(t *testing.T) {
	no := false
	next := residentChildArgs(Args{Orchestrate: true, NoSkill: false, NoContextFiles: false, BaseURL: "https://stale.example/v1", InsecureTLS: false}, "openai", subagents.ResidentChildSpec{
		Provider: "openai", BaseURL: "https://current.example/v1", InsecureTLS: true, Model: "gpt-5", Workspace: "/repo/child",
		InheritSkills: &no, InheritProjectContext: &no,
	})
	if next.Provider != "openai" || next.BaseURL != "https://current.example/v1" || !next.InsecureTLS || next.Model != "gpt-5" || next.CWD != "/repo/child" || !next.NoSkill || !next.NoContextFiles || next.Orchestrate || !next.ResidentChild {
		t.Fatalf("resident child args = %#v", next)
	}
}

func TestResidentChildArgsDoNotForwardCLIKeyAcrossProviders(t *testing.T) {
	parent := Args{Provider: "openai", APIKey: "parent-key"}
	child := subagents.ResidentChildSpec{Provider: "anthropic", Model: "claude"}
	if next := residentChildArgs(parent, "openai", child); next.APIKey != "" {
		t.Fatalf("cross-provider child inherited API key %q", next.APIKey)
	}
	child.Provider = "openai"
	if next := residentChildArgs(parent, "openai", child); next.APIKey != "parent-key" {
		t.Fatalf("same-provider child API key = %q, want parent key", next.APIKey)
	}
}
