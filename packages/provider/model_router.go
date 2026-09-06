package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
	name             string
	fallback         Client
	byAPI            map[string]Client
	modelOverridesMu sync.RWMutex
	modelOverrides   map[string]Model
	dynamicCatalog   bool
	scopedCatalog    bool
}

// ModelMetadataSetter lets a runtime-owned client retain model metadata
// without consulting the process-global catalog on every request.
type ModelMetadataSetter interface {
	SetModelMetadata(Model)
}

// NewModelRouter creates a client that dispatches requests using Model.API.
// Models with no API override, and models absent from the catalog, use fallback.
func NewModelRouter(name string, fallback Client, byAPI map[string]Client) Client {
	return &modelRouter{name: name, fallback: fallback, byAPI: byAPI}
}

func (c *modelRouter) Name() string { return c.name }

func (c *modelRouter) SetModelMetadata(model Model) {
	model = cloneModel(model)
	c.modelOverridesMu.Lock()
	if c.modelOverrides == nil {
		c.modelOverrides = make(map[string]Model)
	}
	c.modelOverrides[model.ID] = model
	c.modelOverridesMu.Unlock()

	setModelMetadata(c.fallback, model)
	for _, client := range c.byAPI {
		setModelMetadata(client, model)
	}
}

func (c *modelRouter) modelOverride(id string) (Model, bool) {
	c.modelOverridesMu.RLock()
	model, ok := c.modelOverrides[id]
	if ok {
		model = cloneModel(model)
	}
	c.modelOverridesMu.RUnlock()
	return model, ok
}

func setModelMetadata(client Client, model Model) {
	if setter, ok := client.(ModelMetadataSetter); ok {
		setter.SetModelMetadata(model)
	}
}

func (c *modelRouter) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	req.Model = strings.TrimSpace(req.Model)
	if err := ValidateFastMode(c.Name(), req.FastMode); err != nil {
		return nil, err
	}
	client := c.fallback
	api := ""
	found := false
	if c.dynamicCatalog {
		// Normal CLI/RPC runtimes follow the process-global catalog so a
		// background refresh can replace stale request metadata. Keep a
		// bootstrap override only until an authoritative live entry exists.
		if model, err := FindModel(c.name, req.Model); err == nil {
			api = model.API
			found = true
			c.SetModelMetadata(model)
		} else if !IsProviderCatalogAuthoritative(c.name) {
			if model, ok := c.modelOverride(req.Model); ok {
				api = model.API
				found = true
			}
		}
	} else if model, ok := c.modelOverride(req.Model); ok {
		api = model.API
		found = true
	} else if !c.scopedCatalog {
		if model, err := FindModel(c.name, req.Model); err == nil {
			api = model.API
			found = true
		}
	}
	if !found && c.dynamicCatalog && IsProviderCatalogAuthoritative(c.name) {
		return nil, fmt.Errorf("unknown model %q (provider=%q)", req.Model, c.name)
	}
	if api == "" && !c.scopedCatalog && AcceptsUnlistedModels(c.name) {
		// The OpenCode Go catalog is intentionally live-only. Preserve the
		// family route for an explicit model while the first discovery is
		// still in flight or unavailable.
		api = openCodeGoAPIForModel(req.Model)
	}
	if api != "" {
		routed := c.byAPI[api]
		if routed == nil && api == APICompletions {
			routed = c.fallback
		}
		if routed == nil {
			return nil, fmt.Errorf("provider %q has no client for model API %q", c.name, api)
		}
		client = routed
	}
	return client.Stream(ctx, req)
}
