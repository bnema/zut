package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestParseArgsRejectsLegacyFastModeChildFlags(t *testing.T) {
	if _, err := ParseArgs([]string{"--fast-mode"}); err == nil {
		t.Fatal("ParseArgs accepted removed --fast-mode child flag")
	}
	if _, err := ParseArgs([]string{"--no-fast-mode"}); err == nil {
		t.Fatal("ParseArgs accepted removed --no-fast-mode child flag")
	}
}

func TestResolveFastModeDefaultsOff(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := SaveConfig(Config{Provider: "openai", Model: "gpt-5"}); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{}, false)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.FastMode {
		t.Fatal("FastMode = true by default, want false")
	}
}

func TestResolveFastModeFromConfig(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	enabled := true
	if err := SaveConfig(Config{Provider: "openai", Model: "gpt-5", FastMode: &enabled}); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{}, false)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if !r.FastMode {
		t.Fatal("FastMode = false, want true from config")
	}
}

func TestResolveFastModeRemainsEnabledForUnsupportedProviderUntilRequest(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	enabled := true
	if err := SaveConfig(Config{Provider: "anthropic", Model: "claude-sonnet-4-5", FastMode: &enabled}); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{}, false)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if !r.FastMode {
		t.Fatal("FastMode = false, want true from config")
	}
}

func TestResolveFastModeRequiresExplicitChildOverride(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := SaveConfig(Config{Provider: "openai", Model: "gpt-5"}); err != nil {
		t.Fatal(err)
	}

	r, err := Resolve(Args{FastMode: true}, false)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.FastMode {
		t.Fatal("FastMode = true, want unmarked programmatic value to be ignored")
	}
}

func TestResolveFastModeDurableChildDisableOverridesConfig(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	enabled := true
	if err := SaveConfig(Config{Provider: "openai", Model: "gpt-5", FastMode: &enabled}); err != nil {
		t.Fatal(err)
	}
	r, err := Resolve(Args{FastModeSet: true}, false)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if r.FastMode {
		t.Fatal("FastMode = true, want explicit child disable to override config")
	}
}

func TestConfigSettingsStorePersistsFastMode(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := SaveConfig(Config{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}

	if err := (configSettingsStore{}).SetFastMode(true); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FastMode == nil || !*cfg.FastMode {
		t.Fatalf("fast_mode = %v, want true", cfg.FastMode)
	}
	if cfg.Theme != "dark" {
		t.Fatalf("unrelated config changed: theme = %q, want dark", cfg.Theme)
	}
}

func TestFastModeConfigJSONUsesSnakeCase(t *testing.T) {
	enabled := true
	data, err := json.Marshal(Config{FastMode: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"fast_mode":true`) {
		t.Fatalf("config JSON = %s, want fast_mode", data)
	}
}

func TestResolvedOllamaClientRejectsFastMode(t *testing.T) {
	client := (Resolved{
		Provider:   "ollama",
		Credential: "ollama",
		BaseURL:    "http://localhost:11434",
	}).NewClient()
	if client.Name() != "ollama" {
		t.Fatalf("client name = %q, want ollama", client.Name())
	}
	_, err := client.Stream(context.Background(), provider.Request{Model: "local", FastMode: true})
	if err == nil || !strings.Contains(err.Error(), "only supported for OpenAI providers") {
		t.Fatalf("Stream error = %v, want unsupported-provider error", err)
	}
}
