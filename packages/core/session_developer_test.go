package core

import (
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestOpenSessionHydratesDeveloperMessage(t *testing.T) {
	session, err := NewSession(t.TempDir(), "/workspace", "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{
		Role:    provider.RoleDeveloper,
		Content: []provider.Content{provider.TextBlock{Text: "host context"}},
	}); err != nil {
		t.Fatal(err)
	}
	path := session.Path
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	_, messages, err := OpenSession(path)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if len(messages) != 1 || messages[0].Role != provider.RoleDeveloper {
		t.Fatalf("messages = %#v, want one developer message", messages)
	}
}
