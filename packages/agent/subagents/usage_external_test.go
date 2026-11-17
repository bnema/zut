package subagents

import (
	"path/filepath"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestResidentLiveProjectionIgnoresExternalContext(t *testing.T) {
	projection := newResidentLiveProjection()
	turn := provider.Usage{InputTokens: 1000}
	projection.Apply(core.EvUsage{Usage: turn, Cumulative: turn})
	cum := provider.Usage{InputTokens: 1040, CostUSD: 0.25}
	projection.Apply(core.EvUsage{Usage: provider.Usage{InputTokens: 40, CostUSD: 0.25}, Cumulative: cum, External: true})
	got := projection.Snapshot()
	if got.ContextUsed != 1000 || got.Usage != cum {
		t.Fatalf("context = %d usage = %#v", got.ContextUsed, got.Usage)
	}
}

func TestResidentJournalIgnoresExternalContext(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "external-usage")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "external-usage", SessionID: "usage-session", Provider: "openai", Model: "gpt-test"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	journal.ConfigureUsage(128_000, false)
	turn := provider.Usage{InputTokens: 10_000}
	if err := journal.RecordAgentEvent(core.EvUsage{Usage: turn, Cumulative: turn}); err != nil {
		t.Fatal(err)
	}
	cum := provider.Usage{InputTokens: 10_040, CostUSD: 0.25}
	if err := journal.RecordAgentEvent(core.EvUsage{Usage: provider.Usage{InputTokens: 40, CostUSD: 0.25}, Cumulative: cum, External: true}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	metadata, err := ReconcileResidentJournal(filepath.Join(root, spec.ID))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Usage != cum || metadata.ContextUsed != 10_000 {
		t.Fatalf("usage = %#v context = %d", metadata.Usage, metadata.ContextUsed)
	}
}
