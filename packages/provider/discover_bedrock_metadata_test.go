package provider

import "testing"

// discoveredWireMaxTokens runs a discovered ID through the same path as
// production (discovery metadata -> MergeCatalog -> FindModel -> the
// agent's MaxTokens -> Bedrock buildRequest) and returns the merged model
// plus the maxTokens that would be sent on the wire.
func discoveredWireMaxTokens(t *testing.T, id string) (Model, int) {
	t.Helper()
	withLiveModels(t, []Model{bedrockDiscoveredModel(id, "us-east-1")})
	m, err := FindModel("amazon-bedrock", id)
	if err != nil {
		t.Fatalf("FindModel(%q): %v", id, err)
	}
	c := &bedrockClient{region: "us-east-1"}
	req, err := c.buildRequest(Request{Model: id, MaxTokens: m.MaxOutput})
	if err != nil {
		t.Fatalf("buildRequest(%q): %v", id, err)
	}
	return m, req.InferenceConfig.MaxTokens
}

// Discovered inference-profile IDs with no exact catalog entry used to
// carry MaxOutput 0, so every request fell back to maxTokens 4096 and
// long responses stopped with max_tokens.
func TestBedrockDiscoveredProfileInheritsBaseMetadata(t *testing.T) {
	base, err := FindModel("amazon-bedrock", "anthropic.claude-sonnet-4-6")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"apac.anthropic.claude-sonnet-4-6", "us-gov.anthropic.claude-sonnet-4-6"} {
		m, wire := discoveredWireMaxTokens(t, id)
		if m.MaxOutput != base.MaxOutput || wire != base.MaxOutput {
			t.Errorf("%s: MaxOutput=%d wire=%d, want %d", id, m.MaxOutput, wire, base.MaxOutput)
		}
		if m.ContextWindow != base.ContextWindow || m.Reasoning != base.Reasoning {
			t.Errorf("%s: metadata not inherited: %+v", id, m)
		}
		if m.PriceOutput != base.PriceOutput {
			t.Errorf("%s: PriceOutput=%v, want %v", id, m.PriceOutput, base.PriceOutput)
		}
	}

	m, _ := discoveredWireMaxTokens(t, "apac.anthropic.claude-sonnet-4-6")
	if m.DisplayName != "Claude Sonnet 4.6 (APAC)" {
		t.Errorf("DisplayName = %q", m.DisplayName)
	}
}

// A base model catalogued only under geo-prefixed IDs still donates its
// metadata, and the donor's region label is not carried over.
func TestBedrockDiscoveredProfileUsesPrefixedCatalogVariant(t *testing.T) {
	const id = "apac.anthropic.claude-opus-5"
	m, wire := discoveredWireMaxTokens(t, id)
	if wire != 128000 || !m.AdaptiveThinking || m.PriceOutput != 25 {
		t.Errorf("%s: wire=%d adaptive=%v price=%v, want 128000 adaptive and $25", id, wire, m.AdaptiveThinking, m.PriceOutput)
	}
	if m.DisplayName != "Claude Opus 5 (APAC)" {
		t.Errorf("DisplayName = %q", m.DisplayName)
	}
}

// An uncatalogued Claude ID must not assume a 64k output limit. Opus 4
// supports only 32k, and an unknown future model has no verified limit.
func TestBedrockDiscoveredUnknownClaudeKeepsFallback(t *testing.T) {
	for _, id := range []string{
		"anthropic.claude-opus-4-20250514-v1:0",
		"us.anthropic.claude-opus-4-20250514-v1:0",
		"anthropic.claude-future-9",
	} {
		m, wire := discoveredWireMaxTokens(t, id)
		if m.MaxOutput != 0 || m.ContextWindow != 0 || wire != 4096 {
			t.Errorf("%s: MaxOutput=%d ctx=%d wire=%d, want 0/0/4096", id, m.MaxOutput, m.ContextWindow, wire)
		}
	}
}

// Legacy Claude and non-Anthropic models cap output well below 64k and
// would reject a larger request, so they keep the client fallback.
func TestBedrockDiscoveredLegacyAndOtherModelsKeepFallback(t *testing.T) {
	for _, id := range []string{
		"anthropic.claude-v2:1",
		"anthropic.claude-instant-v1",
		"us.anthropic.claude-3-5-sonnet-20241022-v2:0",
		"anthropic.claude-3-haiku-20240307-v1:0",
		"mistral.unknown-model-v1:0",
	} {
		m, wire := discoveredWireMaxTokens(t, id)
		if m.MaxOutput != 0 || wire != 4096 {
			t.Errorf("%s: MaxOutput=%d wire=%d, want 0/4096", id, m.MaxOutput, wire)
		}
	}
}

// Exact catalog IDs keep their own catalog metadata through MergeCatalog.
func TestBedrockDiscoveredExactCatalogIDUnchanged(t *testing.T) {
	const id = "au.anthropic.claude-sonnet-4-6"
	want, err := FindModel("amazon-bedrock", id)
	if err != nil {
		t.Fatal(err)
	}
	m, wire := discoveredWireMaxTokens(t, id)
	if m.MaxOutput != want.MaxOutput || wire != want.MaxOutput || m.DisplayName != want.DisplayName {
		t.Errorf("got MaxOutput=%d wire=%d name=%q, want %d %q", m.MaxOutput, wire, m.DisplayName, want.MaxOutput, want.DisplayName)
	}
}
