package subagents

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestResidentManagerSnapshotPageAndLookupAreBounded(t *testing.T) {
	manager := NewResidentManager(t.TempDir(), func(ResidentChildSpec, *ResidentJournal) (ResidentRuntime, error) {
		return ResidentTurnRunner(func(context.Context, string) error { return nil }), nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	for i := 0; i < 5; i++ {
		spec := ResidentChildSpec{ID: fmt.Sprintf("child-%d", i), SessionID: fmt.Sprintf("session-%d", i), Provider: "openai", Model: "gpt-5"}
		if _, err := manager.Spawn(context.Background(), spec, "task"); err != nil {
			t.Fatal(err)
		}
	}
	page, total := manager.SnapshotPage(2, 2)
	if total != 5 || len(page) != 2 || page[0].ID != "child-2" || page[1].ID != "child-3" {
		t.Fatalf("page/total = %#v/%d", page, total)
	}
	snapshot, ok := manager.SnapshotFor("child-4")
	if !ok || snapshot.ID != "child-4" {
		t.Fatalf("single snapshot = %#v, %t", snapshot, ok)
	}
}

func TestResidentManagerRecentSnapshotPageOrdersLatestTransitionFirst(t *testing.T) {
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	manager := &ResidentManager{children: map[string]*ResidentChild{}, recovered: map[string]ResidentSnapshot{
		"older": {ID: "older", State: ResidentIdle, UpdatedAt: now.Add(-time.Hour)},
		"newer": {ID: "newer", State: ResidentRunning, UpdatedAt: now.Add(-time.Minute)},
	}, recoveredSpec: map[string]ResidentChildSpec{}, pending: map[string]struct{}{}}
	page, total := manager.RecentSnapshotPage(0, 1)
	if total != 2 || len(page) != 1 || page[0].ID != "newer" {
		t.Fatalf("recent page = %#v/%d", page, total)
	}
}

func TestResidentManagerActiveSnapshotPageSkipsForeignActiveChildren(t *testing.T) {
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC)
	manager := &ResidentManager{children: map[string]*ResidentChild{}, recovered: map[string]ResidentSnapshot{
		"foreign":     {ID: "foreign", State: ResidentRunning, OwnedElsewhere: true, UpdatedAt: now},
		"local-newer": {ID: "local-newer", State: ResidentRunning, UpdatedAt: now.Add(-time.Minute)},
		"local-older": {ID: "local-older", State: ResidentQueued, UpdatedAt: now.Add(-time.Hour)},
		"idle":        {ID: "idle", State: ResidentIdle, UpdatedAt: now.Add(time.Minute)},
	}, recoveredSpec: map[string]ResidentChildSpec{}, pending: map[string]struct{}{}}
	page, total := manager.ActiveSnapshotPage(1)
	if total != 2 || len(page) != 1 || page[0].ID != "local-newer" {
		t.Fatalf("active page = %#v/%d, want bounded same-host active children", page, total)
	}
}

func TestResidentManagerRecentSnapshotPageHugeLimitDoesNotOverflow(t *testing.T) {
	manager := &ResidentManager{children: map[string]*ResidentChild{}, recovered: map[string]ResidentSnapshot{
		"a": {ID: "a"}, "b": {ID: "b"},
	}, recoveredSpec: map[string]ResidentChildSpec{}, pending: map[string]struct{}{}}
	page, total := manager.RecentSnapshotPage(1, math.MaxInt)
	if total != 2 || len(page) != 1 {
		t.Fatalf("page/total = %#v/%d", page, total)
	}
}

func TestResidentManagerHistoryPageRejectsTraversalChildID(t *testing.T) {
	manager := NewResidentManager(t.TempDir(), nil)
	for _, id := range []string{"", ".", "..", "../escape", "a/b", "/etc"} {
		if _, err := manager.HistoryPage(id, "", 10); err == nil {
			t.Fatalf("HistoryPage(%q) succeeded, want invalid child ID", id)
		}
	}
}

// HistoryRev counts finalized transcript appends only; streaming deltas leave
// it unchanged so readers can skip history reloads.
func TestResidentSnapshotHistoryRevCountsFinalizedEventsOnly(t *testing.T) {
	recorded := make(chan uint64, 1)
	var manager *ResidentManager
	manager = NewResidentManager(t.TempDir(), func(_ ResidentChildSpec, journal *ResidentJournal) (ResidentRuntime, error) {
		return ResidentTurnRunner(func(context.Context, string) error {
			snapshot, _ := manager.SnapshotFor("child")
			before := snapshot.HistoryRev
			if err := journal.RecordAgentEvent(core.EvTextDelta{Delta: "partial"}); err != nil {
				return err
			}
			if snapshot, _ = manager.SnapshotFor("child"); snapshot.HistoryRev != before {
				return fmt.Errorf("delta bumped history rev %d -> %d", before, snapshot.HistoryRev)
			}
			if err := journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done"}}}}); err != nil {
				return err
			}
			snapshot, _ = manager.SnapshotFor("child")
			recorded <- snapshot.HistoryRev - before
			return nil
		}), nil
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	if _, err := manager.Spawn(context.Background(), ResidentChildSpec{ID: "child", SessionID: "session", Provider: "openai", Model: "gpt-5"}, "task"); err != nil {
		t.Fatal(err)
	}
	select {
	case delta := <-recorded:
		if delta != 1 {
			t.Fatalf("finalized message bumped history rev by %d, want 1", delta)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resident turn did not run")
	}
}
