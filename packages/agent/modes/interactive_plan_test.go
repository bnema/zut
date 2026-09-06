package modes

import (
	"testing"

	"github.com/bnema/zut/packages/core"
)

func TestInteractiveTracksCurrentPlanStep(t *testing.T) {
	interactive := &Interactive{}
	interactive.handleEvent(core.EvPlanUpdate{Update: core.PlanUpdate{Plan: []core.PlanStep{
		{Step: "Inspect", Status: core.PlanCompleted},
		{Step: "Implement", Status: core.PlanInProgress},
		{Step: "Verify", Status: core.PlanPending},
	}}})

	if interactive.planCurrent != 2 || interactive.planTotal != 3 {
		t.Fatalf("plan progress = %d/%d, want 2/3", interactive.planCurrent, interactive.planTotal)
	}

	interactive.handleEvent(core.EvPlanUpdate{Update: core.PlanUpdate{Plan: []core.PlanStep{
		{Step: "Inspect", Status: core.PlanCompleted},
	}}})
	if interactive.planCurrent != 0 || interactive.planTotal != 1 {
		t.Fatalf("completed plan progress = %d/%d, want hidden current with total 1", interactive.planCurrent, interactive.planTotal)
	}
}
