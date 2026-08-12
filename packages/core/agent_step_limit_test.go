package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

type loopingToolClient struct {
	calls int32
}

func (c *loopingToolClient) Name() string { return "looping-tool" }

func (c *loopingToolClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	call := atomic.AddInt32(&c.calls, 1)
	callID := fmt.Sprintf("call-%d", call)
	out := make(chan provider.Event, 4)
	out <- provider.EventStart{Provider: c.Name(), Model: req.Model}
	out <- provider.EventToolStart{ID: callID, Name: "echo"}
	out <- provider.EventToolEnd{ID: callID}
	out <- provider.EventDone{Stop: provider.StopToolUse, Message: provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.Content{provider.ToolCallBlock{
			ID: callID, Name: "echo", Arguments: json.RawMessage(`{}`),
		}},
	}}
	close(out)
	return out, nil
}

func TestAgentReturnsTypedStepLimitAfterModelLoopBudget(t *testing.T) {
	client := &loopingToolClient{}
	agent := NewAgent(client, "model", "system", Registry{"echo": &recordingTool{}})
	agent.MaxSteps = 2

	err := agent.Prompt(context.Background(), "keep using tools", nil, nil)
	if !errors.Is(err, ErrMaxSteps) {
		t.Fatalf("prompt error = %v, want ErrMaxSteps", err)
	}
	if got := atomic.LoadInt32(&client.calls); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
}
