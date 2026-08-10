package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
)

func TestSubagentPolicyParsesStartupTimeout(t *testing.T) {
	policy := subagentPolicyFromConfig(SubagentsConfig{StartupTimeout: "42s"})
	if got := policy.StartupTimeout; got != 42*time.Second {
		t.Fatalf("StartupTimeout = %s, want 42s", got)
	}
}

func TestSubagentPolicyIgnoresLegacyMaxTotalSpawned(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := os.WriteFile(ConfigPath(), []byte(`{"subagents":{"max_concurrent":1,"max_total_spawned":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Subagents.MaxTotalSpawned; got != 1 {
		t.Fatalf("legacy max_total_spawned = %d, want 1", got)
	}

	supervisor := subagents.New(subagents.Config{
		Root:     t.TempDir(),
		RepoRoot: t.TempDir(),
		Policy:   subagentPolicyFromConfig(cfg.Subagents),
		NewRunner: func(*subagents.Agent) subagents.Runner {
			return subagents.RunnerFunc(func(context.Context, subagents.Sink) error { return nil })
		},
	})
	t.Cleanup(supervisor.StopAll)

	for _, task := range []string{"first", "second"} {
		agent, err := supervisor.Spawn(context.Background(), task)
		if err != nil {
			t.Fatalf("Spawn(%q): %v", task, err)
		}
		agent.Wait()
		if got := agent.Status(); got != subagents.StatusDone {
			t.Fatalf("%s status = %s, want %s", task, got, subagents.StatusDone)
		}
	}
}
