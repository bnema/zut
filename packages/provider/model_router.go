package provider

import (
	"context"
	"fmt"
)

const (
	// APICompletions identifies the OpenAI Chat Completions wire API.
	APICompletions = "openai-completions"
	// APIResponses identifies the OpenAI Responses wire API.
	APIResponses = "openai-responses"
)

// modelRouter dispatches each request according to the selected model's API.
// This lets one provider expose models that use different wire protocols while
// remaining reusable across model switches.
type modelRouter struct {
	name     string
	fallback Client
	byAPI    map[string]Client
}

// NewModelRouter creates a client that dispatches requests using Model.API.
// Models with no API override, and models absent from the catalog, use fallback.
func NewModelRouter(name string, fallback Client, byAPI map[string]Client) Client {
	return &modelRouter{name: name, fallback: fallback, byAPI: byAPI}
}

func (c *modelRouter) Name() string { return c.name }

func (c *modelRouter) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	if err := ValidateFastMode(c.Name(), req.FastMode); err != nil {
		return nil, err
	}
	client := c.fallback
	api := ""
	if model, err := FindModel(c.name, req.Model); err == nil {
		api = model.API
	}
	if api == "" && AcceptsUnlistedModels(c.name) {
		// The OpenCode Go catalog is intentionally live-only. Preserve the
		// family route for an explicit model while the first discovery is
		// still in flight or unavailable.
		api = openCodeGoAPIForModel(req.Model)
	}
	if api != "" {
		routed := c.byAPI[api]
		if routed == nil {
			return nil, fmt.Errorf("provider %q has no client for model API %q", c.name, api)
		}
		client = routed
	}
	return client.Stream(ctx, req)
}
