package subagents

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func BenchmarkResidentManagerReconcile(b *testing.B) {
	root := b.TempDir()
	for i := 0; i < 100; i++ {
		spec := ResidentChildSpec{ID: fmt.Sprintf("child-%04d", i), SessionID: "session", InitialTurnID: "turn", Provider: "test", Model: "test"}
		j, err := OpenResidentJournal(root, spec.ID)
		if err != nil {
			b.Fatal(err)
		}
		if err := j.Accept(spec, "task"); err != nil {
			b.Fatal(err)
		}
		if err := j.RecordTurnStarted(spec, "turn"); err != nil {
			b.Fatal(err)
		}
		for n := 0; n < 8; n++ {
			if err := j.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: strings.Repeat("answer ", 4096)}}}}); err != nil {
				b.Fatal(err)
			}
		}
		if err := j.RecordTurnFinished(spec, "turn", nil); err != nil {
			b.Fatal(err)
		}
		if err := j.Close(); err != nil {
			b.Fatal(err)
		}
		if _, err := ReconcileResidentJournal(filepath.Join(root, spec.ID)); err != nil {
			b.Fatal(err)
		}
	}
	manager := NewResidentManager(root, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if errs := manager.Reconcile(); len(errs) != 0 {
			b.Fatal(errs)
		}
	}
}
