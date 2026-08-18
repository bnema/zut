package sdk

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
)

func TestToEventPreservesCacheDiagnostics(t *testing.T) {
	for _, diagnostics := range []core.EvCacheDiagnostics{
		{Eligible: true, Mode: "automatic", Transport: "http_sse", Continuation: "full"},
		{Eligible: false, Mode: "below_minimum", Transport: "websocket", Continuation: "incremental"},
	} {
		event := toEvent(diagnostics)
		if event.Type != "cache_diagnostics" || event.CacheDiagnostics == nil {
			t.Fatalf("event = %#v", event)
		}
		got := event.CacheDiagnostics
		if got.Eligible != diagnostics.Eligible || got.Mode != diagnostics.Mode || got.Transport != diagnostics.Transport || got.Continuation != diagnostics.Continuation {
			t.Fatalf("cache diagnostics = %#v, want %#v", got, diagnostics)
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if !json.Valid(encoded) {
			t.Fatalf("encoded diagnostics are invalid JSON: %s", encoded)
		}
		if !diagnostics.Eligible && !strings.Contains(string(encoded), `"eligible":false`) {
			t.Fatalf("encoded ineligible diagnostics = %s, want eligible=false", encoded)
		}
	}
}

func TestToEventPreservesActivityAndToolStreamFacts(t *testing.T) {
	retry := toEvent(core.EvRetryScheduled{
		Scope: core.RetryScopeProvider, Attempt: 2, MaxAttempts: 3, Delay: 250 * time.Millisecond,
	})
	if retry.Type != "retry_scheduled" || retry.Scope != "provider" || retry.Attempt != 2 || retry.MaxAttempts != 3 || retry.DelayMS == nil || *retry.DelayMS != 250 {
		t.Fatalf("retry event = %#v", retry)
	}

	zeroDelay := toEvent(core.EvRetryScheduled{Scope: core.RetryScopeProvider, Attempt: 2, MaxAttempts: 3})
	encoded, err := json.Marshal(zeroDelay)
	if err != nil {
		t.Fatalf("marshal zero-delay retry: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("decode zero-delay retry: %v", err)
	}
	if got, ok := wire["delay_ms"]; !ok || got != float64(0) {
		t.Fatalf("zero-delay retry JSON delay_ms = %#v, want 0", got)
	}

	toolStart := toEvent(core.EvToolUseStart{ID: "call-1", Name: "read"})
	if toolStart.Type != "tool_use_start" || toolStart.ID != "call-1" || toolStart.Name != "read" {
		t.Fatalf("tool start = %#v", toolStart)
	}
	toolArgs := toEvent(core.EvToolUseArgs{ID: "call-1", Delta: `{"path":"a"`})
	if toolArgs.Type != "tool_use_args" || toolArgs.ID != "call-1" || toolArgs.Delta == "" {
		t.Fatalf("tool args = %#v", toolArgs)
	}
	toolEnd := toEvent(core.EvToolExecutionStarted{ID: "call-1", Name: "read"})
	if toolEnd.Type != "tool_execution_started" || toolEnd.ID != "call-1" || toolEnd.Name != "read" {
		t.Fatalf("tool execution = %#v", toolEnd)
	}
}
