package provider

import (
	"context"
	"strings"
	"testing"
)

func TestOpenAIFastModeAddsServiceTier(t *testing.T) {
	client := NewOpenAI("key", "").(*openaiClient)

	wire, err := client.buildRequest(Request{Model: "gpt-5", FastMode: true})
	if err != nil {
		t.Fatal(err)
	}
	if wire.ServiceTier != fastModeServiceTier {
		t.Fatalf("service_tier = %q, want %q", wire.ServiceTier, fastModeServiceTier)
	}

	wire, err = client.buildRequest(Request{Model: "gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	if wire.ServiceTier != "" {
		t.Fatalf("service_tier = %q with fast mode disabled, want omitted", wire.ServiceTier)
	}
}

func TestOpenAICodexFastModeAddsServiceTier(t *testing.T) {
	client := newOpenAICodexClient("token", "account", "")

	wire, err := client.buildRequest(Request{Model: "gpt-5.6-sol", FastMode: true})
	if err != nil {
		t.Fatal(err)
	}
	if wire.ServiceTier != fastModeServiceTier {
		t.Fatalf("service_tier = %q, want %q", wire.ServiceTier, fastModeServiceTier)
	}
}

func TestFastModeIsRestrictedToOpenAIProviders(t *testing.T) {
	for _, providerName := range []string{"openai", "openai-codex", "openai-responses"} {
		if err := ValidateFastMode(providerName, true); err != nil {
			t.Errorf("ValidateFastMode(%q, true) = %v", providerName, err)
		}
	}

	for _, providerName := range []string{"anthropic", "google", "openrouter", "azure-openai-responses"} {
		err := ValidateFastMode(providerName, true)
		if err == nil || !strings.Contains(err.Error(), "only supported for OpenAI providers") {
			t.Errorf("ValidateFastMode(%q, true) = %v, want unsupported-provider error", providerName, err)
		}
	}
}

func TestFastModeRejectedBeforeNonOpenAIRequest(t *testing.T) {
	client := NewOpenAICompat("openrouter", "key", "", "")
	_, err := client.Stream(context.Background(), Request{Model: "gpt-5", FastMode: true})
	if err == nil || !strings.Contains(err.Error(), "only supported for OpenAI providers") {
		t.Fatalf("Stream error = %v, want unsupported-provider error", err)
	}
}
