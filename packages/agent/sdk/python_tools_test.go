package sdk

import (
	"testing"
)

func TestPythonFollowsNormalRegistryBehavior(t *testing.T) {
	base := Config{Provider: "openai", Model: "gpt-5", APIKey: "synthetic-test-key"}

	runtime, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if _, ok := runtime.agent.ToolsSnapshot()["python"]; !ok {
		t.Fatal("default SDK runtime must expose python without bypassing tool selection")
	}

	selected, err := New(Config{Provider: "openai", Model: "gpt-5", APIKey: "synthetic-test-key", Tools: []string{"bash"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = selected.Close() })
	if _, ok := selected.agent.ToolsSnapshot()["python"]; ok {
		t.Fatal("SDK tool selection without python must not expose python")
	}

	included, err := New(Config{Provider: "openai", Model: "gpt-5", APIKey: "synthetic-test-key", Tools: []string{"bash", "python"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = included.Close() })
	if _, ok := included.agent.ToolsSnapshot()["python"]; !ok {
		t.Fatal("SDK tool selection including python must expose python")
	}
}
