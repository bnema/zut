package sdk

import (
	"testing"

	"github.com/bnema/zut/packages/core"
)

// TestRuntimePlanAccessors guards GO-004: an embedder that seeds history with
// SetMessages can also seed the session-scoped plan, so `show` and indexed
// updates see the persisted steps instead of an empty plan.
func TestRuntimePlanAccessors(t *testing.T) {
	rt := &Runtime{agent: core.NewAgent(nil, "model", "", nil)}
	rt.SetPlan([]PlanStep{
		{Step: "one", Status: "in_progress"},
		{Step: "two", Status: "pending"},
	})

	got := rt.Plan()
	if len(got) != 2 || got[0].Step != "one" || got[0].Status != "in_progress" || got[1].Step != "two" || got[1].Status != "pending" {
		t.Fatalf("plan = %#v", got)
	}
	got[0].Step = "mutated"
	if again := rt.Plan(); again[0].Step != "one" {
		t.Fatalf("Plan returned an aliased slice: %#v", again)
	}

	rt.SetPlan(nil)
	if cleared := rt.Plan(); cleared != nil {
		t.Fatalf("plan after clear = %#v, want nil", cleared)
	}
}

func TestRuntimePlanAccessorsWithoutAgent(t *testing.T) {
	rt := &Runtime{}
	rt.SetPlan([]PlanStep{{Step: "one", Status: "pending"}})
	if got := rt.Plan(); got != nil {
		t.Fatalf("plan = %#v, want nil without an agent", got)
	}
}
