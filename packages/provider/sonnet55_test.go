package provider

import (
	"slices"
	"testing"
)

func TestSonnet55CatalogAndThinking(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client *anthropicClient
	}{
		{"api-key", NewAnthropic("synthetic-key", "").(*anthropicClient)},
		{"oauth", NewAnthropicOAuth("synthetic-token", "").(*anthropicClient)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const id = "claude-sonnet-5-5"
			m, err := FindModel("anthropic", id)
			if err != nil {
				t.Fatal(err)
			}
			if m.ContextWindow != 1000000 || m.MaxOutput != 128000 || !m.Reasoning || !m.AdaptiveThinking || m.Speculative {
				t.Fatalf("unexpected capabilities: %+v", m)
			}
			if m.PriceInput != 2 || m.PriceOutput != 10 || m.PriceCacheRead != 0.2 || m.PriceCacheWrite != 2.5 {
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
				wire, err := tc.client.buildRequest(Request{Model: id, Reasoning: level, Temperature: &temperature})
				if err != nil {
					t.Fatal(err)
				}
				if wire.Model != id || wire.MaxTokens != 128000 || wire.Temperature != nil {
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

func TestSonnet55ReasoningOverrides(t *testing.T) {
	m, err := FindModel("anthropic", "claude-sonnet-5-5")
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
