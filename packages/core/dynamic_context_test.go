package core

import (
	"context"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestDynamicContextComposesHostAndExtensionContext(t *testing.T) {
	agent := NewAgent(&compactLifecycleClient{}, "test-model", "system", Registry{})
	agent.HostContext = func(context.Context) string { return "host inventory" }
	agent.BeforeTurnContext = func(context.Context, int) (bool, string, string) {
		return true, "", "extension data"
	}
	if err := agent.Prompt(context.Background(), "user message", nil, nil); err != nil {
		t.Fatal(err)
	}
	messages := agent.Messages()
	if len(messages) < 2 {
		t.Fatalf("messages = %#v", messages)
	}
	text, ok := messages[0].Content[0].(provider.TextBlock)
	if !ok {
		t.Fatalf("context content = %T", messages[0].Content[0])
	}
	for _, want := range []string{"host inventory", "[Extension context]", "extension data"} {
		if !strings.Contains(text.Text, want) {
			t.Fatalf("dynamic context missing %q: %q", want, text.Text)
		}
	}
}

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

	opened, messages, err := OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if len(messages) < 2 || messages[0].Role != provider.RoleDeveloper || messages[1].Role != provider.RoleUser {
		t.Fatalf("restored messages = %#v, want developer context followed by accepted user message", messages)
	}
}
