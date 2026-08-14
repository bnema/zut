package modes

import (
	"testing"

	"github.com/bnema/zut/packages/agent/internal/orchestration"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
)

func TestSubmitOrQueuePreservesSealedWorkerWaveWithPendingWorker(t *testing.T) {
	i := &Interactive{agent: &core.Agent{}}
	i.applyCoordinator(orchestration.Event{Kind: orchestration.EventManagerStarted})
	i.applyCoordinator(orchestration.Event{Kind: orchestration.EventWorkerRegistered, WorkerID: "worker#1"})
	i.applyCoordinator(orchestration.Event{Kind: orchestration.EventManagerFinished})

	i.submitOrQueue("review the result", nil, true)

	actions := i.applyCoordinator(orchestration.Event{
		Kind:     orchestration.EventWorkerFinished,
		WorkerID: "worker#1",
		Completion: subagents.Completion{
			AgentID: "worker",
			Status:  "completed",
		},
	})
	if len(actions) != 1 {
		t.Fatalf("completion actions = %d, want 1", len(actions))
	}
	if got := actions[0].Text; got != "review the result" {
		t.Fatalf("completion action text = %q, want queued user input", got)
	}
}
