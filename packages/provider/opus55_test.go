package provider

import (
	"slices"
	"testing"
)

func TestClaude55CatalogAndThinking(t *testing.T) {
	type pricing struct{ input, output, cacheRead, cacheWrite float64 }
	copilot := NewGithubCopilotClient("synthetic-token").(*copilotClient)
	copilotAnthropic := copilot.router.byAPI[APIAnthropicMessages].(*anthropicClient)
	for _, tc := range []struct {
		name, provider, id string
		client             *anthropicClient
		context            int
		price              pricing
	}{
		{"opus/anthropic", "anthropic", "claude-opus-5-5", NewAnthropic("synthetic-key", "").(*anthropicClient), 500000, pricing{4, 20, 0.2, 5}},
		{"opus/anthropic-oauth", "anthropic", "claude-opus-5-5", NewAnthropicOAuth("synthetic-token", "").(*anthropicClient), 500000, pricing{4, 20, 0.2, 5}},
		{"opus/github-copilot", "github-copilot", "claude-opus-5.5", copilotAnthropic, 1000000, pricing{4, 20, 0.2, 5}},
		{"sonnet/anthropic", "anthropic", "claude-sonnet-5-5", NewAnthropic("synthetic-key", "").(*anthropicClient), 1000000, pricing{2, 10, 0.2, 2.5}},
		{"sonnet/anthropic-oauth", "anthropic", "claude-sonnet-5-5", NewAnthropicOAuth("synthetic-token", "").(*anthropicClient), 1000000, pricing{2, 10, 0.2, 2.5}},
		{"sonnet/github-copilot", "github-copilot", "claude-sonnet-5.5", copilotAnthropic, 1000000, pricing{2, 10, 0.1, 2.5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := FindModel(tc.provider, tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if m.ContextWindow != tc.context || m.MaxOutput != 128000 || !m.Reasoning || !m.AdaptiveThinking || m.Speculative {
				t.Fatalf("unexpected capabilities: %+v", m)
			}
			got := pricing{m.PriceInput, m.PriceOutput, m.PriceCacheRead, m.PriceCacheWrite}
			if got != tc.price {
				t.Fatalf("pricing = %+v, want %+v", got, tc.price)
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

func TestOpus55ReasoningOverrides(t *testing.T) {
	m, err := FindModel("anthropic", "claude-opus-5-5")
	if err != nil {
		t.Fatal(err)
	}
	m.ReasoningLevelMap = map[string]string{"max": "xhigh"}
	want := []string{"low", "medium", "high", "xhigh"}
	if got := AvailableReasoningLevels(m); !slices.Equal(got, want) {
		t.Fatalf("levels = %q, want %q", got, want)
	}
	if got := ClampReasoningForModel(m, "off"); got != "low" {
		t.Fatalf("off clamped to %q, want low", got)
	}
}
