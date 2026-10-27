package provider

import "testing"

func TestCopilotOctoberCatalogAdditions(t *testing.T) {
	for _, tc := range []struct {
		id, name, api   string
		context, output int
		adaptive        bool
	}{
		{"claude-haiku-5.5", "Claude Haiku 5.5", APIAnthropicMessages, 1000000, 128000, true},
		{"gemini-4-argon", "Gemini 4 Argon", APICompletions, 1048576, 65536, false},
		{"gpt-5.4-nano", "GPT-5.4 nano", APIResponses, 400000, 128000, false},
		{"gpt-6.1-sol", "GPT-6.1 Sol", APIResponses, 1050000, 128000, false},
		{"gpt-6.1-terra", "GPT-6.1 Terra", APIResponses, 1050000, 128000, false},
		{"grok-4.7", "Grok 4.7", APIResponses, 500000, 128000, false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			m, err := FindModel(ProviderGitHubCopilot, tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if m.DisplayName != tc.name || m.API != tc.api || m.ContextWindow != tc.context || m.MaxOutput != tc.output || !m.Reasoning || m.AdaptiveThinking != tc.adaptive {
				t.Fatalf("unexpected metadata: %+v", m)
			}
			if m.PriceInput != 0 || m.PriceOutput != 0 || m.PriceCacheRead != 0 || m.PriceCacheWrite != 0 {
				t.Fatalf("unexpected subscription pricing: %+v", m)
			}
		})
	}
}

func TestCopilotCatalogDropsModelsAbsentFromBothSources(t *testing.T) {
	for _, id := range []string{"claude-opus-4.5", "claude-opus-4.6", "claude-sonnet-4.5", "gemini-2.5-pro", "gemini-3-flash-preview", "gemini-3.1-pro-preview", "gpt-4.1", "gpt-4o", "gpt-5.2", "gpt-5.2-codex", "grok-code-fast-1"} {
		if _, err := FindModel(ProviderGitHubCopilot, id); err == nil {
			t.Errorf("retired model %s retained", id)
		}
	}
}
