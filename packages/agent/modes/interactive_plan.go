package modes

import "github.com/bnema/zut/packages/core"

// RefreshPlan reseeds the plan counters and the per-call checklist snapshot map
// after a session switch. The CLI seeds the agent's core plan state before it
// commits a loaded session; this reads it back and installs the snapshots the
// CLI reconstructed from the session's full transcript.
//
// It mirrors RefreshGoal: the CLI owns agent/session swapping and calls this
// once the swap is committed. snapshots must come from the untrimmed transcript:
// a resumed agent only holds the trim window, and replaying a delta against an
// empty base rebuilds the wrong checklist.
func (i *Interactive) RefreshPlan(snapshots map[string]core.PlanUpdate) {
	var steps []core.PlanStep
	if i.cfg.CurrentPlan != nil {
		// CurrentPlan reaches back into the live agent, so read it before
		// taking i.mu to keep the lock order one-directional.
		steps = i.cfg.CurrentPlan()
	}
	i.applyPlanState(steps, snapshots)
}

// applyPlanState installs the plan counters and snapshot map. The revision bump
// is part of msgVisualKey, so every cached checklist row is invalidated.
func (i *Interactive) applyPlanState(steps []core.PlanStep, snapshots map[string]core.PlanUpdate) {
	i.mu.Lock()
	i.planSnapshots = snapshots
	i.planRevision++
	i.planCurrent, i.planTotal = planProgress(steps)
	i.mu.Unlock()
	i.invalidate()
}

// PlanStatus reports the session-scoped plan counters along with the number of
// reconstructed per-call checklist snapshots. Hosts and tests read the plan
// view through it instead of reaching into the private render state.
func (i *Interactive) PlanStatus() (current, total, snapshots int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.planCurrent, i.planTotal, len(i.planSnapshots)
}

// seedPlanState populates the initial plan counters and snapshot map during
// construction, before the first render. cfg.CurrentPlan reads the live agent
// (not i), so this may run while the Interactive is still private.
//
// InitialPlanSnapshots, when supplied by the CLI, was reconstructed from the
// session's full transcript; it is the only correct source after a resume whose
// trim window starts mid-plan. Without it (embedders, tests) the agent's own
// messages are the best available transcript.
func (i *Interactive) seedPlanState() {
	if i.agent != nil {
		snapshots := i.cfg.InitialPlanSnapshots
		if snapshots == nil {
			snapshots = core.PlanUpdateSnapshots(i.agent.Messages())
		}
		i.planSnapshots = snapshots
		// Advance rather than assign so a re-seed on a live instance cannot
		// move the revision backwards past a render that already observed it.
		i.planRevision++
	}
	if i.cfg.CurrentPlan != nil {
		i.planCurrent, i.planTotal = planProgress(i.cfg.CurrentPlan())
	}
}
