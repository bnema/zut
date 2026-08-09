package agent

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestRunOrchestratedPrintEmitsOnlyFinalAnswer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	server := orchestratedModeTestServer("final answer")
	defer server.Close()

	output, runErr := captureTestStdout(t, func() error {
		return runOrchestratedPrintMode(context.Background(), Args{
			Mode:        ModePrint,
			Orchestrate: true,
			Provider:    "openai",
			Model:       "gpt-5",
			BaseURL:     server.URL,
			APIKey:      "test-key",
			Prompt:      "say hello",
		}, "test")
	})
	if runErr != nil {
		t.Fatalf("runOrchestratedPrintMode error: %v", runErr)
	}
	if got := output; got != "final answer\n" {
		t.Fatalf("stdout = %q, want final answer only", got)
	}
}

func TestRunOrchestratedPrintPropagatesConfigLoadError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	if err := os.WriteFile(ConfigPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runOrchestratedPrintMode(context.Background(), Args{
		Mode:        ModePrint,
		Orchestrate: true,
		Provider:    "ollama",
		Model:       "llama3",
		Prompt:      "test",
	}, "test")
	if err == nil || !strings.Contains(err.Error(), "load config for orchestrated print") {
		t.Fatalf("runOrchestratedPrintMode error = %v, want config load error", err)
	}
}
