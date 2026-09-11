package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestResidentChildRunnerWithoutStepLimitRunsPastFormerCap(t *testing.T) {
	server, requests := residentToolLoopServer(t, 51)
	runner, _, _ := newResidentChildTestRunner(t, "unlimited-child", Args{
		Provider: "openai", Model: "gpt-4o", APIKey: "synthetic", BaseURL: server.URL,
		CWD: t.TempDir(), NoContextFiles: true, NoSkill: true,
	})
	if err := runner(t.Context(), "review"); err != nil {
		t.Fatalf("runner error = %v, want success past the former 50-step cap", err)
	}
	if got := requests.Load(); got != 52 {
		t.Fatalf("provider requests = %d, want 51 tool turns and one answer", got)
	}
}

func TestResidentChildRunnerEnforcesExplicitParentStepLimit(t *testing.T) {
	server, requests := residentToolLoopServer(t, 200)
	runner, _, _ := newResidentChildTestRunner(t, "limited-child", Args{
		Provider: "openai", Model: "gpt-4o", APIKey: "synthetic", BaseURL: server.URL,
		MaxSteps: 3, CWD: t.TempDir(), NoContextFiles: true, NoSkill: true,
	})
	err := runner(t.Context(), "review")
	if !errors.Is(err, core.ErrMaxSteps) || !strings.Contains(err.Error(), "3") {
		t.Fatalf("runner error = %v, want step limit 3", err)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("provider requests = %d, want 3", got)
	}
}

// residentToolLoopServer answers the first toolCalls requests with a distinct
// read call and then returns a substantive final answer.
func residentToolLoopServer(t *testing.T, toolCalls int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		number := requests.Add(1)
		w.Header().Set("content-type", "text/event-stream")
		if int(number) <= toolCalls {
			writeOpenAIChunk(t, w, map[string]any{
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index": 0, "id": fmt.Sprintf("call-%d", number), "type": "function",
						"function": map[string]string{"name": "read", "arguments": `{"path":"x"}`},
					}}},
					"finish_reason": "tool_calls",
				}},
				"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 1},
			})
			return
		}
		writeOpenAIChunk(t, w, map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "final result"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 1},
		})
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

// newResidentChildTestRunner builds one journaled resident child runner with
// its own accepted spec.
func newResidentChildTestRunner(t *testing.T, childID string, args Args) (subagents.ResidentTurnRunner, *subagents.ResidentJournal, subagents.ResidentChildSpec) {
	t.Helper()
	spec := subagents.ResidentChildSpec{
		ID: childID, SessionID: childID + "-session", Provider: "openai", Model: "gpt-4o",
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
	return runner, journal, spec
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

func TestConfigureResidentContextNudgeRemindsOncePerBand(t *testing.T) {
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

	// Below the first band: silent, including cache-heavy usage.
	check(provider.Usage{InputTokens: 849}, false)
	check(provider.Usage{InputTokens: 400, CacheReadTokens: 400, CacheWriteTokens: 49}, false)
	// Each band fires once, carrying the computed percent of the crossing turn.
	first := check(provider.Usage{InputTokens: 850}, true)
	if !strings.Contains(first, "85%") {
		t.Fatalf("reminder percent = %q, want 85%%", first)
	}
	check(provider.Usage{InputTokens: 860}, false)
	second := check(provider.Usage{InputTokens: 900}, true)
	if !strings.Contains(second, "90%") {
		t.Fatalf("reminder percent = %q, want 90%%", second)
	}
	check(provider.Usage{InputTokens: 920}, false)
	third := check(provider.Usage{InputTokens: 950}, true)
	if !strings.Contains(third, "95%") {
		t.Fatalf("reminder percent = %q, want 95%%", third)
	}
	// Past the last band the ladder is spent.
	check(provider.Usage{InputTokens: 999}, false)
}

func TestConfigureResidentContextNudgeIgnoresShrinkingUsage(t *testing.T) {
	agent := core.NewAgent(nil, "model", "system", core.Registry{"read": nil})
	configureResidentContextNudge(agent, 1_000)

	check := func(usage provider.Usage, wantReminder bool) string {
		t.Helper()
		agent.SeedLastTurnUsage(usage)
		allowed, reason, contextText := agent.BeforeTurnContext(t.Context(), 1)
		if !allowed || reason != "" {
			t.Fatalf("nudge blocked turn: allowed=%t reason=%q", allowed, reason)
		}
		if wantReminder != (contextText != "") {
			t.Fatalf("reminder for usage %#v = %q, want reminder=%t", usage, contextText, wantReminder)
		}
		return contextText
	}

	// A shrinking prompt never delivers a band it did not cross.
	check(provider.Usage{InputTokens: 700}, false)
	check(provider.Usage{InputTokens: 500}, false)
	jumped := check(provider.Usage{InputTokens: 910}, true)
	if !strings.Contains(jumped, "91%") {
		t.Fatalf("reminder percent = %q, want 91%%", jumped)
	}
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
	runner, journal, _ := newResidentChildTestRunner(t, "resident-child", Args{
		Provider: "openai", Model: "gpt-4o", APIKey: "synthetic", BaseURL: baseURL,
		CWD: t.TempDir(), NoContextFiles: true, NoSkill: true,
	})
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

// residentOverflowScript scripts the provider responses of a child that hits
// the context window: leading tool-call turns, one overflow, one compaction
// summary, then continued tool turns or the final text.
type residentOverflowScript struct {
	toolCallRequests      int
	continuationToolCalls int
	secondOverflow        bool
	finalText             string
	onOverflow            func()
	// toolUsageTokens is the prompt size reported by tool-call responses; zero
	// reports a small prompt.
	toolUsageTokens int
	// summaryUsageTokens, when positive, is the prompt size reported by the
	// compaction summary response.
	summaryUsageTokens int
	// continuationUsageTokens is the prompt size reported by tool turns after a
	// successful compaction; zero reuses toolUsageTokens.
	continuationUsageTokens int

	requests atomic.Int32
	mu       sync.Mutex
	bodies   []string
}

func (s *residentOverflowScript) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		number := int(s.requests.Add(1))
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		encoded, _ := json.Marshal(body["messages"])
		s.mu.Lock()
		s.bodies = append(s.bodies, string(encoded))
		s.mu.Unlock()

		overflowAt := s.toolCallRequests + 1
		switch {
		case number <= s.toolCallRequests:
			w.Header().Set("content-type", "text/event-stream")
			writeOpenAIChunk(t, w, residentToolCallChunk(number, s.toolUsageTokens))
		case number == overflowAt:
			if s.onOverflow != nil {
				s.onOverflow()
			}
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"input exceeds the context window","code":"context_length_exceeded"}}`))
		case number == overflowAt+1:
			w.Header().Set("content-type", "text/event-stream")
			writeOpenAIChunk(t, w, residentSummaryChunk(s.summaryUsageTokens))
		case s.secondOverflow:
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"input exceeds the context window","code":"context_length_exceeded"}}`))
		case number < overflowAt+2+s.continuationToolCalls:
			w.Header().Set("content-type", "text/event-stream")
			continuationUsage := s.continuationUsageTokens
			if continuationUsage <= 0 {
				continuationUsage = s.toolUsageTokens
			}
			writeOpenAIChunk(t, w, residentToolCallChunk(number, continuationUsage))
		default:
			w.Header().Set("content-type", "text/event-stream")
			writeOpenAIChunk(t, w, residentFinalChunk(s.finalText))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func residentToolCallChunk(number, usageTokens int) map[string]any {
	if usageTokens <= 0 {
		usageTokens = 10
	}
	return map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": fmt.Sprintf("call-%d", number), "type": "function",
				"function": map[string]string{"name": "read", "arguments": `{"path":"x"}`},
			}}},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]int{"prompt_tokens": usageTokens, "completion_tokens": 1},
	}
}

func residentSummaryChunk(usageTokens int) map[string]any {
	chunk := map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "compacted digest"}, "finish_reason": "stop"}},
	}
	if usageTokens > 0 {
		chunk["usage"] = map[string]int{"prompt_tokens": usageTokens, "completion_tokens": 2}
	}
	return chunk
}

func residentFinalChunk(text string) map[string]any {
	return map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": text}, "finish_reason": "stop"}},
		"usage":   map[string]int{"prompt_tokens": 12, "completion_tokens": 5},
	}
}

func (s *residentOverflowScript) capturedBodies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

func residentArguments(t *testing.T, baseURL string) Args {
	t.Helper()
	return Args{
		Provider: "openai", Model: "gpt-4o", APIKey: "synthetic", BaseURL: baseURL,
		CWD: t.TempDir(), NoContextFiles: true, NoSkill: true,
	}
}

func countResidentCheckpoints(t *testing.T, journal *subagents.ResidentJournal) int {
	t.Helper()
	records, err := subagents.ReadResidentJournal(filepath.Join(journal.Dir(), "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := 0
	for _, record := range records {
		if record.Type == "child.compacted" {
			checkpoints++
		}
	}
	return checkpoints
}

func TestResidentChildRunnerRecoversFromContextOverflowOnce(t *testing.T) {
	script := &residentOverflowScript{toolCallRequests: 2, finalText: "overflow recovered result"}
	server := script.start(t)
	runner, journal, spec := newResidentChildTestRunner(t, "overflow-child", residentArguments(t, server.URL))
	if err := journal.RecordTurnStarted(spec, "turn-1"); err != nil {
		t.Fatal(err)
	}
	runErr := runner(t.Context(), "review")
	if err := journal.RecordTurnFinished(spec, "turn-1", runErr); err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatalf("runner error = %v, want one recovered turn", runErr)
	}
	if got := script.requests.Load(); got != 5 {
		t.Fatalf("provider requests = %d, want 3 prompt, 1 compaction, and 1 continuation", got)
	}
	result, err := journal.Result()
	if err != nil {
		t.Fatal(err)
	}
	if result.State != subagents.ResidentIdle || result.Summary != "overflow recovered result" {
		t.Fatalf("result = %#v, want the recovered final text", result)
	}
	records, err := subagents.ReadResidentJournal(filepath.Join(journal.Dir(), "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := 0
	for _, record := range records {
		if record.Type != "child.compacted" {
			continue
		}
		checkpoints++
		if record.TurnID != "turn-1" {
			t.Fatalf("checkpoint turn ID = %q, want the active accepted turn", record.TurnID)
		}
	}
	if checkpoints != 1 {
		t.Fatalf("compaction checkpoints = %d, want 1", checkpoints)
	}
	// Resume replay starts from the checkpoint: the digest replaces the
	// pre-overflow tool exchange and the continuation request carried it.
	messages, err := subagents.ReadResidentTranscriptMessages(journal.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || !strings.HasPrefix(residentMessageText(messages[0]), "## Context Summary (compacted)") {
		t.Fatalf("resumed transcript = %#v, want the checkpoint plus the kept tail", messages)
	}
	bodies := script.capturedBodies()
	if len(bodies) != 5 || !strings.Contains(bodies[4], "compacted digest") {
		t.Fatalf("continuation request = %q, want the compacted transcript", bodies[len(bodies)-1])
	}
	if strings.Contains(bodies[4], "call-1") {
		t.Fatalf("continuation request kept the dropped tool exchange: %q", bodies[4])
	}
}

func TestResidentChildRunnerDoesNotRecoverTwicePerTurn(t *testing.T) {
	script := &residentOverflowScript{toolCallRequests: 2, secondOverflow: true, finalText: "unreached"}
	server := script.start(t)
	runner, journal, _ := newResidentChildTestRunner(t, "double-overflow", residentArguments(t, server.URL))
	err := runner(t.Context(), "review")
	if err == nil || !provider.IsContextOverflowError(err) {
		t.Fatalf("runner error = %v, want the second overflow to stay terminal", err)
	}
	if got := script.requests.Load(); got != 5 {
		t.Fatalf("provider requests = %d, want no second compaction", got)
	}
	if checkpoints := countResidentCheckpoints(t, journal); checkpoints != 1 {
		t.Fatalf("compaction checkpoints = %d, want 1", checkpoints)
	}
}

func TestResidentChildRunnerDoesNotReplenishExplicitStepLimit(t *testing.T) {
	script := &residentOverflowScript{toolCallRequests: 3, finalText: "unreached"}
	server := script.start(t)
	args := residentArguments(t, server.URL)
	args.MaxSteps = 4
	runner, journal, _ := newResidentChildTestRunner(t, "limited-overflow", args)
	err := runner(t.Context(), "review")
	if err == nil || !provider.IsContextOverflowError(err) {
		t.Fatalf("runner error = %v, want the overflow", err)
	}
	if got := script.requests.Load(); got != 5 {
		t.Fatalf("provider requests = %d, want the compaction attempt only", got)
	}
	// The checkpoint is still journaled so an explicit resume starts small,
	// but the spent allowance forbids continuing this turn.
	if checkpoints := countResidentCheckpoints(t, journal); checkpoints != 1 {
		t.Fatalf("compaction checkpoints = %d, want 1", checkpoints)
	}
	for _, body := range script.capturedBodies() {
		if strings.Contains(body, "compacted digest") {
			t.Fatalf("recovery continued after the step allowance was spent: %q", body)
		}
	}
}

func TestResidentChildRunnerFailsTurnWhenCheckpointCannotPersist(t *testing.T) {
	root := t.TempDir()
	spec := subagents.ResidentChildSpec{
		ID: "persist-failure", SessionID: "persist-failure-session", Provider: "openai", Model: "gpt-4o",
		Tools: []string{"read"},
	}
	journal, err := subagents.OpenResidentJournal(root, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	if err := journal.Accept(spec, "review"); err != nil {
		t.Fatal(err)
	}
	// The journal is closed at the exact overflow boundary, so the recovery
	// cannot persist its checkpoint.
	script := &residentOverflowScript{
		toolCallRequests: 2,
		finalText:        "unreached",
		onOverflow:       func() { _ = journal.Close() },
	}
	server := script.start(t)
	runner, err := newResidentChildRunner(residentArguments(t, server.URL), spec, journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner(t.Context(), "review"); err == nil {
		t.Fatal("runner recovered without persisting its checkpoint")
	}
	if got := script.requests.Load(); got != 4 {
		t.Fatalf("provider requests = %d, want the compaction attempt only", got)
	}
	data, readErr := os.ReadFile(filepath.Join(journal.Dir(), "transcript.jsonl"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(data), `"type":"child.compacted"`) {
		t.Fatal("a failed recovery recorded a checkpoint")
	}

	// A later reconstructed child resumes the pre-compaction transcript.
	reopened, err := subagents.OpenResidentJournal(root, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	runner, err = newResidentChildRunner(residentArguments(t, server.URL), spec, reopened)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner(t.Context(), "verify"); err != nil {
		t.Fatalf("follow-up error = %v", err)
	}
	bodies := script.capturedBodies()
	if last := bodies[len(bodies)-1]; !strings.Contains(last, "call-1") {
		t.Fatalf("follow-up request = %q, want the pre-compaction transcript", last)
	}
}

func TestRecoverResidentContextOverflowRestoresTranscript(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		writeOpenAIChunk(t, w, map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "compacted digest"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 5, "completion_tokens": 2},
		})
	}))
	defer server.Close()
	resolved, err := Resolve(residentArguments(t, server.URL), true)
	if err != nil {
		t.Fatal(err)
	}
	agent := core.NewAgent(resolved.NewClient(), resolved.Model, "", resolved.ToolRegistry)
	before := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "task"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{"path":"x"}`)}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "call-1", Content: []provider.Content{provider.TextBlock{Text: "missing"}}}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "keep working"}}},
	}
	agent.SetMessages(before)

	journal, err := subagents.OpenResidentJournal(t.TempDir(), "restore-child")
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Accept(subagents.ResidentChildSpec{ID: "restore-child", SessionID: "restore-session", Provider: "openai", Model: "gpt-4o"}, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	overflow := errors.New("openai: http 413: context_length_exceeded")
	err = recoverResidentContextOverflow(t.Context(), agent, journal, func(core.AgentEvent) {}, 0, 4, overflow)
	if !errors.Is(err, overflow) || !strings.Contains(err.Error(), "persist resident child compaction") {
		t.Fatalf("recovery error = %v, want a checkpoint persistence failure", err)
	}
	if !reflect.DeepEqual(agent.Messages(), before) {
		t.Fatalf("transcript = %#v, want the pre-compaction transcript restored", agent.Messages())
	}
}

func residentMessageText(message provider.Message) string {
	var text strings.Builder
	for _, content := range message.Content {
		if block, ok := content.(provider.TextBlock); ok {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}

func TestResidentChildRunnerIgnoresCompactionUsageInReminder(t *testing.T) {
	// The summarization request reports the pre-compaction prompt size. If that
	// gauge armed the next reminder, the first continuation step would claim the
	// compacted transcript sits at the top of the window. The continuation's own
	// usage must still be able to arm a truthful reminder.
	script := &residentOverflowScript{
		toolCallRequests:        2,
		continuationToolCalls:   1,
		finalText:               "overflow recovered result",
		summaryUsageTokens:      1 << 20,
		continuationUsageTokens: 1 << 20,
	}
	server := script.start(t)
	runner, _, _ := newResidentChildTestRunner(t, "usage-child", residentArguments(t, server.URL))
	if err := runner(t.Context(), "review"); err != nil {
		t.Fatalf("runner error = %v, want one recovered turn", err)
	}
	bodies := script.capturedBodies()
	if len(bodies) != 6 {
		t.Fatalf("provider requests = %d, want 3 prompt, 1 compaction, and 2 continued steps", len(bodies))
	}
	if strings.Contains(bodies[4], "Context usage is at") {
		t.Fatalf("first continuation step carries a reminder from the compaction request: %q", bodies[4])
	}
	if !strings.Contains(bodies[5], "Context usage is at") {
		t.Fatalf("second continuation step lost the reminder: %q", bodies[5])
	}
}

func TestResidentCheckpointMessagesDropsOnlyHostContext(t *testing.T) {
	internal := provider.Message{
		Role:    provider.RoleDeveloper,
		Meta:    map[string]string{"internal_context": "true"},
		Content: []provider.Content{provider.TextBlock{Text: "Context usage is at 96% of the model window."}},
	}
	importedNote := provider.Message{
		Role:    provider.RoleDeveloper,
		Content: []provider.Content{provider.TextBlock{Text: "imported developer note"}},
	}
	task := provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "task"}}}
	kept := residentCheckpointMessages([]provider.Message{internal, importedNote, task})
	if len(kept) != 2 || kept[0].Role != provider.RoleDeveloper || kept[1].Role != provider.RoleUser {
		t.Fatalf("checkpoint messages = %#v, want host context dropped and other messages kept", kept)
	}
	if !core.IsInternalContextMessage(internal) || core.IsInternalContextMessage(importedNote) {
		t.Fatal("IsInternalContextMessage misclassified a developer message")
	}
}

func TestResidentChildRunnerKeepsHostContextOutOfCheckpoint(t *testing.T) {
	// Tool turns report a prompt size past the top band, so the child holds a
	// host reminder when the overflow arrives.
	script := &residentOverflowScript{
		toolCallRequests: 2,
		finalText:        "overflow recovered result",
		toolUsageTokens:  1 << 20,
	}
	server := script.start(t)
	runner, journal, _ := newResidentChildTestRunner(t, "checkpoint-context", residentArguments(t, server.URL))
	if err := runner(t.Context(), "review"); err != nil {
		t.Fatalf("runner error = %v, want one recovered turn", err)
	}
	messages, err := subagents.ReadResidentTranscriptMessages(journal.Dir())
	if err != nil {
		t.Fatal(err)
	}
	joined := joinedResidentText(messages)
	if !strings.Contains(joined, "compacted digest") {
		t.Fatalf("resumed transcript = %q, want the checkpoint digest", joined)
	}
	if strings.Contains(joined, "Context usage is at") {
		t.Fatalf("checkpoint replayed a stale host reminder: %q", joined)
	}
}

func TestResidentChildRunnerKeepsRemainingStepAllowanceAcrossRecovery(t *testing.T) {
	// Two tool turns and the overflow leave two of the five allowed steps. The
	// continuation must receive exactly those, not a fresh limit.
	script := &residentOverflowScript{
		toolCallRequests:      2,
		continuationToolCalls: 10,
		finalText:             "unreached",
	}
	server := script.start(t)
	args := residentArguments(t, server.URL)
	args.MaxSteps = 5
	runner, journal, _ := newResidentChildTestRunner(t, "partial-allowance", args)
	err := runner(t.Context(), "review")
	if !errors.Is(err, core.ErrMaxSteps) {
		t.Fatalf("runner error = %v, want the continuation to exhaust the remaining allowance", err)
	}
	if got := script.requests.Load(); got != 6 {
		t.Fatalf("provider requests = %d, want 2 tool turns, the overflow, the compaction, and 2 more steps", got)
	}
	if checkpoints := countResidentCheckpoints(t, journal); checkpoints != 1 {
		t.Fatalf("compaction checkpoints = %d, want 1", checkpoints)
	}
}

func TestResidentChildRunnerDoesNotCompactWithoutHistory(t *testing.T) {
	// The overflow arrives on the first request, so the transcript is too short
	// to summarize without replacing the task itself.
	script := &residentOverflowScript{toolCallRequests: 0, finalText: "unreached"}
	server := script.start(t)
	runner, journal, _ := newResidentChildTestRunner(t, "short-overflow", residentArguments(t, server.URL))
	err := runner(t.Context(), "review")
	if err == nil || !provider.IsContextOverflowError(err) || !strings.Contains(err.Error(), "context_length_exceeded") {
		t.Fatalf("runner error = %v, want the original overflow", err)
	}
	if got := script.requests.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want no compaction attempt", got)
	}
	if checkpoints := countResidentCheckpoints(t, journal); checkpoints != 0 {
		t.Fatalf("compaction checkpoints = %d, want none without history", checkpoints)
	}
}

func joinedResidentText(messages []provider.Message) string {
	var text strings.Builder
	for _, message := range messages {
		text.WriteString(residentMessageText(message))
		text.WriteString("\n")
	}
	return text.String()
}
