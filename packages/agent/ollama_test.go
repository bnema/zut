package agent

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/bnema/zut/packages/provider"
	providerauth "github.com/bnema/zut/packages/provider/auth"
)

func TestResolveCredentialOllamaEnv(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OLLAMA_API_KEY", "ollama-cloud-key")

	cred, method, _, err := ResolveCredentialFull("ollama", "")
	if err != nil {
		t.Fatalf("ResolveCredentialFull failed: %v", err)
	}
	if cred != "ollama-cloud-key" {
		t.Errorf("cred = %q, want %q", cred, "ollama-cloud-key")
	}
	if method != "apikey" {
		t.Errorf("method = %q, want apikey", method)
	}
	if !CredentialAvailable("ollama") {
		t.Error("CredentialAvailable(ollama) = false, want true with OLLAMA_API_KEY set")
	}
}

func TestResolveCredentialOllamaExplicitWinsOverEnv(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OLLAMA_API_KEY", "env-key")

	cred, _, _, err := ResolveCredentialFull("ollama", "explicit-key")
	if err != nil {
		t.Fatalf("ResolveCredentialFull failed: %v", err)
	}
	if cred != "explicit-key" {
		t.Errorf("cred = %q, want explicit-key", cred)
	}
}

func TestResolveOllamaCloudDefaultWithKey(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OLLAMA_API_KEY", "ollama-cloud-key")

	resolved, err := Resolve(Args{Provider: "ollama", Model: "gemma4:31b"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Credential != "ollama-cloud-key" {
		t.Errorf("credential = %q, want real key", resolved.Credential)
	}
	if resolved.BaseURL != "https://ollama.com/v1" {
		t.Errorf("baseURL = %q, want https://ollama.com/v1", resolved.BaseURL)
	}
}

func TestResolveOllamaLocalDefaultWithoutKey(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OLLAMA_API_KEY", "")

	resolved, err := Resolve(Args{Provider: "ollama", Model: "qwen3.5:4b"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Credential != "ollama" {
		t.Errorf("credential = %q, want dummy ollama", resolved.Credential)
	}
	if resolved.BaseURL != "http://localhost:11434" {
		t.Errorf("baseURL = %q, want http://localhost:11434", resolved.BaseURL)
	}
}

func TestResolveOllamaExplicitBaseURLWinsOverCloud(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OLLAMA_API_KEY", "ollama-cloud-key")

	resolved, err := Resolve(Args{Provider: "ollama", Model: "qwen3.5:4b", BaseURL: "http://localhost:11434"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.BaseURL != "http://localhost:11434" {
		t.Errorf("baseURL = %q, want explicit local URL", resolved.BaseURL)
	}
	if resolved.Credential != "ollama-cloud-key" {
		t.Errorf("credential = %q, want real key (local server ignores it)", resolved.Credential)
	}
}

func TestResolveOllamaExplicitKeyTriggersCloud(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OLLAMA_API_KEY", "")

	resolved, err := Resolve(Args{Provider: "ollama", Model: "gemma4:31b", APIKey: "explicit-key"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Credential != "explicit-key" {
		t.Errorf("credential = %q, want explicit-key", resolved.Credential)
	}
	if resolved.BaseURL != "https://ollama.com/v1" {
		t.Errorf("baseURL = %q, want https://ollama.com/v1", resolved.BaseURL)
	}
}

func TestResolveOllamaModelBaseURLWinsOverCloud(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OLLAMA_API_KEY", "ollama-cloud-key")
	provider.SetUserModels([]provider.Model{{
		Provider: "ollama", ID: "custom-local", DisplayName: "custom-local",
		ContextWindow: 32768, MaxOutput: 8192,
		BaseURL: "http://mylocal:11434", Source: "user",
	}})
	t.Cleanup(func() { provider.SetUserModels(nil) })

	resolved, err := Resolve(Args{Provider: "ollama", Model: "custom-local"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.BaseURL != "http://mylocal:11434" {
		t.Errorf("baseURL = %q, want model-level URL", resolved.BaseURL)
	}
	if resolved.Credential != "ollama-cloud-key" {
		t.Errorf("credential = %q, want real key", resolved.Credential)
	}
}

func TestResolveOllamaStoredCredTriggersCloud(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OLLAMA_API_KEY", "")
	encoded, err := json.Marshal(providerauth.Credentials{AdditionalAPIKeyCreds: map[string]providerauth.ProviderCreds{
		"ollama": {APIKey: "stored-key"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AuthPath(), encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	cred, _, _, err := ResolveCredentialFull("ollama", "")
	if err != nil {
		t.Fatalf("ResolveCredentialFull failed: %v", err)
	}
	if cred != "stored-key" {
		t.Errorf("cred = %q, want stored-key", cred)
	}

	resolved, err := Resolve(Args{Provider: "ollama", Model: "gemma4:31b"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Credential != "stored-key" {
		t.Errorf("credential = %q, want stored-key", resolved.Credential)
	}
	if resolved.BaseURL != "https://ollama.com/v1" {
		t.Errorf("baseURL = %q, want https://ollama.com/v1", resolved.BaseURL)
	}
}
