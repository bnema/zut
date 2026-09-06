package provider

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"
	"sync"
)

// UserModelsFile is the JSON format for user-defined models.
// Place a models.json in $ZUT_HOME to add models that aren't in the
// baked-in catalog or to override catalog entries. Custom providers
// (not in the built-in set) may specify a baseUrl and api format at
// the provider level:
//
//	{
//	  "providers": {
//	    "my-company": {
//	      "baseUrl": "https://llm.mycompany.com/v1",
//	      "api": "openai",
//	      "models": [
//	        {
//	          "id": "company-llm-v2",
//	          "name": "Company LLM v2",
//	          "contextWindow": 128000,
//	          "maxTokens": 32000
//	        }
//	      ]
//	    }
//	  }
//	}
type UserModelsFile struct {
	Providers map[string]UserProvider `json:"providers"`
}

// UserProvider groups models under a provider key.
type UserProvider struct {
	BaseURL string      `json:"baseUrl,omitempty"`
	API     string      `json:"api,omitempty"` // "openai" (default), "openai-responses", or "anthropic"
	Models  []UserModel `json:"models"`
}

// CustomProviderConfig holds runtime config for a user-defined provider
// that isn't part of the built-in catalog.
type CustomProviderConfig struct {
	BaseURL string
	API     string // "openai", "openai-responses", or "anthropic"
}

var (
	customProvidersMu sync.RWMutex
	customProviders   = map[string]CustomProviderConfig{}
)

// CustomProviders returns a snapshot of the user-defined providers loaded
// from models.json. Callers cannot mutate provider-owned registry state.
func CustomProviders() map[string]CustomProviderConfig {
	customProvidersMu.RLock()
	defer customProvidersMu.RUnlock()
	return maps.Clone(customProviders)
}

// UserModel is a single model entry in the user's models.json.
type UserModel struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Reasoning         bool              `json:"reasoning"`
	ReasoningLevelMap map[string]string `json:"reasoningLevelMap,omitempty"`
	ContextWindow     int               `json:"contextWindow"`
	MaxTokens         int               `json:"maxTokens"`
	PriceInput        float64           `json:"priceInput"`
	PriceOutput       float64           `json:"priceOutput"`
	PriceCacheRead    float64           `json:"priceCacheRead"`
	PriceCacheWrite   float64           `json:"priceCacheWrite"`
	BaseURL           string            `json:"baseUrl,omitempty"`
	Input             []string          `json:"input"` // informational only
	API               string            `json:"api"`   // informational only
}

func normalizeReasoningLevelMap(levelMap map[string]string, modelRef string) (map[string]string, []string) {
	if levelMap == nil {
		return nil, nil
	}
	normalized := make(map[string]string, len(levelMap))
	var warnings []string
	for rawLevel, rawTarget := range levelMap {
		level := NormalizeReasoning(rawLevel)
		if reasoningLevelRank(level) == 0 {
			warnings = append(warnings, fmt.Sprintf("models.json: %s reasoningLevelMap has unknown level %q; entry ignored", modelRef, rawLevel))
			continue
		}
		target := NormalizeReasoning(rawTarget)
		if rawTarget != "" && target == "" {
			// Explicit off aliases remove the level just like an empty value.
			normalized[level] = ""
			continue
		}
		if target != "" && reasoningLevelRank(target) == 0 {
			warnings = append(warnings, fmt.Sprintf("models.json: %s reasoningLevelMap has unknown target %q; entry ignored", modelRef, rawTarget))
			continue
		}
		normalized[level] = target
	}
	return normalized, warnings
}

// LoadUserModels reads a models.json file and returns the models
// converted to the internal Model type. Returns nil on any error
// (missing file, bad JSON, etc.) so the caller can treat it as
// optional without error handling.
func LoadUserModels(path string) []Model {
	models, _ := LoadUserModelsWithWarnings(path)
	return models
}

// LoadUserModelsWithWarnings is like LoadUserModels but also returns
// human-readable warnings about every recoverable issue it found in
// the file (unknown provider id, empty model id, malformed JSON for a
// single provider block, etc.). The caller is responsible for
// surfacing the warnings; the file is never rejected wholesale unless
// the top-level JSON itself fails to parse.
func LoadUserModelsWithWarnings(path string) ([]Model, []string) {
	var warnings []string
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var file UserModelsFile
	if err := json.Unmarshal(data, &file); err != nil {
		warnings = append(warnings, fmt.Sprintf("models.json: parse error: %v (file ignored)", err))
		return nil, warnings
	}

	var out []Model
	customProvidersMu.Lock()
	defer customProvidersMu.Unlock()
	// Reset custom providers on each load so removed entries don't linger.
	customProviders = map[string]CustomProviderConfig{}
	for providerName, prov := range file.Providers {
		if providerName == "" {
			warnings = append(warnings, "models.json: empty provider key skipped")
			continue
		}
		// Normalize legacy transport aliases to their provider names.
		normalized := providerName
		switch providerName {
		case "openai-responses":
			normalized = "openai"
		case "anthropic-messages":
			normalized = "anthropic"
		case "moonshot", "moonshot-ai", "kimi-code":
			normalized = "kimi"
		case "deepseek-chat", "deepseek-ai":
			normalized = "deepseek"
		}

		// Register custom providers that carry endpoint metadata either at
		// the provider level or on any model. Model-level baseUrl-only
		// configs still need a custom provider entry so Resolve/NewClient
		// accept the provider name and choose a wire format.
		hasModelBaseURL := false
		for _, um := range prov.Models {
			if um.BaseURL != "" {
				hasModelBaseURL = true
				break
			}
		}
		if prov.BaseURL != "" || prov.API != "" || hasModelBaseURL {
			api := strings.ToLower(strings.TrimSpace(prov.API))
			if api == "" {
				api = "openai"
			}
			// Normalize common aliases for the wire format.
			switch api {
			case "openai-completions", "openai-chat", "chat", "openai":
				api = "openai"
			case "openai-responses", "responses":
				api = APIResponses
			case "anthropic-messages", "messages", "anthropic":
				api = "anthropic"
			default:
				warnings = append(warnings, fmt.Sprintf("models.json: provider %q has unknown api %q; defaulting to openai", providerName, prov.API))
				api = "openai"
			}
			customProviders[normalized] = CustomProviderConfig{
				BaseURL: prov.BaseURL,
				API:     api,
			}
		}

		for i, um := range prov.Models {
			if um.ID == "" {
				warnings = append(warnings, fmt.Sprintf("models.json: provider %q entry #%d has empty id; skipped", providerName, i))
				continue
			}
			if um.ContextWindow < 0 || um.MaxTokens < 0 {
				warnings = append(warnings, fmt.Sprintf("models.json: %s/%s has negative contextWindow/maxTokens; clamped to 0", normalized, um.ID))
				if um.ContextWindow < 0 {
					um.ContextWindow = 0
				}
				if um.MaxTokens < 0 {
					um.MaxTokens = 0
				}
			}
			levelMap, levelWarnings := normalizeReasoningLevelMap(um.ReasoningLevelMap, normalized+"/"+um.ID)
			warnings = append(warnings, levelWarnings...)
			if !um.Reasoning && len(levelMap) > 0 {
				warnings = append(warnings, fmt.Sprintf("models.json: %s reasoningLevelMap ignored because reasoning is false", normalized+"/"+um.ID))
				levelMap = nil
			}

			// Propagate provider-level BaseURL to models without their own.
			modelBaseURL := um.BaseURL
			if modelBaseURL == "" {
				modelBaseURL = prov.BaseURL
			}
			modelAPI := ""
			if cfg, ok := customProviders[normalized]; ok {
				modelAPI = cfg.API
			}
			m := Model{
				Provider:          normalized,
				ID:                um.ID,
				DisplayName:       um.Name,
				API:               modelAPI,
				ContextWindow:     um.ContextWindow,
				MaxOutput:         um.MaxTokens,
				Reasoning:         um.Reasoning,
				ReasoningLevelMap: levelMap,
				PriceInput:        um.PriceInput,
				PriceOutput:       um.PriceOutput,
				PriceCacheRead:    um.PriceCacheRead,
				PriceCacheWrite:   um.PriceCacheWrite,
				BaseURL:           modelBaseURL,
				Source:            "user",
			}
			if m.DisplayName == "" {
				m.DisplayName = m.ID
			}
			out = append(out, m)
		}
	}
	return out, warnings
}

// SetUserModels replaces the durable user-model overlay. User models take
// precedence over the baked-in, live-discovered, and managed catalogs. Keeping
// the overlay separate means a later live refresh cannot discard it.
func SetUserModels(models []Model) {
	activeMu.Lock()
	defer activeMu.Unlock()
	userModels = cloneModels(models)
}
