package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
)

func TestOrchestratedWaitDeliversReportOnlyThroughContinuation(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		writeOpenAIChunk(t, w, map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "synthetic child report"}, "finish_reason": "stop"}},
		})
	}))
	defer server.Close()
	tracker := subagents.NewCompletionTracker()
	args := Args{CWD: t.TempDir(), Provider: "openai", Model: "gpt-4o", APIKey: "synthetic", BaseURL: server.URL, NoContextFiles: true, NoSkill: true, NoLSP: true}
	runtime := newOrchestratedRuntime(t.Context(), args, Resolved{CWD: args.CWD, Provider: args.Provider, Model: args.Model, BaseURL: args.BaseURL}, Config{}, tracker)
	t.Cleanup(func() { _ = runtime.ResidentManager().Close(context.Background()) })
	facade := runtime.InjectTools(core.Registry{})["subagent"]
	if facade == nil {
		t.Fatal("subagent tool unavailable")
	}
	result, err := facade.Execute(t.Context(), json.RawMessage(`{"action":"spawn","task":"return a report","wait":5}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("spawn = (%#v, %v)", result, err)
	}
	encoded, err := json.Marshal(result.Content)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "synthetic child report") || !strings.Contains(string(encoded), "host_update") {
		t.Fatalf("tool report = %s", encoded)
	}
	calls := 0
	got, err := runHeadlessContinuation(t.Context(), tracker, "initial", func(_ context.Context, prompt string) (string, error) {
		calls++
		if strings.Count(prompt, "[auto-subagents update]") != 1 || strings.Count(prompt, "synthetic child report") != 1 {
			t.Errorf("continuation report = %q", prompt)
		}
		return "synthesis", nil
	})
	if err != nil || calls != 1 || got != "synthesis" {
		t.Fatalf("continuation = (%q, %v), calls=%d", got, err, calls)
	}
}
