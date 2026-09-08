package core

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestSessionMessageOriginsAcrossSwitch(t *testing.T) {
	session, err := NewSession(t.TempDir(), t.TempDir(), "opencode", "model-a", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	msg := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.ReasoningBlock{ID: "rs_a:rs_b", Encrypted: "opaque"}, provider.TextBlock{Text: "answer"}}}
	if err := session.AppendMessage(msg); err != nil {
		t.Fatal(err)
	}
	if err := session.UpdateModel("openai-codex", "model-b"); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(msg); err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadSessionSnapshot(session.Path)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"opencode", "openai-codex"} {
		if got := snapshot.Messages[i].Meta["provider"]; got != want {
			t.Fatalf("message %d origin = %q, want %q", i, got, want)
		}
	}
	if err := session.AppendCompaction(snapshot.Messages); err != nil {
		t.Fatal(err)
	}
	if err := session.UpdateModel("google", "model-c"); err != nil {
		t.Fatal(err)
	}
	snapshot, err = ReadSessionSnapshot(session.Path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Messages[0].Meta["provider"] != "opencode" || snapshot.Messages[1].Meta["model"] != "model-b" {
		t.Fatalf("compaction lost origins: %+v", snapshot.Messages)
	}
}

func TestAgentRecordsMessageOriginAcrossModelSwitch(t *testing.T) {
	client := &identityRecordingClient{}
	agent := NewAgent(client, "model-a", "system", Registry{})
	if err := agent.Prompt(context.Background(), "first", nil, nil); err != nil {
		t.Fatal(err)
	}
	agent.Model = "model-b"
	if err := agent.Prompt(context.Background(), "second", nil, nil); err != nil {
		t.Fatal(err)
	}
	var models []string
	for _, msg := range agent.Messages() {
		if msg.Role == provider.RoleAssistant {
			if msg.Meta["provider"] != client.Name() {
				t.Fatal("missing provider origin")
			}
			models = append(models, msg.Meta["model"])
		}
	}
	if len(models) != 2 || models[0] != "model-a" || models[1] != "model-b" {
		t.Fatalf("origins = %v", models)
	}
}
