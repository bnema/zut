package daemonproto_test

import (
	"encoding/json"
	"testing"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestExternalUsageRoundTrip(t *testing.T) {
	ev := core.EvUsage{Usage: provider.Usage{InputTokens: 4, CostUSD: 0.5}, Cumulative: provider.Usage{CostUSD: 1}, External: true}
	kind, raw, err := daemonproto.EncodeAgentEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["external"] != true {
		t.Fatalf("projection external = %#v", wire["external"])
	}
	got, err := daemonproto.DecodeAgentEvent(kind, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != ev {
		t.Fatalf("round trip = %#v", got)
	}
	_, raw, err = daemonproto.EncodeAgentEvent(core.EvUsage{})
	if err != nil {
		t.Fatal(err)
	}
	wire = nil
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["external"]; ok {
		t.Fatal("external present for model usage")
	}
}
