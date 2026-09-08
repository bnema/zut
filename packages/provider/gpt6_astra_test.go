package provider

import (
	"context"
	"slices"
	"testing"
)

func TestGPT6AstraCatalog(t *testing.T) {
	for _, name := range []string{"openai", "openai-responses", "openai-codex", "github-copilot"} {
		t.Run(name, func(t *testing.T) {
			m, err := FindModel(name, "gpt-6-astra")
			if err != nil {
				t.Fatal(err)
			}
			wantContext := 272000
			if name == "github-copilot" {
				wantContext = 1050000
			}
			if m.API != APIResponses || m.ContextWindow != wantContext || m.MaxOutput != 128000 || !m.Reasoning || m.Speculative {
				t.Fatalf("unexpected model capabilities: %+v", m)
			}
			if m.PriceInput != 10 || m.PriceOutput != 50 || m.PriceCacheRead != 1 || m.PriceCacheWrite != 12.5 {
				t.Fatalf("unexpected standard prices: %+v", m)
			}
			if m.PriceTierInputTokens != 272000 || m.PriceInputAbove != 20 || m.PriceOutputAbove != 75 || m.PriceCacheReadAbove != 2 || m.PriceCacheWriteAbove != 25 {
				t.Fatalf("unexpected long-context prices: %+v", m)
			}
			wantLevels := []string{"", "low", "medium", "high", "xhigh", "max"}
			if got := AvailableReasoningLevels(m); !slices.Equal(got, wantLevels) {
				t.Fatalf("reasoning levels = %q, want %q", got, wantLevels)
			}
		})
	}
}

func TestGPT6AstraResponsesReasoning(t *testing.T) {
	copilot := NewGithubCopilotClient("test-token").(*copilotClient).router
	clients := map[string]*codexClient{
		"openai":           NewOpenAIResponsesNamed("test-token", "", "openai").(*renamedClient).inner.(*codexClient),
		"openai-responses": NewOpenAIResponsesNamed("test-token", "", "openai-responses").(*renamedClient).inner.(*codexClient),
		"openai-codex":     NewOpenAICodex("test-token", "test-account", "").(*codexClient),
		"github-copilot":   copilot.byAPI[APIResponses].(*codexClient),
	}
	for name, client := range clients {
		t.Run(name, func(t *testing.T) {
			for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
				wire, err := client.buildRequest(Request{Model: "gpt-6-astra", Reasoning: effort})
				if err != nil {
					t.Fatal(err)
				}
				if wire.Model != "gpt-6-astra" {
					t.Fatalf("wire model = %q", wire.Model)
				}
				if effort == "" {
					if wire.Reasoning != nil {
						t.Fatalf("unexpected reasoning config: %+v", wire.Reasoning)
					}
				} else if wire.Reasoning == nil || wire.Reasoning.Effort != effort {
					t.Fatalf("reasoning = %+v, want %q", wire.Reasoning, effort)
				}
			}
		})
	}
}

func TestGPT6AstraCopilotDispatch(t *testing.T) {
	router := NewGithubCopilotClient("test-token").(*copilotClient).router
	completions := &routeCaptureClient{name: "github-copilot"}
	responses := &routeCaptureClient{name: "github-copilot"}
	router.fallback = completions
	router.byAPI[APIResponses] = responses

	stream, err := router.Stream(context.Background(), Request{Model: "gpt-6-astra"})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if !slices.Equal(responses.models, []string{"gpt-6-astra"}) || len(completions.models) != 0 {
		t.Fatalf("Responses models = %v, Completions models = %v", responses.models, completions.models)
	}
}
