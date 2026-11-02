package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
)

func BenchmarkResidentSummariesRecovered(b *testing.B) {
	root := b.TempDir()
	for i := 0; i < 5000; i++ {
		id := fmt.Sprintf("child-%05d", i)
		j, err := subagents.OpenResidentJournal(root, id)
		if err != nil {
			b.Fatal(err)
		}
		if err := j.Accept(subagents.ResidentChildSpec{ID: id, SessionID: id, InitialTurnID: id, Profile: "worker", Provider: "test", Model: "test"}, "task"); err != nil {
			b.Fatal(err)
		}
		if err := j.Close(); err != nil {
			b.Fatal(err)
		}
	}
	m := subagents.NewResidentManager(root, nil)
	if errs := m.Reconcile(); len(errs) != 0 {
		b.Fatal(errs)
	}
	c := &Controller{deps: Deps{ResidentManager: m}}
	b.ReportAllocs()
	for b.Loop() {
		c.residentSummaries()
	}
}

func TestResidentSummariesCache(t *testing.T) {
	m := newMockResidentHostLocal(t)
	var revision uint64
	m.EXPECT().Revision().RunAndReturn(func() uint64 { return revision })
	c := &Controller{deps: Deps{ResidentManager: m}}
	c.res.specs = make(map[string]subagents.ResidentChildSpec)
	c.res.tasks = make(map[string]string)
	rows := []subagents.ResidentSnapshot{{ID: "a", Profile: "worker", State: subagents.ResidentRunning}, {ID: "foreign", OwnedElsewhere: true}}
	m.EXPECT().Snapshot().Return(rows).Once()
	first := c.residentSummaries()
	if len(first) != 1 || first[0].ID != "a" {
		t.Fatalf("summaries = %#v", first)
	}
	if &first[0] != &c.residentSummaries()[0] {
		t.Fatal("unchanged revision rebuilt summaries")
	}
	// A local acceptance changes workspace/task without a manager mutation.
	c.res.specs["a"] = subagents.ResidentChildSpec{Workspace: "/workspace"}
	c.res.tasks["a"] = "task"
	c.res.inputsRevision++
	m.EXPECT().Snapshot().Return(rows).Once()
	second := c.residentSummaries()
	if second[0].Workspace != "/workspace" || second[0].Task != "task" {
		t.Fatalf("stale local inputs: %#v", second)
	}
	if first[0].Workspace != "" {
		t.Fatal("rebuild mutated published summaries")
	}
	revision++
	rows[0].State = subagents.ResidentIdle
	m.EXPECT().Snapshot().Return(rows).Once()
	third := c.residentSummaries()
	if third[0].State != string(subagents.ResidentIdle) {
		t.Fatal("stale manager state")
	}
	if second[0].State != string(subagents.ResidentRunning) {
		t.Fatal("rebuild mutated old slice")
	}
	if &third[0] != &c.residentSummaries()[0] {
		t.Fatal("cache miss after rebuild")
	}
}

func TestResidentSummariesMutationDuringSnapshot(t *testing.T) {
	m := newMockResidentHostLocal(t)
	var revision uint64
	m.EXPECT().Revision().RunAndReturn(func() uint64 { return revision })
	m.EXPECT().Snapshot().RunAndReturn(func() []subagents.ResidentSnapshot {
		revision++
		return []subagents.ResidentSnapshot{{ID: "a", State: subagents.ResidentRunning, HistoryRev: revision}}
	}).Twice()
	c := &Controller{deps: Deps{ResidentManager: m}}
	first := c.residentSummaries()
	second := c.residentSummaries()
	if first[0].HistoryRev == second[0].HistoryRev {
		t.Fatal("concurrent revision stamped as cached")
	}
}

func TestResidentSummariesCacheInvalidatesProjectionFields(t *testing.T) {
	m := newMockResidentHostLocal(t)
	var revision uint64
	row := subagents.ResidentSnapshot{ID: "child", State: subagents.ResidentRunning}
	m.EXPECT().Revision().RunAndReturn(func() uint64 { return revision })
	m.EXPECT().Snapshot().RunAndReturn(func() []subagents.ResidentSnapshot { return []subagents.ResidentSnapshot{row} })
	c := &Controller{deps: Deps{ResidentManager: m}}
	c.res.specs = map[string]subagents.ResidentChildSpec{"child": {}}
	for _, mutate := range []func(){
		func() { row.State = subagents.ResidentIdle },
		func() { row.HistoryRev++ },
		func() { row.Profile = "worker" },
		func() { row.WaitingForModel = true },
		func() { row.ContextUsed = 123; row.ContextMax = 456; row.Subscription = true },
		func() { row.Usage.InputTokens = 789 },
		func() {
			row.UpdatedAt = time.Unix(1, 0)
			row.TurnStartedAt = time.Unix(2, 0)
			row.ActivityUpdatedAt = time.Unix(3, 0)
		},
	} {
		old := c.residentSummaries()
		mutate()
		revision++
		got := c.residentSummaries()
		want := ResidentSummary{ID: row.ID, State: string(row.State), Profile: row.Profile, HistoryRev: row.HistoryRev,
			WaitingForModel: row.WaitingForModel, ContextUsed: row.ContextUsed, ContextMax: row.ContextMax, Subscription: row.Subscription,
			Usage: row.Usage, UpdatedAt: row.UpdatedAt, TurnStartedAt: row.TurnStartedAt, ActivityUpdatedAt: row.ActivityUpdatedAt}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("stale projection: %#v, want %#v", got, want)
		}
		if &old[0] == &got[0] {
			t.Fatal("mutation reused published slice")
		}
	}
	row.OwnedElsewhere = true
	revision++
	if len(c.residentSummaries()) != 0 {
		t.Fatal("foreign resident remained cached")
	}
	row.OwnedElsewhere = false
	revision++
	if len(c.residentSummaries()) != 1 {
		t.Fatal("released lease remained hidden")
	}
}

func TestResidentSummariesSessionScope(t *testing.T) {
	m := newMockResidentHostLocal(t)
	rows := []subagents.ResidentSnapshot{
		{ID: "owned-spec", State: subagents.ResidentCompleted},
		{ID: "owned-task", State: subagents.ResidentFailed},
		{ID: "running", State: subagents.ResidentRunning},
		{ID: "queued", State: subagents.ResidentQueued},
		{ID: "waiting", State: subagents.ResidentIdle, WaitingForModel: true},
		{ID: "foreign", State: subagents.ResidentRunning, OwnedElsewhere: true},
	}
	for _, state := range []subagents.ResidentState{subagents.ResidentIdle, subagents.ResidentCompleted, subagents.ResidentFailed, subagents.ResidentStopped, subagents.ResidentInterrupted} {
		rows = append(rows, subagents.ResidentSnapshot{ID: string(state), State: state})
	}
	m.EXPECT().Revision().Return(uint64(1)).Once()
	m.EXPECT().Snapshot().Return(rows).Once()
	c := &Controller{deps: Deps{ResidentManager: m}}
	c.res.specs = map[string]subagents.ResidentChildSpec{"owned-spec": {}, "foreign": {}}
	c.res.tasks = map[string]string{"owned-task": ""}
	got := c.residentSummaries()
	want := []string{"owned-spec", "owned-task", "running", "queued", "waiting"}
	if len(got) != len(want) {
		t.Fatalf("summaries = %#v", got)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("summary %d = %s, want %s", i, got[i].ID, id)
		}
	}
}

// A manager mutation during Snapshot must not cause updateInfo to rebuild a
// second time and combine run state from one revision with rows from another.
func TestUpdateInfoUsesOneResidentSnapshot(t *testing.T) {
	m := newMockResidentHostLocal(t)
	var revision uint64
	m.EXPECT().Revision().RunAndReturn(func() uint64 { return revision }).Twice()
	m.EXPECT().Snapshot().RunAndReturn(func() []subagents.ResidentSnapshot {
		revision++
		state := subagents.ResidentRunning
		if revision > 1 {
			state = subagents.ResidentCompleted
		}
		return []subagents.ResidentSnapshot{{ID: "child", State: state}}
	}).Twice()
	c := &Controller{deps: Deps{ResidentManager: m, Clock: time.Now}}
	c.residentSeam = c
	c.res.specs = map[string]subagents.ResidentChildSpec{"child": {}}
	c.updateInfo()
	if info := c.Info(); info.Run != RunStateRunning || info.Residents[0].State != string(subagents.ResidentRunning) {
		t.Fatalf("inconsistent first projection: %#v", info)
	}
	c.updateInfo()
	if info := c.Info(); info.Run != RunStateIdle || info.Residents[0].State != string(subagents.ResidentCompleted) {
		t.Fatalf("inconsistent second projection: %#v", info)
	}
}
