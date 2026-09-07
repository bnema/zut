package sdk

import (
	"context"
	"errors"
	"testing"
)

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

func TestNewContextWithCanceledContextAndAPIKeyFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewContext(ctx, Config{
		Provider: "openai",
		Model:    "gpt-5",
		APIKey:   "synthetic-test-key",
		NoTools:  true,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
