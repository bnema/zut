package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
)

func BenchmarkEventRingAppend(b *testing.B) {
	residents := make([]ResidentSummary, 5000)
	for i := range residents {
		residents[i] = ResidentSummary{ID: fmt.Sprintf("child-%05d", i), Profile: "worker", State: "interrupted"}
	}
	for _, tc := range []struct {
		name  string
		event Event
	}{
		{"StreamingDelta", Event{Seq: 1, Kind: EvAgent, Agent: core.EvTextDelta{Delta: "a typical streamed text fragment"}}},
		{"LargeInfo", Event{Seq: 1, Kind: EvInfo, Data: InfoEvent{Info: Info{Residents: residents}}}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			r := new(eventRing)
			b.ReportAllocs()
			for b.Loop() {
				r.append(tc.event)
			}
		})
	}
}

func TestEventRingExactJSONSize(t *testing.T) {
	for _, ev := range []Event{
		{Seq: 1, Kind: EvTitle, Data: TitleEvent{Title: "<>&\n\"界"}},
		{Seq: 2, Kind: EvAgent, At: time.Date(2026, 11, 1, 20, 0, 0, 123, time.UTC), Agent: core.EvTextDelta{Delta: "text"}},
		{Seq: 3, Data: json.RawMessage(`{ "nested": [1, true, null] }`)},
	} {
		encoded, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		r := new(eventRing)
		r.append(ev)
		if r.bytes != len(encoded) || len(r.entries) != 1 || r.entries[0].bytes != len(encoded) {
			t.Fatalf("ring size = %d, want %d", r.bytes, len(encoded))
		}
	}
}

func TestEventRingExactByteBoundary(t *testing.T) {
	ev := Event{Seq: 1, Kind: EvTitle, Data: TitleEvent{}}
	encoded, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	ev.Data = TitleEvent{Title: strings.Repeat("x", eventRingBytes-len(encoded))}
	r := new(eventRing)
	r.append(ev)
	if r.bytes != eventRingBytes || len(r.entries) != 1 {
		t.Fatalf("exact boundary: bytes=%d entries=%d", r.bytes, len(r.entries))
	}
	r.append(Event{Seq: 2})
	if len(r.entries) != 1 || r.entries[0].event.Seq != 2 {
		t.Fatal("byte limit did not evict oldest event")
	}
}

func TestEventRingMarshalErrorIsOversize(t *testing.T) {
	for _, data := range []any{make(chan int), json.RawMessage(`invalid`)} {
		r := new(eventRing)
		r.append(Event{Seq: 1})
		r.append(Event{Seq: 2, Data: data})
		if len(r.entries) != 0 || r.bytes != 0 {
			t.Fatalf("marshal error retained events: bytes=%d entries=%d", r.bytes, len(r.entries))
		}
		r.append(Event{Seq: 3})
		if len(r.entries) != 1 {
			t.Fatal("ring did not recover after marshal error")
		}
	}
}

func TestEventRingCountLimit(t *testing.T) {
	r := new(eventRing)
	for seq := uint64(1); seq <= EventRingSize+1; seq++ {
		r.append(Event{Seq: seq})
	}
	if len(r.entries) != EventRingSize || r.entries[0].event.Seq != 2 {
		t.Fatal("count limit did not evict oldest event")
	}
	total := 0
	for _, entry := range r.entries {
		total += entry.bytes
	}
	if r.bytes != total {
		t.Fatalf("byte accounting after count eviction: got %d, want %d", r.bytes, total)
	}
}
