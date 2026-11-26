package daemonproto_test

import (
	"encoding/json"
	"testing"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestAdvisorNoteUserMessageRoundTrip(t *testing.T) {
	ev := core.EvUserMessage{Message: provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "[advisor note]\ncheck lexer.go"}},
		Meta:    map[string]string{core.WatcherNoteMetaKey: "true"},
	}}
	kind, raw, err := daemonproto.EncodeAgentEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["advisor_note"] != true {
		t.Fatalf("projection advisor_note = %#v", wire["advisor_note"])
	}
	got, err := daemonproto.DecodeAgentEvent(kind, raw)
	if err != nil {
		t.Fatal(err)
	}
	msg := got.(core.EvUserMessage).Message
	if msg.Meta[core.WatcherNoteMetaKey] != "true" {
		t.Fatalf("decoded meta = %#v", msg.Meta)
	}
}

func TestLegacyAdvisorReviewIsUnknown(t *testing.T) {
	ev, err := daemonproto.DecodeAgentEvent("advisor_review", json.RawMessage(`{"type":"advisor_review","value":{"Kind":"change","Phase":"start"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev == nil || ev.Type() != "advisor_review" {
		t.Fatalf("unknown event = %#v", ev)
	}
}
