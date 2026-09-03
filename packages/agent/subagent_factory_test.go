package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

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

func TestConfigureResidentBudgetOnlyStopsAtExhaustion(t *testing.T) {
	agent := core.NewAgent(nil, "model", "system", core.Registry{"read": nil})
	budget := subagents.NewRolloutBudget(1_000, provider.Usage{})
	configureResidentBudget(agent, budget)

	budget.Observe(provider.Usage{InputTokens: 900})
	allowed, reason, contextText := agent.BeforeTurnContext(t.Context(), 1)
	if !allowed || reason != "" || contextText != "" {
		t.Fatalf("near-limit context = allowed=%t reason=%q context=%q", allowed, reason, contextText)
	}
	if len(agent.ToolsSnapshot()) != 1 || agent.MaxTokens != 0 {
		t.Fatalf("near-limit agent = tools=%#v max_tokens=%d", agent.ToolsSnapshot(), agent.MaxTokens)
	}

	budget.Observe(provider.Usage{InputTokens: 1_000})
	allowed, reason, _ = agent.BeforeTurnContext(t.Context(), 2)
	if allowed || reason != subagents.ErrBudgetExceeded.Error() {
		t.Fatalf("exhausted context = allowed=%t reason=%q", allowed, reason)
	}
}

func TestResidentChildRunnerKeepsToolsAtNinetyPercent(t *testing.T) {
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

	runner, journal := newBudgetedResidentTestRunner(t, server.URL, 1_000)
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

func TestResidentChildRunnerTerminatesAtBudgetLimit(t *testing.T) {
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

	runner, journal := newBudgetedResidentTestRunner(t, server.URL, 1_000)
	if err := runner(t.Context(), "review"); err != nil {
		t.Fatalf("runner error = %v, want terminal result", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("provider requests = %d, want 1", got)
	}
	if summary := latestResidentAssistantText(t, journal); summary != "best available result" {
		t.Fatalf("latest summary = %q", summary)
	}
	if err := runner(t.Context(), "continue"); !errors.Is(err, subagents.ErrBudgetExceeded) {
		t.Fatalf("exhausted follow-up error = %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("exhausted follow-up made %d provider requests, want 1 total", got)
	}
}

func newBudgetedResidentTestRunner(t *testing.T, baseURL string, limit int64) (subagents.ResidentTurnRunner, *subagents.ResidentJournal) {
	t.Helper()
	spec := subagents.ResidentChildSpec{
		ID: "budget-child", SessionID: "budget-session", Provider: "openai", Model: "gpt-4o",
		Tools: []string{"read"}, BudgetLimit: limit,
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
