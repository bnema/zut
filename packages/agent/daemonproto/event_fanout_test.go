package daemonproto

import (
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
)

// BenchmarkInfoFanoutSeven measures encoding work in the publish/ring and
// seven subscriber notification paths, excluding transport scheduling and IO.
func BenchmarkInfoFanoutSeven(b *testing.B) {
	residents := make([]session.ResidentSummary, 100)
	for i := range residents {
		residents[i] = session.ResidentSummary{ID: fmt.Sprintf("child-%d", i), State: "running", Task: "a representative worker task"}
	}
	ev := session.Event{Session: "session", Seq: 42, Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{Residents: residents}}}
	b.ReportAllocs()
	for b.Loop() {
		raw, err := (EventEncoder{}).EncodeNotification(ev)
		if err != nil {
			b.Fatal(err)
		}
		// Ring accounts len(raw); every subscriber validates and frames shared bytes.
		for range 7 {
			var envelope Envelope
			if err := json.Unmarshal(raw, &envelope); err != nil {
				b.Fatal(err)
			}
			framed := make([]byte, len(raw)+1)
			copy(framed, raw)
			framed[len(raw)] = '\n'
			if _, err := io.Discard.Write(framed); err != nil {
				b.Fatal(err)
			}
		}
	}
}
