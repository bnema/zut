package modes

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func interactivePlanCounters(i *Interactive) (current, total int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.planCurrent, i.planTotal
}

func interactivePlanSnapshotCount(i *Interactive) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.planSnapshots)
}

func interactivePlanSnapshot(i *Interactive, callID string) core.PlanUpdate {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.planSnapshots[callID]
}

func TestInteractiveTracksCurrentPlanStep(t *testing.T) {
	interactive := &Interactive{}
	interactive.handleEvent(core.EvPlanUpdate{CallID: "plan-1", Update: core.PlanUpdate{Plan: []core.PlanStep{
		{Step: "Inspect", Status: core.PlanCompleted},
		{Step: "Implement", Status: core.PlanInProgress},
		{Step: "Verify", Status: core.PlanPending},
	}}})

	if current, total := interactivePlanCounters(interactive); current != 2 || total != 3 {
		t.Fatalf("plan progress = %d/%d, want 2/3", current, total)
	}
	if got := interactivePlanSnapshot(interactive, "plan-1").Plan; len(got) != 3 {
		t.Fatalf("plan snapshot = %#v, want the three-step checklist", got)
	}

	interactive.handleEvent(core.EvPlanUpdate{CallID: "plan-2", Update: core.PlanUpdate{Plan: []core.PlanStep{
		{Step: "Inspect", Status: core.PlanCompleted},
	}}})
	if current, total := interactivePlanCounters(interactive); current != 0 || total != 1 {
		t.Fatalf("completed plan progress = %d/%d, want hidden current with total 1", current, total)
	}
}

// TestInteractivePlanCountersSurviveSecondTurn guards the removed per-turn reset:
// a new turn must not wipe the session-scoped counters.
// A new daemon turn (Info.Run running) must not wipe the session-scoped
// counters the live EvPlanUpdate set.
func TestInteractivePlanCountersSurviveSecondTurn(t *testing.T) {
	i := newQueuePortInteractive(t, sessionmocks.NewMockPort(t), session.Snapshot{Info: session.Info{Run: session.RunStateIdle}}, InteractiveConfig{Terminal: &alertTestTerminal{}})
	i.handleEvent(core.EvPlanUpdate{CallID: "plan-1", Update: core.PlanUpdate{Plan: []core.PlanStep{
		{Step: "one", Status: core.PlanCompleted},
		{Step: "two", Status: core.PlanInProgress},
	}}})
	i.handleEvent(core.EvTurnStart{})
	i.handleEvent(core.EvAssistantStart{})
	if current, total := interactivePlanCounters(i); current != 2 || total != 2 {
		t.Fatalf("plan counters after a new turn = %d/%d, want 2/2", current, total)
	}
}

// TestInteractiveRefreshPlanSeedsCountersAndSnapshots covers the resume path:
// the CLI seeds the agent's core plan from the loaded session, and the host
// derives its counters and historical checklists from it.
// Attaching hydrates the checklist snapshots and step counters from the
// daemon (session TestGoalPlanEvents covers how the daemon derives them).
func TestInteractiveRefreshPlanSeedsCountersAndSnapshots(t *testing.T) {
	plan := map[string]core.PlanUpdate{"delta-1": {Plan: []core.PlanStep{
		{Step: "one", Status: core.PlanCompleted},
		{Step: "two", Status: core.PlanInProgress},
		{Step: "three", Status: core.PlanPending},
	}}}
	i := newQueuePortInteractive(t, sessionmocks.NewMockPort(t), session.Snapshot{
		Info: session.Info{Run: session.RunStateIdle, PlanStep: &session.PlanStepSummary{Index: 2, Total: 3, Text: "two"}},
		Plan: plan,
	}, InteractiveConfig{})
	if current, total := interactivePlanCounters(i); current != 2 || total != 3 {
		t.Fatalf("hydrated counters = %d/%d, want 2/3", current, total)
	}
	if got := interactivePlanSnapshot(i, "delta-1").Plan; !slices.Equal(got, plan["delta-1"].Plan) {
		t.Fatalf("hydrated snapshot = %#v", got)
	}
}

// TestInteractiveClearDropsPlanEverywhere covers /clear: core state, the
// counters, the snapshot map, and the persisted session file.

// TestInteractiveClearWithoutPlanDoesNotPersist keeps /clear a no-op when there
// is nothing to drop, so a --no-session run never surfaces a persistence error.

// TestInteractiveClearPersistsWhenCoreAndFileDiverge guards GO-005: /clear must
// clear the file plan even when core state lost it, because the bound session
// still carries it and a stale file would seed the next resume.

// TestInteractivePlanSnapshotsRaceWithRender publishes plan updates from one
// goroutine while another renders a frame. Run under -race, it guards the
// copy-on-write snapshot map and the render clone that carries it.
func TestInteractivePlanSnapshotsRaceWithRender(t *testing.T) {
	i := newQueuePortInteractive(t, sessionmocks.NewMockPort(t), session.Snapshot{
		Info: session.Info{Run: session.RunStateIdle},
		Messages: []provider.Message{
			{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "plan-1", Name: "plan", Arguments: []byte(`{"action":"set"}`)}}},
			{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "plan-1", Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}}}}},
		},
	}, InteractiveConfig{Terminal: &alertTestTerminal{}})
	i.mu.Lock()
	i.planSnapshots = map[string]core.PlanUpdate{
		"plan-1": {Plan: []core.PlanStep{{Step: "one", Status: core.PlanPending}}},
	}
	i.mu.Unlock()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			i.handleEvent(core.EvPlanUpdate{
				CallID: fmt.Sprintf("plan-%d", n%4),
				Update: core.PlanUpdate{Plan: []core.PlanStep{{Step: fmt.Sprintf("step-%d", n), Status: core.PlanInProgress}}},
			})
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			i.redraw()
		}
	}()

	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("plan publish/render did not finish")
	}
}

// TestModelSwapCarriesPlanAcrossProviderRebuild covers the other agent rebuild
// that D4 calls out: a cross-provider /model swap builds a fresh agent, so the
// session-scoped plan has to be carried like the transcript and cost.
