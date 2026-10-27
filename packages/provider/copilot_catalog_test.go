package provider

import (
	"context"
	"testing"
)

func TestCopilotCatalogCoversPublicChatModels(t *testing.T) {
	groups := map[string][]string{
		APIAnthropicMessages: {"claude-fable-5", "claude-fable-5.1", "claude-haiku-4.5", "claude-opus-4.7", "claude-opus-4.8", "claude-opus-4.8-fast", "claude-opus-5", "claude-opus-5.5", "claude-sonnet-5", "claude-sonnet-5.5"},
		APIResponses:         {"gpt-5-mini", "gpt-5.3-codex", "gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "grok-4.5", "grok-4.6", "mai-code-1.1-flash"},
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

func TestCopilotCatalogKeepsUncertainSonnet46(t *testing.T) {
	m, err := FindModel(ProviderGitHubCopilot, "claude-sonnet-4.6")
	if err != nil {
		t.Fatal(err)
	}
	if m.API != APIAnthropicMessages || m.AdaptiveThinking || usesAdaptiveThinking(m) {
		t.Fatalf("unexpected legacy metadata: %+v", m)
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
