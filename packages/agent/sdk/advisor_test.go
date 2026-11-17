package sdk

import (
	"path/filepath"
	"testing"

	"github.com/bnema/zut/packages/agent"
	"github.com/bnema/zut/packages/agent/tools"
)

// Embedders never inherit the user's advisor config, client or native.
func TestRuntimeExcludesAdvisor(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ZUT_HOME", filepath.Join(root, "zut"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	if err := agent.SaveConfig(agent.Config{Advisor: &agent.AdvisorConfig{Enabled: true, Model: "anthropic/claude-opus-4-7:high"}}); err != nil {
		t.Fatal(err)
	}
	runtime, err := New(Config{Provider: "anthropic", Model: "claude-sonnet-4-6", APIKey: "synthetic-test-key", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if adv := runtime.agent.ServerAdvisor(); adv != nil {
		t.Fatalf("server advisor = %+v", adv)
	}
	if _, ok := runtime.agent.ToolsSnapshot()[tools.AdvisorToolName]; ok {
		t.Fatal("client advisor tool registered")
	}
}
