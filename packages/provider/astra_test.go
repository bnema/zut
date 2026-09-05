package provider

import (
	"math"
	"slices"
	"testing"
)

func TestAstraReleasedMetadata(t *testing.T) {
	for _, name := range []string{"openai", "openai-codex"} {
		t.Run(name, func(t *testing.T) {
			m, err := FindModel(name, "gpt-6-astra")
			if err != nil {
				t.Fatal(err)
			}
			if m.ContextWindow != 500000 || m.MaxOutput != 128000 || m.Speculative {
				t.Fatalf("released model must retain the intentional 500k budget: %+v", m)
			}
			if name == "openai" && m.API != APIResponses {
				t.Fatalf("API = %q, want Responses", m.API)
			}
			if !slices.Contains(AvailableReasoningLevels(m), "max") || ClampReasoningForModel(m, "max") != "max" {
				t.Fatalf("native max unavailable: %v", AvailableReasoningLevels(m))
			}
			if m.PriceTierInputTokens != 272000 || m.PriceInputAbove != 20 || m.PriceOutputAbove != 75 || m.PriceCacheReadAbove != 2 || m.PriceCacheWriteAbove != 25 {
				t.Fatalf("long-context rates: %+v", m)
			}
			for _, tc := range []struct {
				name  string
				usage Usage
				want  float64
			}{
				{"at_threshold", Usage{InputTokens: 100000, CacheReadTokens: 100000, CacheWriteTokens: 72000, OutputTokens: 1000}, 2.05},
				{"above_threshold", Usage{InputTokens: 100000, CacheReadTokens: 100001, CacheWriteTokens: 72000, OutputTokens: 1000}, 4.075002},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if got := ComputeCost(m, tc.usage); math.Abs(got-tc.want) > 1e-9 {
						t.Fatalf("cost = %.9f, want %.9f", got, tc.want)
					}
				})
			}
		})
	}
}

func TestAstraResponsesCapabilities(t *testing.T) {
	public := NewOpenAIResponsesNamed("synthetic-token", "", "openai").(*renamedClient).inner.(*responsesWebSocketClient).http
	custom := NewOpenAIResponsesNamed("synthetic-token", "https://example.com/v1/responses", "openai").(*renamedClient).inner.(*codexClient)
	for _, tc := range []struct {
		name       string
		client     *codexClient
		breakpoint bool
	}{
		{"public", public, true},
		{"subscription", newOpenAICodexClient("synthetic-token", "synthetic-account", ""), false},
		{"custom", custom, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := tc.client.buildRequest(Request{Model: "gpt-6-astra", Reasoning: "max", Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hello"}}}}})
			if err != nil {
				t.Fatal(err)
			}
			if wire.Reasoning == nil || wire.Reasoning.Effort != "max" {
				t.Fatalf("reasoning = %+v, want max", wire.Reasoning)
			}
			if !tc.breakpoint {
				if wire.PromptCacheOptions != nil || len(wire.Input) != 1 {
					t.Fatalf("undeclared endpoint received explicit caching: %+v", wire)
				}
				return
			}
			if wire.PromptCacheOptions == nil || wire.PromptCacheOptions.Mode != "implicit" || len(wire.Input) != 2 {
				t.Fatalf("missing stable boundary with implicit history caching: %+v", wire)
			}
			boundary, ok := wire.Input[0].(codexInputMessage)
			if !ok || boundary.Role != "developer" || len(boundary.Content) != 1 {
				t.Fatalf("boundary = %#v", wire.Input[0])
			}
			text, ok := boundary.Content[0].(codexInputText)
			if !ok || text.PromptCacheBreakpoint == nil || text.PromptCacheBreakpoint.Mode != "explicit" {
				t.Fatalf("breakpoint = %#v", boundary.Content[0])
			}
		})
	}
}
