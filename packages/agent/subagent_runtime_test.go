package agent

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

func TestResidentChildSpecSnapshotsCurrentProviderTransportSettings(t *testing.T) {
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args: Args{}, Root: t.TempDir(), RepoRoot: t.TempDir(),
		Provider: "openai", Model: "gpt-5.6-sol", BaseURL: "https://old.example/v1", InsecureTLS: false,
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	runtime.SetProviderSettings("https://current.example/v1", true)
	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{Task: "review"}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if spec.BaseURL != "https://current.example/v1" || !spec.InsecureTLS {
		t.Fatalf("resident spec transport = %q insecure=%t", spec.BaseURL, spec.InsecureTLS)
	}
}

func TestResidentChildSpecDoesNotCarryTransportAcrossProviderOverride(t *testing.T) {
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Args: Args{}, Root: t.TempDir(), RepoRoot: t.TempDir(),
		Provider: "openai", Model: "gpt-5.6-sol", BaseURL: "https://parent.example/v1", InsecureTLS: true,
	})
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	spec, err := runtime.buildResidentChildSpec(context.Background(), tools.ResidentSpawnRequest{
		Task: "review", Provider: "anthropic", Model: "claude-sonnet-4-5",
	}, core.Registry{"read": nil})
	if err != nil {
		t.Fatal(err)
	}
	if spec.BaseURL != "" || spec.InsecureTLS {
		t.Fatalf("cross-provider spec inherited transport: %#v", spec)
	}
}
