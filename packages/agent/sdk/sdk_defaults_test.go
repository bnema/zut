package sdk

import "testing"

func TestRuntimeLeavesAgentStepsUnlimitedByDefault(t *testing.T) {
	runtime, err := New(Config{
		Provider: "openai",
		Model:    "gpt-5",
		APIKey:   "synthetic-test-key",
		NoTools:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if runtime.agent.MaxSteps != 0 {
		t.Fatalf("max steps = %d, want unlimited", runtime.agent.MaxSteps)
	}
}
