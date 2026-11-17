package modes

import (
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestHandleEventExternalUsageKeepsContext(t *testing.T) {
	interactive := NewInteractive(InteractiveConfig{})
	interactive.handleEvent(core.EvUsage{Usage: provider.Usage{InputTokens: 450}})
	cum := provider.Usage{InputTokens: 490, CostUSD: 0.25}
	interactive.handleEvent(core.EvUsage{Usage: provider.Usage{InputTokens: 40, CostUSD: 0.25}, Cumulative: cum, External: true})
	if interactive.lastCtxInput != 450 {
		t.Fatalf("context usage = %d, want 450", interactive.lastCtxInput)
	}
	if interactive.cumUsage != cum {
		t.Fatalf("cumulative = %#v", interactive.cumUsage)
	}
}

func TestPrintRendererSumsExternalUsage(t *testing.T) {
	r := NewPrintRenderer(nil)
	r.Handle(core.EvUsage{Usage: provider.Usage{InputTokens: 10, CostUSD: 1}})
	r.Handle(core.EvUsage{Usage: provider.Usage{InputTokens: 40, CostUSD: 0.25}, External: true})
	if got := r.Usage(); got.InputTokens != 50 || got.CostUSD != 1.25 {
		t.Fatalf("usage = %#v", got)
	}
}

func TestEventToJSONExternalUsage(t *testing.T) {
	if _, ok := EventToJSON(core.EvUsage{})["external"]; ok {
		t.Fatal("external present for model usage")
	}
	if got := EventToJSON(core.EvUsage{External: true})["external"]; got != true {
		t.Fatalf("external = %#v", got)
	}
}
