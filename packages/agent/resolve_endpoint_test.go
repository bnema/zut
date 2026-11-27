package agent

import (
	"context"
	"os"
	"testing"

	"github.com/bnema/zut/packages/agent/config"
	"github.com/bnema/zut/packages/provider"
)

// These pin resolve()'s endpoint behavior shared with the advisor through
// resolveEndpointCredential/resolveEndpoint.

func endpointTestEnv(t *testing.T) string {
	t.Helper()
	project := pythonTestEnv(t)
	if err := os.MkdirAll(config.ZutHome(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"OLLAMA_API_KEY", "OPENAI_API_KEY"} {
		t.Setenv(k, "")
	}
	return project
}

func endpointArgs(project, prov, model string) Args {
	return Args{Provider: prov, Model: model, CWD: project, NoLSP: true, NoSkill: true, NoContextFiles: true}
}

func TestResolveOllamaDefaultEndpoint(t *testing.T) {
	project := endpointTestEnv(t)
	r := resolvePython(t, endpointArgs(project, "ollama", "any-local-model"))
	if r.BaseURL != "http://localhost:11434" || r.Credential != "ollama" {
		t.Fatalf("local: base=%q cred=%q", r.BaseURL, r.Credential)
	}
	t.Setenv("OLLAMA_API_KEY", "synthetic-test-key")
	r = resolvePython(t, endpointArgs(project, "ollama", "any-local-model"))
	if r.BaseURL != provider.OllamaCloudBaseURL || r.Credential != "synthetic-test-key" {
		t.Fatalf("cloud: base=%q", r.BaseURL)
	}
	explicit := endpointArgs(project, "ollama", "any-local-model")
	explicit.BaseURL = "http://127.0.0.1:9"
	if r = resolvePython(t, explicit); r.BaseURL != "http://127.0.0.1:9" {
		t.Fatalf("explicit: base=%q", r.BaseURL)
	}
}

func TestResolveKeylessCustomProvider(t *testing.T) {
	project := endpointTestEnv(t)
	loadTestModelsJSON(t, `{"providers":{
		"unsloth":{"baseUrl":"http://127.0.0.1:9/v1","api":"openai","auth":"none","models":[{"id":"qista"}]},
		"nobase":{"api":"openai","auth":"none","models":[{"id":"m1"}]}}}`)
	r := resolvePython(t, endpointArgs(project, "unsloth", "qista"))
	if r.Credential != "" || !r.credentialOptional || !r.HasCredential() || r.BaseURL != "http://127.0.0.1:9/v1" {
		t.Fatalf("keyless: %+v", r)
	}
	// models.json drops auth "none" without any base URL, so a keyless
	// provider with no endpoint cannot load; it keeps the credential flow.
	if provider.CustomProviders()["nobase"].NoAuth {
		t.Fatal("keyless provider without base URL loaded as keyless")
	}
	// resolveEndpoint still rejects that combination directly.
	ec := endpointCredential{keyless: true}
	_, err := resolveEndpoint(context.Background(), endpointInput{provider: "nobase", getenv: func(string) string { return "" }}, &ec)
	if err == nil || err.Error() != `custom provider "nobase" has auth none but no base URL` {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveInsecureScopedToExplicitEndpoints(t *testing.T) {
	project := endpointTestEnv(t)
	t.Setenv("OPENAI_API_KEY", "synthetic-test-key")
	builtin := endpointArgs(project, "openai", "gpt-5")
	builtin.InsecureTLS = true
	if r := resolvePython(t, builtin); r.InsecureTLS {
		t.Fatal("--insecure applied to a built-in endpoint")
	}
	explicit := builtin
	explicit.BaseURL = "https://127.0.0.1:9/v1"
	if r := resolvePython(t, explicit); !r.InsecureTLS {
		t.Fatal("--insecure not applied to an explicit endpoint")
	}
	explicit.InsecureTLS = false
	if r := resolvePython(t, explicit); r.InsecureTLS {
		t.Fatal("explicit endpoint insecure without --insecure")
	}
}
