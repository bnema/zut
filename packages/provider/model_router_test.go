package provider

import (
	"context"
	"testing"
)

type routeCaptureClient struct {
	name   string
	models []string
}

func (c *routeCaptureClient) Name() string { return c.name }
func (c *routeCaptureClient) Stream(_ context.Context, req Request) (<-chan Event, error) {
	c.models = append(c.models, req.Model)
	out := make(chan Event)
	close(out)
	return out, nil
}

func TestModelRouterDispatchesByModelAPI(t *testing.T) {
	completions := &routeCaptureClient{name: "xai"}
	responses := &routeCaptureClient{name: "xai"}
	router := NewModelRouter("xai", completions, map[string]Client{
		APIResponses: responses,
	})

	for _, model := range []string{"grok-4.5", "grok-build-0.1"} {
		stream, err := router.Stream(context.Background(), Request{Model: model})
		if err != nil {
			t.Fatal(err)
		}
		for range stream {
		}
	}

	if len(responses.models) != 1 || responses.models[0] != "grok-4.5" {
		t.Fatalf("Responses models = %v", responses.models)
	}
	if len(completions.models) != 1 || completions.models[0] != "grok-build-0.1" {
		t.Fatalf("Completions models = %v", completions.models)
	}
}

func TestModelRouterRejectsMissingAPIClient(t *testing.T) {
	router := NewModelRouter("xai", &routeCaptureClient{name: "xai"}, nil)
	if _, err := router.Stream(context.Background(), Request{Model: "grok-4.5"}); err == nil {
		t.Fatal("expected missing API client error")
	}
}

func TestOpenCodeGoRoutesLunaToResponses(t *testing.T) {
	preserveActiveCatalog(t)
	SetLiveModels([]Model{
		{Provider: "opencode-go", ID: "gpt-5.6-luna", API: APIResponses},
		{Provider: "opencode-go", ID: "kimi-k3"},
		{Provider: "opencode-go", ID: "shape-completions", API: APICompletions},
	})

	router := NewOpenCodeGo("token", "https://example.com/go/v1").(*modelRouter)
	if got := router.fallback.(*openaiClient).baseURL; got != "https://example.com/go/v1" {
		t.Fatalf("Completions base URL = %q", got)
	}
	responses := router.byAPI[APIResponses].(*renamedClient).inner.(*codexClient)
	if got := responses.baseURL; got != "https://example.com/go/v1/responses" {
		t.Fatalf("Responses base URL = %q", got)
	}

	completionsCapture := &routeCaptureClient{name: "opencode-go"}
	responsesCapture := &routeCaptureClient{name: "opencode-go"}
	router.fallback = completionsCapture
	router.byAPI[APIResponses] = responsesCapture
	for _, model := range []string{"gpt-5.6-luna", "kimi-k3", "shape-completions"} {
		stream, err := router.Stream(context.Background(), Request{Model: model})
		if err != nil {
			t.Fatal(err)
		}
		for range stream {
		}
	}

	if len(responsesCapture.models) != 1 || responsesCapture.models[0] != "gpt-5.6-luna" {
		t.Fatalf("Responses models = %v", responsesCapture.models)
	}
	if len(completionsCapture.models) != 2 || completionsCapture.models[0] != "kimi-k3" || completionsCapture.models[1] != "shape-completions" {
		t.Fatalf("Completions models = %v", completionsCapture.models)
	}
}

func TestOpenCodeGoRoutesUncataloguedGPT56ToResponses(t *testing.T) {
	preserveActiveCatalog(t)
	SetLiveModels(nil)

	responses := &routeCaptureClient{name: "opencode-go"}
	completions := &routeCaptureClient{name: "opencode-go"}
	router := NewModelRouter("opencode-go", completions, map[string]Client{
		APIResponses: responses,
	})
	stream, err := router.Stream(context.Background(), Request{Model: "gpt-5.6-future"})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if len(responses.models) != 1 || responses.models[0] != "gpt-5.6-future" {
		t.Fatalf("Responses models = %v", responses.models)
	}
	if len(completions.models) != 0 {
		t.Fatalf("Completions models = %v, want none", completions.models)
	}
}

func TestOpenCodeGoDynamicRouterRefreshesMetadata(t *testing.T) {
	preserveActiveCatalog(t)
	SetLiveModels([]Model{{Provider: ProviderOpenCodeGo, ID: "refreshable-model", API: APIResponses}})

	responses := &routeCaptureClient{name: ProviderOpenCodeGo}
	completions := &routeCaptureClient{name: ProviderOpenCodeGo}
	router := NewOpenCodeGo("token", "https://example.com/go/v1").(*modelRouter)
	router.fallback = completions
	router.byAPI[APIResponses] = responses
	stream, err := router.Stream(context.Background(), Request{Model: "refreshable-model"})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if router.modelOverrides["refreshable-model"].API != APIResponses {
		t.Fatalf("initial router metadata = %+v", router.modelOverrides["refreshable-model"])
	}

	SetLiveModels([]Model{{Provider: ProviderOpenCodeGo, ID: "refreshable-model", API: APICompletions}})
	stream, err = router.Stream(context.Background(), Request{Model: "refreshable-model"})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if router.modelOverrides["refreshable-model"].API != APICompletions {
		t.Fatalf("refreshed router metadata = %+v, want completions", router.modelOverrides["refreshable-model"])
	}
	if len(completions.models) != 1 || completions.models[0] != "refreshable-model" {
		t.Fatalf("Completions models = %v, want refreshed model", completions.models)
	}
}

func TestOpenCodeGoDynamicRouterRejectsUnknownAuthoritativeModel(t *testing.T) {
	preserveActiveCatalog(t)
	SetLiveModelsForProviders([]Model{{Provider: ProviderOpenCodeGo, ID: "served-model"}}, []string{ProviderOpenCodeGo})

	router := NewOpenCodeGo("token", "https://example.com/go/v1")
	if _, err := router.Stream(context.Background(), Request{Model: "removed-model"}); err == nil {
		t.Fatal("authoritative OpenCode Go router accepted removed model")
	}
}
