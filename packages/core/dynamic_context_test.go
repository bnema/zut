package core

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestFirstDynamicContextPersistsAsTranscriptReplacement(t *testing.T) {
	client := &compactLifecycleClient{}
	agent := NewAgent(client, "test-model", "system", Registry{})
	session, err := NewSession(t.TempDir(), "/workspace", "test", "test-model", "test")
	if err != nil {
		t.Fatal(err)
	}
	agent.OnMessageAppended = func(message provider.Message) {
		if err := session.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	agent.OnTranscriptCompacted = func(messages []provider.Message) {
		if err := session.AppendCompaction(messages); err != nil {
			t.Fatal(err)
		}
	}
	agent.BeforeTurnContext = func(context.Context, int) (bool, string, string) {
		return true, "", "accepted extension context"
	}
	if err := agent.Prompt(context.Background(), "accepted user message", nil, nil); err != nil {
		t.Fatal(err)
	}
	path := session.Path
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	_, messages, err := OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) < 2 || messages[0].Role != provider.RoleDeveloper || messages[1].Role != provider.RoleUser {
		t.Fatalf("restored messages = %#v, want developer context followed by accepted user message", messages)
	}
}
