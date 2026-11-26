package subagents

import (
	"context"
	"path/filepath"
	"testing"
)

// A Spawn that loses the race with Close has already reported acceptance to
// the host, so it must also report the initial turn's completion: otherwise
// the host tracks a turn that never ends.
func TestResidentSpawnClosedAfterAcceptanceReportsCompletion(t *testing.T) {
	var manager *ResidentManager
	factory := func(ResidentChildSpec, *ResidentJournal) (ResidentRuntime, error) {
		// Close lands between acceptance and child registration.
		if err := manager.Close(context.Background()); err != nil {
			t.Errorf("close manager: %v", err)
		}
		return ResidentTurnRunner(func(context.Context, string) error {
			t.Error("closed manager started the initial turn")
			return nil
		}), nil
	}
	root := t.TempDir()
	manager = NewResidentManager(root, factory)
	var accepted []string
	manager.SetAcceptedObserver(func(spec ResidentChildSpec, turnID, _ string) { accepted = append(accepted, spec.ID+"/"+turnID) })
	var completions []ResidentCompletion
	manager.SetCompletionObserver(func(c ResidentCompletion) bool { completions = append(completions, c); return true })

	spec := ResidentChildSpec{ID: "late", SessionID: "session", InitialTurnID: "initial", Provider: "openai", Model: "test"}
	if _, err := manager.Spawn(t.Context(), spec, "task"); err == nil {
		t.Fatal("spawn on a closed manager succeeded")
	}
	if len(accepted) != 1 || accepted[0] != "late/initial" {
		t.Fatalf("accepted = %v, want late/initial", accepted)
	}
	if len(completions) != 1 {
		t.Fatalf("completions = %d, want one for the accepted turn", len(completions))
	}
	got := completions[0].Completion()
	if got.AgentID != "late" || got.TurnID != "initial" || got.Status != CompletionDropped || got.Error == "" {
		t.Fatalf("completion = %#v, want late/initial dropped with its cause", got)
	}
	records, err := ReadResidentJournal(filepath.Join(root, spec.ID, residentTranscriptName))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("journal has no records")
	}
	last := records[len(records)-1]
	if last.Type != residentRecordInterrupted || last.TurnID != "initial" {
		t.Fatalf("last journal record = %s turn %q, want the initial turn interrupted", last.Type, last.TurnID)
	}
}
