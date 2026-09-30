package provider

import (
	"context"
	"strings"
	"testing"
)

func TestCopilotCatalogCoversPublicChatModels(t *testing.T) {
	groups := map[string][]string{
		APIAnthropicMessages: {"claude-fable-5", "claude-fable-5.1", "claude-haiku-4.5", "claude-opus-4.7", "claude-opus-4.8", "claude-opus-4.8-fast", "claude-opus-5", "claude-sonnet-5"},
		APIResponses:         {"gpt-5-mini", "gpt-5.3-codex", "gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-6-astra", "grok-4.5", "grok-4.6", "mai-code-1.1-flash"},
		APICompletions:       {"gemini-3.5-flash", "gemini-3.6-flash", "gemini-3.7-flash", "gemini-3.8-flash", "kimi-k2.7-code", "kimi-k3"},
	}
	for api, ids := range groups {
		for _, id := range ids {
			t.Run(id, func(t *testing.T) {
				m, err := FindModel("github-copilot", id)
				if err != nil {
					t.Fatal(err)
				}
				if m.API != api {
					t.Fatalf("API = %q, want %q", m.API, api)
				}
				if m.ContextWindow <= 0 || m.MaxOutput <= 0 || m.MaxOutput >= m.ContextWindow {
					t.Fatalf("invalid context/output limits: %d/%d", m.ContextWindow, m.MaxOutput)
				}
				if m.BaseURL != copilotDefaultBaseURL || !m.Reasoning {
					t.Fatal("invalid endpoint or reasoning metadata")
				}
				client := NewGithubCopilotClient("synthetic-token").(*copilotClient)
				captures := make(map[string]*routeCaptureClient)
				for _, key := range []string{APIAnthropicMessages, APIResponses, APICompletions} {
					capture := &routeCaptureClient{name: "github-copilot"}
					captures[key] = capture
					client.router.byAPI[key] = capture
				}
				stream, err := client.Stream(context.Background(), Request{Model: id})
				if err != nil {
					t.Fatal(err)
				}
				for range stream {
				}
				for key, capture := range captures {
					want := 0
					if key == api {
						want = 1
					}
					if len(capture.models) != want {
						t.Errorf("%s routed to %s incorrectly", id, key)
					}
				}
			})
		}
	}
}

func TestCopilotCatalogKeepsPreviouslySupportedIDsAndExcludesUtilityModels(t *testing.T) {
	// Rows that shipped before the catalog refresh stay selectable until
	// retirement is verified; account availability hides unavailable ones.
	kept := map[string]string{
		"claude-opus-4.5": APIAnthropicMessages, "claude-opus-4.6": APIAnthropicMessages,
		"claude-sonnet-4.5": APIAnthropicMessages, "claude-sonnet-4.6": APIAnthropicMessages,
		"gemini-2.5-pro": APICompletions, "gemini-3-flash-preview": APICompletions, "gemini-3.1-pro-preview": APICompletions,
		"gpt-4.1": APICompletions, "gpt-4o": APICompletions, "grok-code-fast-1": APICompletions,
		"gpt-5.2": APIResponses, "gpt-5.2-codex": APIResponses,
	}
	for id, api := range kept {
		m, err := FindModel("github-copilot", id)
		if err != nil {
			t.Errorf("previously supported Copilot model %q dropped: %v", id, err)
			continue
		}
		if m.API != api {
			t.Errorf("%s API = %q, want %q", id, m.API, api)
		}
		if (strings.HasSuffix(id, "-4.5") || strings.HasSuffix(id, "-4.6")) && (m.AdaptiveThinking || usesAdaptiveThinking(m)) {
			t.Errorf("%s must not use adaptive thinking", id)
		}
	}
	// GPT-5.4 nano is a Codex VS Code model, not a Copilot Chat choice.
	if _, err := FindModel("github-copilot", "gpt-5.4-nano"); err == nil {
		t.Error("Copilot catalog includes gpt-5.4-nano")
	}
}

func TestConfigureCopilotModelAdaptiveDefaultsFollowKnownFamilies(t *testing.T) {
	for id, want := range map[string]bool{
		"claude-opus-4.6": false, "claude-sonnet-4.6": false, "claude-sonnet-4.5": false, "claude-haiku-4.5": false,
		"claude-opus-4.7": true, "claude-opus-4.8": true, "claude-opus-5": true, "claude-sonnet-5": true, "claude-fable-5.1": true,
	} {
		m := Model{Provider: ProviderGitHubCopilot, ID: id}
		configureCopilotModel(&m)
		if m.API != APIAnthropicMessages || m.AdaptiveThinking != want {
			t.Errorf("%s API=%q adaptive=%v, want adaptive=%v", id, m.API, m.AdaptiveThinking, want)
		}
	}
	// Explicit metadata for a 4.6 row is preserved.
	m := Model{Provider: ProviderGitHubCopilot, ID: "claude-opus-4.6", AdaptiveThinking: true}
	configureCopilotModel(&m)
	if !m.AdaptiveThinking {
		t.Error("explicit adaptive metadata was cleared")
	}
}

func TestCopilotCatalogOmitsRetiredMAIPickerRow(t *testing.T) {
	if _, err := FindModel("github-copilot", "mai-code-1-flash-picker"); err == nil {
		t.Error("retired mai-code-1-flash-picker row is still in the catalog")
	}
}
