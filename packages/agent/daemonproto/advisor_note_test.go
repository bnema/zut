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

func TestAdvisorReviewRoundTrip(t *testing.T) {
	ev := core.EvAdvisorReview{Kind: "finish", Phase: core.AdvisorReviewStart}
	kind, raw, err := daemonproto.EncodeAgentEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["kind"] != "finish" || wire["phase"] != "start" {
		t.Fatalf("projection = %#v", wire)
	}
	got, err := daemonproto.DecodeAgentEvent(kind, raw)
	if err != nil || got != ev {
		t.Fatalf("round trip = %#v, %v", got, err)
	}
}
