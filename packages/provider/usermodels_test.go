package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetUserModelsPreservesCatalogWithoutLiveOverlay(t *testing.T) {
	SetLiveModels(nil)
	t.Cleanup(func() { SetLiveModels(nil) })

	SetUserModels([]Model{{
		Provider:    "custom-test",
		ID:          "custom-model",
		DisplayName: "Custom Model",
		Source:      "user",
	}})

	if _, err := FindModel("anthropic", "claude-sonnet-4-5"); err != nil {
		t.Fatalf("built-in model hidden after SetUserModels: %v", err)
	}
	if _, err := FindModel("custom-test", "custom-model"); err != nil {
		t.Fatalf("custom model missing after SetUserModels: %v", err)
	}
}

func TestLoadUserModelsRegistersModelLevelBaseURLCustomProvider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte(`{
		"providers": {
			"model-base-only": {
				"models": [
					{"id": "m1", "baseUrl": "https://llm.example.com/v1"}
				]
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	models, warnings := LoadUserModelsWithWarnings(path)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if len(models) != 1 {
		t.Fatalf("models = %d, want 1", len(models))
	}
	cfg, ok := CustomProviders()["model-base-only"]
	if !ok {
		t.Fatal("custom provider was not registered")
	}
	if cfg.API != "openai" {
		t.Fatalf("api = %q, want openai", cfg.API)
	}
}

func TestLoadUserModelsReasoningLevelMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte(`{
		"providers": {
			"company-proxy": {
				"baseUrl": "https://llm.example.com/v1",
				"api": "anthropic",
				"models": [{
					"id": "reasoning-model",
					"reasoning": true,
					"reasoningLevelMap": {"minimal": "low", "max": "off"}
				}]
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	models, warnings := LoadUserModelsWithWarnings(path)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if len(models) != 1 {
		t.Fatalf("models = %d, want 1", len(models))
	}
	model := models[0]
	if model.API != "anthropic" {
		t.Fatalf("api = %q, want anthropic", model.API)
	}
	if model.ReasoningLevelMap["minimum"] != "low" {
		t.Fatalf("minimum mapping = %q, want low", model.ReasoningLevelMap["minimum"])
	}
	if mapped, ok := model.ReasoningLevelMap["max"]; !ok || mapped != "" {
		t.Fatalf("max mapping = %q, %v, want explicit removal", mapped, ok)
	}
}

func TestLoadUserModelsWarnsOnInvalidReasoningLevelMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte(`{
		"providers": {
			"custom": {
				"models": [{
					"id": "bad-map",
					"reasoning": true,
					"reasoningLevelMap": {"turbo": "high", "low": "extreme"}
				}]
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	models, warnings := LoadUserModelsWithWarnings(path)
	if len(models) != 1 || len(models[0].ReasoningLevelMap) != 0 {
		t.Fatalf("invalid map was not discarded: %#v", models)
	}
	if len(warnings) != 2 || !strings.Contains(strings.Join(warnings, "\n"), "reasoningLevelMap") {
		t.Fatalf("warnings = %v, want two reasoningLevelMap warnings", warnings)
	}
}

func TestLoadUserModelsAcceptsOpenAIResponsesAPI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte(`{
		"providers": {
			"company-proxy": {
				"baseUrl": "https://llm.example.com/v1",
				"api": "openai-responses",
				"models": [{"id": "reasoning-model"}]
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, warnings := LoadUserModelsWithWarnings(path)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if cfg := CustomProviders()["company-proxy"]; cfg.API != APIResponses {
		t.Fatalf("api = %q, want %q", cfg.API, APIResponses)
	}
}

func TestLoadUserModelsWarnsOnUnknownAPI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte(`{
		"providers": {
			"bad-api": {
				"baseUrl": "https://llm.example.com/v1",
				"api": "anthropic-message",
				"models": [{"id": "m1"}]
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, warnings := LoadUserModelsWithWarnings(path)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "unknown api") {
		t.Fatalf("warnings = %v, want unknown api warning", warnings)
	}
	if cfg := CustomProviders()["bad-api"]; cfg.API != "openai" {
		t.Fatalf("api = %q, want openai", cfg.API)
	}
}

func TestLoadUserModelsKeylessAuthRequiresEndpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{"providers":{
		"keyless":{"baseUrl":"http://localhost:8888/v1","auth":"none","models":[{"id":"m"}]},
		"unknown":{"baseUrl":"http://localhost:9999/v1","auth":"typo","models":[{"id":"m"}]},
		"no-url":{"auth":"none","models":[{"id":"m"}]}
	}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, warnings := LoadUserModelsWithWarnings(path)
	if !CustomProviders()["keyless"].NoAuth || CustomProviders()["unknown"].NoAuth || CustomProviders()["no-url"].NoAuth {
		t.Fatalf("invalid keyless configuration: %+v", CustomProviders())
	}
	if len(warnings) != 2 || !strings.Contains(strings.Join(warnings, "\n"), "unknown auth") || !strings.Contains(strings.Join(warnings, "\n"), "without a baseUrl") {
		t.Fatalf("warnings = %v, want unknown auth and missing URL warnings", warnings)
	}
}

func TestLoadUserModelsDiscoverFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte(`{
		"providers": {
			"m4": {
				"baseUrl": "http://127.0.0.1:48000/v1",
				"discover": true
			},
			"m5": {
				"baseUrl": "http://127.0.0.1:48001/v1",
				"models": [{"id": "pinned"}]
			},
			"no-base": {
				"discover": true,
				"models": [{"id": "m1", "baseUrl": "http://127.0.0.1:48002/v1"}]
			}
		}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, warnings := LoadUserModelsWithWarnings(path)
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"no-base"`) || !strings.Contains(warnings[0], "discover") {
		t.Fatalf("warnings = %v, want one discover warning for no-base", warnings)
	}
	cps := CustomProviders()
	if !cps["m4"].Discover {
		t.Fatal("m4 discovery not enabled")
	}
	if cps["m5"].Discover {
		t.Fatal("m5 discovery enabled without flag")
	}
	if cfg, ok := cps["no-base"]; !ok || cfg.Discover {
		t.Fatalf("no-base = %+v, %v; want registered without discovery", cfg, ok)
	}
}
