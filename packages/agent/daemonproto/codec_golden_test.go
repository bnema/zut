package daemonproto_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func equalJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	return reflect.DeepEqual(decodeJSONNumbers(t, a), decodeJSONNumbers(t, b))
}

// decodeJSONNumbers keeps numbers as json.Number so large integers compare exactly.
func decodeJSONNumbers(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestEqualJSONDistinguishesLargeIntegers(t *testing.T) {
	if equalJSON(t, []byte(`{"n":9007199254740993}`), []byte(`{"n":9007199254740992}`)) {
		t.Fatal("adjacent large integers compared equal")
	}
}

func largeInfoEvent() session.Event {
	info := session.Info{ID: "synthetic-session", Title: "Synthetic benchmark", CWD: "/workspace", Provider: "test", Model: "test-model", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	for i := 0; i < 100; i++ {
		info.Residents = append(info.Residents, session.ResidentSummary{ID: fmt.Sprintf("resident-%03d", i), Profile: "worker", State: "idle", Workspace: "/workspace", Task: "Inspect synthetic source and report findings", UpdatedAt: info.CreatedAt, Usage: provider.Usage{InputTokens: 1000, OutputTokens: 100}})
	}
	return session.Event{Session: "s", Seq: 42, Kind: session.EvInfo, Data: session.InfoEvent{Info: info}}
}

func TestEncodeEventGolden(t *testing.T) {
	events := []session.Event{}
	for _, ev := range eventCases() {
		events = append(events, session.Event{Session: "s", Seq: 42, Kind: session.EvAgent, Agent: ev})
	}
	for _, p := range sessionPayloadCases() {
		events = append(events, session.Event{Session: "s", Seq: 42, Kind: p.k, Data: p.v})
	}
	events = append(events, largeInfoEvent())
	d := -1234567 * time.Nanosecond
	type nested struct {
		Duration *time.Duration `json:"duration"`
		Values   [][]any        `json:"values"`
		Nil      *string        `json:"nil"`
	}
	events = append(events, session.Event{Kind: session.EvNote, Data: &nested{Duration: &d, Values: [][]any{{nil, provider.ReasoningBlock{}, &provider.ToolTiming{Duration: 1234567}, json.RawMessage(`{"z":1,"a":2}`)}, nil, {}}}})
	events = append(events, session.Event{Kind: session.EvNote, Data: (*nested)(nil)})
	events = append(events, session.Event{Kind: session.EvNote, Data: provider.ToolResultBlock{CallID: "c", Timing: &provider.ToolTiming{Duration: d}, Content: []provider.Content{provider.TextBlock{Text: "nested"}, provider.ToolResultBlock{CallID: "inner", Content: nil}}}})
	got := make([]json.RawMessage, 0, len(events))
	for _, e := range events {
		n, err := daemonproto.EncodeEvent(e)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, raw)
	}
	raw, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	const path = "testdata/encode_event.golden.json"
	if os.Getenv("UPDATE_DAEMONPROTO_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !equalJSON(t, want, raw) {
		t.Fatalf("encoded events differ from current-code golden")
	}
}

func BenchmarkEncodeEvent(b *testing.B) {
	for _, tc := range []struct {
		name  string
		event session.Event
	}{
		{"StreamingDelta", session.Event{Session: "s", Seq: 42, Kind: session.EvAgent, Agent: core.EvTextDelta{Delta: "a typical streamed text fragment"}}},
		{"LargeInfo", largeInfoEvent()},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := daemonproto.EncodeEvent(tc.event); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
