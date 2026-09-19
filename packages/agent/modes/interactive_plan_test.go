package modes

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

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
func TestInteractivePlanCountersSurviveSecondTurn(t *testing.T) {
	ag := core.NewAgent(alertBlockingClient{}, "test-model", "", nil)
	i := NewInteractive(InteractiveConfig{Agent: ag, Terminal: &alertTestTerminal{}})
	i.handleEvent(core.EvPlanUpdate{CallID: "plan-1", Update: core.PlanUpdate{Plan: []core.PlanStep{
		{Step: "one", Status: core.PlanCompleted},
		{Step: "two", Status: core.PlanInProgress},
	}}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	i.runCtx = ctx
	go i.runStreamPacer(ctx)
	i.startTurn(ctx, "second turn")

	// Wait until the turn owns the busy slot, which is after the turn-start
	// reset site that used to clear planCurrent/planTotal.
	deadline := time.Now().Add(time.Second)
	started := false
	for time.Now().Before(deadline) {
		i.mu.Lock()
		busy := i.busy
		i.mu.Unlock()
		if busy {
			started = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !started {
		t.Fatal("second turn did not start")
	}
	if current, total := interactivePlanCounters(i); current != 2 || total != 2 {
		t.Fatalf("plan counters after a new turn = %d/%d, want 2/2", current, total)
	}
	i.CancelTurn()
}

// TestInteractiveRefreshPlanSeedsCountersAndSnapshots covers the resume path:
// the CLI seeds the agent's core plan from the loaded session, and the host
// derives its counters and historical checklists from it.
func TestInteractiveRefreshPlanSeedsCountersAndSnapshots(t *testing.T) {
	ag := core.NewAgent(alertBlockingClient{}, "test-model", "", nil)
	ag.SetMessages([]provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{
			ID: "legacy-1", Name: "update_plan", Arguments: []byte(`{"plan":[{"step":"one","status":"completed"},{"step":"two","status":"in_progress"}]}`),
		}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID: "legacy-1", Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}},
		}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{
			ID: "delta-1", Name: "plan", Arguments: []byte(`{"action":"add","steps":[{"step":"three"}]}`),
		}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID: "delta-1", Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}},
		}}},
	})
	wantPlan := []core.PlanStep{
		{Step: "one", Status: core.PlanCompleted},
		{Step: "two", Status: core.PlanInProgress},
		{Step: "three", Status: core.PlanPending},
	}
	ag.SetPlan(wantPlan)

	i := NewInteractive(InteractiveConfig{Agent: ag, CurrentPlan: ag.CurrentPlan})
	// Clear the construction-time seed so RefreshPlan is what rebuilds it.
	i.mu.Lock()
	i.planSnapshots = nil
	i.planRevision = 0
	i.mu.Unlock()

	i.RefreshPlan(core.PlanUpdateSnapshots(ag.Messages()))

	if current, total := interactivePlanCounters(i); current != 2 || total != 3 {
		t.Fatalf("refreshed counters = %d/%d, want 2/3", current, total)
	}
	i.mu.Lock()
	legacy := i.planSnapshots["legacy-1"]
	delta := i.planSnapshots["delta-1"]
	i.mu.Unlock()
	if !slices.Equal(legacy.Plan, []core.PlanStep{
		{Step: "one", Status: core.PlanCompleted},
		{Step: "two", Status: core.PlanInProgress},
	}) {
		t.Fatalf("legacy snapshot = %#v", legacy.Plan)
	}
	if !slices.Equal(delta.Plan, []core.PlanStep{
		{Step: "one", Status: core.PlanCompleted},
		{Step: "two", Status: core.PlanInProgress},
		{Step: "three", Status: core.PlanPending},
	}) {
		t.Fatalf("delta snapshot = %#v", delta.Plan)
	}
}

// TestInteractiveClearDropsPlanEverywhere covers /clear: core state, the
// counters, the snapshot map, and the persisted session file.
func TestInteractiveClearDropsPlanEverywhere(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	steps := []core.PlanStep{{Step: "one", Status: core.PlanPending}}
	if err := sess.UpdatePlan(&core.SessionPlan{Steps: steps}); err != nil {
		t.Fatal(err)
	}

	ag := core.NewAgent(alertBlockingClient{}, "model", "", nil)
	ag.SetPlan(steps)
	persisted := 0
	i := NewInteractive(InteractiveConfig{
		Agent:       ag,
		CurrentPlan: ag.CurrentPlan,
		PersistPlan: func(plan *core.SessionPlan) error {
			persisted++
			return sess.UpdatePlan(plan)
		},
	})
	i.handleEvent(core.EvPlanUpdate{CallID: "plan-1", Update: core.PlanUpdate{Plan: steps}})

	i.runSlash(context.Background(), "/clear")

	if got := ag.CurrentPlan(); got != nil {
		t.Fatalf("core plan after /clear = %#v, want nil", got)
	}
	if current, total := interactivePlanCounters(i); current != 0 || total != 0 {
		t.Fatalf("counters after /clear = %d/%d, want 0/0", current, total)
	}
	if got := interactivePlanSnapshotCount(i); got != 0 {
		t.Fatalf("snapshot map after /clear has %d entries, want 0", got)
	}
	if persisted != 1 {
		t.Fatalf("PersistPlan calls = %d, want 1", persisted)
	}
	if sess.Meta.Plan != nil {
		t.Fatalf("in-memory session plan after /clear = %#v, want nil", sess.Meta.Plan)
	}
	reopened, _, err := core.OpenSession(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Meta.Plan != nil {
		t.Fatalf("persisted session plan after /clear = %#v, want nil", reopened.Meta.Plan)
	}
}

// TestInteractiveClearWithoutPlanDoesNotPersist keeps /clear a no-op when there
// is nothing to drop, so a --no-session run never surfaces a persistence error.
func TestInteractiveClearWithoutPlanDoesNotPersist(t *testing.T) {
	ag := core.NewAgent(alertBlockingClient{}, "model", "", nil)
	called := false
	i := NewInteractive(InteractiveConfig{
		Agent:       ag,
		CurrentPlan: ag.CurrentPlan,
		PersistPlan: func(*core.SessionPlan) error {
			called = true
			return nil
		},
	})
	i.runSlash(context.Background(), "/clear")
	if called {
		t.Fatal("/clear persisted an empty plan")
	}
}

// TestInteractiveClearPersistsWhenCoreAndFileDiverge guards GO-005: /clear must
// clear the file plan even when core state lost it, because the bound session
// still carries it and a stale file would seed the next resume.
func TestInteractiveClearPersistsWhenCoreAndFileDiverge(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if err := sess.UpdatePlan(&core.SessionPlan{Steps: []core.PlanStep{{Step: "one", Status: core.PlanPending}}}); err != nil {
		t.Fatal(err)
	}

	// Core state is empty while the session file still holds a plan.
	ag := core.NewAgent(alertBlockingClient{}, "model", "", nil)
	persisted := 0
	i := NewInteractive(InteractiveConfig{
		Agent:       ag,
		CurrentPlan: ag.CurrentPlan,
		PersistedPlan: func() *core.SessionPlan {
			if sess.Meta.Plan == nil {
				return nil
			}
			plan := *sess.Meta.Plan
			return &plan
		},
		PersistPlan: func(plan *core.SessionPlan) error {
			persisted++
			return sess.UpdatePlan(plan)
		},
	})

	i.runSlash(context.Background(), "/clear")

	if persisted != 1 {
		t.Fatalf("PersistPlan calls = %d, want 1 for a divergent session plan", persisted)
	}
	if sess.Meta.Plan != nil {
		t.Fatalf("session plan after /clear = %#v, want nil", sess.Meta.Plan)
	}
}

// TestInteractivePlanSnapshotsRaceWithRender publishes plan updates from one
// goroutine while another renders a frame. Run under -race, it guards the
// copy-on-write snapshot map and the render clone that carries it.
func TestInteractivePlanSnapshotsRaceWithRender(t *testing.T) {
	ag := core.NewAgent(alertBlockingClient{}, "test-model", "", nil)
	ag.SetMessages([]provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "plan-1", Name: "plan", Arguments: []byte(`{"action":"set"}`)}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "plan-1", Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}}}}},
	})
	i := NewInteractive(InteractiveConfig{Agent: ag, Terminal: &alertTestTerminal{}})
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
func TestModelSwapCarriesPlanAcrossProviderRebuild(t *testing.T) {
	targets := provider.ModelsForProvider("anthropic")
	if len(targets) == 0 {
		t.Skip("anthropic catalog unavailable")
	}
	target := targets[0]

	ag := core.NewAgent(alertBlockingClient{}, "start-model", "", nil)
	ag.SetMessages([]provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "hello"}},
	}})
	want := []core.PlanStep{{Step: "one", Status: core.PlanPending}}
	ag.SetPlan(want)

	i := NewInteractive(InteractiveConfig{Agent: ag, Provider: "openai", Model: "start-model"})
	rebuilt := core.NewAgent(alertBlockingClient{}, target.ID, "", nil)
	replaced, ok := i.swapModelUnserialized(target.Provider, target.ID, func(string, string) (*core.Agent, string, string, error) {
		return rebuilt, target.Provider, target.ID, nil
	}, false)
	if !ok || !replaced {
		t.Fatalf("cross-provider swap replaced=%v ok=%v", replaced, ok)
	}
	if got := rebuilt.CurrentPlan(); !slices.Equal(got, want) {
		t.Fatalf("plan after provider rebuild = %#v, want %#v", got, want)
	}
}
