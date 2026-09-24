package provider

import (
	"context"
	"slices"
	"testing"
)

func TestGPT6SolAndLunaCatalog(t *testing.T) {
	models := []struct {
		id         string
		input      float64
		output     float64
		cacheRead  float64
		cacheWrite float64
		inputAbove float64
		outAbove   float64
		readAbove  float64
		writeAbove float64
	}{
		{"gpt-6-sol", 2, 10, 0.2, 2.5, 4, 15, 0.4, 5},
		{"gpt-6-luna", 0.1, 0.5, 0.01, 0.125, 0.2, 0.75, 0.02, 0.25},
	}
	for _, model := range models {
		for _, name := range []string{"openai", "openai-responses", "openai-codex", "github-copilot"} {
			t.Run(name+"/"+model.id, func(t *testing.T) {
				m, err := FindModel(name, model.id)
				if err != nil {
					t.Fatal(err)
				}
				wantContext := 272000
				if name == "github-copilot" {
					wantContext = 1000000
				}
				if m.API != APIResponses || m.ContextWindow != wantContext || m.MaxOutput != 128000 || !m.Reasoning || m.Speculative {
					t.Fatalf("unexpected model capabilities: %+v", m)
				}
				if m.PriceInput != model.input || m.PriceOutput != model.output || m.PriceCacheRead != model.cacheRead || m.PriceCacheWrite != model.cacheWrite {
					t.Fatalf("unexpected standard prices: %+v", m)
				}
				if m.PriceTierInputTokens != 272000 || m.PriceInputAbove != model.inputAbove || m.PriceOutputAbove != model.outAbove || m.PriceCacheReadAbove != model.readAbove || m.PriceCacheWriteAbove != model.writeAbove {
					t.Fatalf("unexpected long-context prices: %+v", m)
				}
				wantLevels := []string{"", "low", "medium", "high", "xhigh", "max"}
				if got := AvailableReasoningLevels(m); !slices.Equal(got, wantLevels) {
					t.Fatalf("reasoning levels = %q, want %q", got, wantLevels)
				}
			})
		}
	}
}

func TestGPT6SolAndLunaResponsesReasoning(t *testing.T) {
	copilot := NewGithubCopilotClient("test-token").(*copilotClient).router
	clients := map[string]*codexClient{
		"openai":           NewOpenAIResponsesNamed("test-token", "", "openai").(*renamedClient).inner.(*codexClient),
		"openai-responses": NewOpenAIResponsesNamed("test-token", "", "openai-responses").(*renamedClient).inner.(*codexClient),
		"openai-codex":     NewOpenAICodex("test-token", "test-account", "").(*codexClient),
		"github-copilot":   copilot.byAPI[APIResponses].(*codexClient),
	}
	for name, client := range clients {
		for _, model := range []string{"gpt-6-sol", "gpt-6-luna"} {
			t.Run(name+"/"+model, func(t *testing.T) {
				wire, err := client.buildRequest(Request{Model: model, Reasoning: "max"})
				if err != nil {
					t.Fatal(err)
				}
				if wire.Model != model || wire.Reasoning == nil || wire.Reasoning.Effort != "max" {
					t.Fatalf("unexpected request: %+v", wire)
				}
			})
		}
	}
}

func TestGPT6SolAndLunaCopilotDispatch(t *testing.T) {
	router := NewGithubCopilotClient("test-token").(*copilotClient).router
	completions := &routeCaptureClient{name: "github-copilot"}
	responses := &routeCaptureClient{name: "github-copilot"}
	router.fallback = completions
	router.byAPI[APIResponses] = responses

	for _, model := range []string{"gpt-6-sol", "gpt-6-luna"} {
		stream, err := router.Stream(context.Background(), Request{Model: model})
		if err != nil {
			t.Fatal(err)
		}
		for range stream {
		}
	}
	if !slices.Equal(responses.models, []string{"gpt-6-sol", "gpt-6-luna"}) || len(completions.models) != 0 {
		t.Fatalf("Responses models = %v, Completions models = %v", responses.models, completions.models)
	}
}

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
