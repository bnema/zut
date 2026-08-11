package orchestration

import (
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
)

func TestCoordinatorSealsWorkerWaveAndWakesOnce(t *testing.T) {
	c := New()
	c.Apply(Event{Kind: EventManagerStarted})
	c.Apply(Event{Kind: EventWorkerRegistered, WorkerID: "a"})
	c.Apply(Event{Kind: EventWorkerRegistered, WorkerID: "b"})
	assertActions(t, c.Apply(Event{Kind: EventManagerFinished}), []Action{{Kind: ActionWait}})
	assertActions(t, c.Apply(Event{Kind: EventWorkerFinished, WorkerID: "b", Completion: subagents.Completion{AgentID: "b"}}), nil)
	assertActions(t, c.Apply(Event{Kind: EventWorkerFinished, WorkerID: "a", Completion: subagents.Completion{AgentID: "a"}}), []Action{{
		Kind: ActionRunManager, Reason: WakeWorkers,
		Completions: []subagents.Completion{{AgentID: "a"}, {AgentID: "b"}},
	}})
}

func TestCoordinatorGivesQueuedUserInputPriority(t *testing.T) {
	c := New()
	c.Apply(Event{Kind: EventManagerStarted})
	c.Apply(Event{Kind: EventWorkerRegistered, WorkerID: "worker"})
	c.Apply(Event{Kind: EventManagerFinished})
	c.Apply(Event{Kind: EventUserInput, Text: "stop and explain"})
	assertActions(t, c.Apply(Event{Kind: EventWorkerFinished, WorkerID: "worker", Completion: subagents.Completion{AgentID: "worker", Status: "failed"}}), []Action{{
		Kind: ActionRunManager, Reason: WakeUser, Text: "stop and explain",
		Completions: []subagents.Completion{{AgentID: "worker", Status: "failed"}},
	}})
}

func TestCoordinatorUsesGoalOnlyWhenIdle(t *testing.T) {
	c := New()
	c.Apply(Event{Kind: EventGoalChanged, GoalActive: true})
	c.Apply(Event{Kind: EventManagerStarted})
	assertActions(t, c.Apply(Event{Kind: EventManagerFinished}), []Action{{Kind: ActionRunManager, Reason: WakeGoal}})
}

func TestCoordinatorStopsAndIgnoresLateWorkerEvents(t *testing.T) {
	c := New()
	c.Apply(Event{Kind: EventManagerStarted})
	c.Apply(Event{Kind: EventWorkerRegistered, WorkerID: "worker"})
	assertActions(t, c.Apply(Event{Kind: EventCancelled}), []Action{{Kind: ActionStop}})
	assertActions(t, c.Apply(Event{Kind: EventWorkerFinished, WorkerID: "worker"}), nil)
}

func assertActions(t *testing.T, got Result, want []Action) {
	t.Helper()
	if len(got.Actions) != len(want) {
		t.Fatalf("actions = %#v, want %#v", got.Actions, want)
	}
	for i := range want {
		if got.Actions[i].Kind != want[i].Kind || got.Actions[i].Reason != want[i].Reason || got.Actions[i].Text != want[i].Text {
			t.Fatalf("action %d = %#v, want %#v", i, got.Actions[i], want[i])
		}
		if len(got.Actions[i].Completions) != len(want[i].Completions) {
			t.Fatalf("action %d completions = %#v, want %#v", i, got.Actions[i].Completions, want[i].Completions)
		}
		for j := range want[i].Completions {
			if got.Actions[i].Completions[j].AgentID != want[i].Completions[j].AgentID || got.Actions[i].Completions[j].Status != want[i].Completions[j].Status {
				t.Fatalf("action %d completion %d = %#v, want %#v", i, j, got.Actions[i].Completions[j], want[i].Completions[j])
			}
		}
	}
}
