package provider

import (
	"slices"
	"testing"
)

func TestSonnet55CatalogAndThinking(t *testing.T) {
	copilot := NewGithubCopilotClient("synthetic-token").(*copilotClient)
	for _, tc := range []struct {
		provider, id string
		client       *anthropicClient
	}{
		{"anthropic", "claude-sonnet-5-5", NewAnthropic("synthetic-key", "").(*anthropicClient)},
		{"anthropic", "claude-sonnet-5-5", NewAnthropicOAuth("synthetic-token", "").(*anthropicClient)},
		{"github-copilot", "claude-sonnet-5.5", copilot.router.byAPI[APIAnthropicMessages].(*anthropicClient)},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			m, err := FindModel(tc.provider, tc.id)
			if err != nil {
				t.Fatal(err)
			}
			wantContext := 1000000

			if m.ContextWindow != wantContext || m.MaxOutput != 128000 || !m.Reasoning || !m.AdaptiveThinking || m.Speculative {
				t.Fatalf("unexpected capabilities: %+v", m)
			}
			wantCacheRead := 0.1
			if tc.provider == "anthropic" {
				wantCacheRead = 0.2
			}
			if m.PriceInput != 2 || m.PriceOutput != 10 || m.PriceCacheRead != wantCacheRead || m.PriceCacheWrite != 2.5 {
				t.Fatalf("unexpected pricing: %+v", m)
			}
			want := []string{"low", "medium", "high", "xhigh", "max"}
			if got := AvailableReasoningLevels(m); !slices.Equal(got, want) {
				t.Fatalf("levels = %q, want %q", got, want)
			}
			for _, level := range []string{"", "off", "minimum", "low", "medium", "high", "xhigh", "max"} {
				effort := level
				if level == "" || level == "off" || level == "minimum" {
					effort = "low"
				}
				if got := ClampReasoningForModel(m, level); got != effort {
					t.Errorf("clamp(%q) = %q, want %q", level, got, effort)
				}
				temperature := float32(0.5)
				wire, err := tc.client.buildRequest(Request{Model: tc.id, Reasoning: level, Temperature: &temperature})
				if err != nil {
					t.Fatal(err)
				}
				if wire.Model != tc.id || wire.MaxTokens != 128000 || wire.Temperature != nil {
					t.Fatalf("unexpected request: %+v", wire)
				}
				if wire.Thinking == nil || wire.Thinking.Type != "adaptive" || wire.Thinking.BudgetTokens != 0 {
					t.Fatalf("thinking = %+v", wire.Thinking)
				}
				if wire.OutputConfig == nil || wire.OutputConfig.Effort != effort {
					t.Fatalf("output config = %+v, want effort %q", wire.OutputConfig, effort)
				}
			}
		})
	}
}
