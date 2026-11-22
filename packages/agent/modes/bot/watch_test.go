package bot

import (
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestTurnTrackerDoesNotMirrorAdvisorNotes(t *testing.T) {
	var tr turnTracker
	user := provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "fix it"}}}
	note := provider.Message{Role: provider.RoleUser, Meta: map[string]string{core.WatcherNoteMetaKey: "true"},
		Content: []provider.Content{provider.TextBlock{Text: "[advisor note]\ncheck lexer.go"}}}
	tr.apply(session.Event{Kind: session.EvAgent, Agent: core.EvUserMessage{Message: user}})
	tr.apply(session.Event{Kind: session.EvAgent, Agent: core.EvUserMessage{Message: note}})
	if len(tr.prompts) != 1 || tr.prompts[0] != "fix it" {
		t.Fatalf("prompts = %q, want only the user's", tr.prompts)
	}
}
