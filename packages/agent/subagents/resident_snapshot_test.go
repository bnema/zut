package subagents

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestResidentManagerSnapshotPageAndLookupAreBounded(t *testing.T) {
	manager := NewResidentManager(t.TempDir(), func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
		return func(context.Context, string) error { return nil }, nil
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
