package modes

import (
	"testing"

	"github.com/bnema/zut/packages/agent/internal/orchestration"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestSubmitOrQueuePreservesImagesInHostQueue(t *testing.T) {
	i := &Interactive{agent: &core.Agent{}, busy: true, compacting: true}
	image := provider.ImageBlock{MimeType: "image/png", Data: []byte("png-1")}

	i.submitOrQueue("inspect", []provider.ImageBlock{image}, false)

	if len(i.queued) != 1 || i.queued[0].Text != "inspect" {
		t.Fatalf("queued messages = %#v, want one inspect prompt", i.queued)
	}
	if len(i.queued[0].Images) != 1 || string(i.queued[0].Images[0].Data) != "png-1" {
		t.Fatalf("queued images = %#v, want png-1", i.queued[0].Images)
	}
}

func TestSubmitOrQueuePreservesSealedWorkerWaveWithPendingWorker(t *testing.T) {
	i := &Interactive{agent: &core.Agent{}}
	i.applyCoordinator(orchestration.Event{Kind: orchestration.EventManagerStarted})
	i.applyCoordinator(orchestration.Event{Kind: orchestration.EventWorkerRegistered, WorkerID: "worker#1"})
	i.applyCoordinator(orchestration.Event{Kind: orchestration.EventManagerFinished})

	image := provider.ImageBlock{MimeType: "image/png", Data: []byte("png-1")}
	i.submitOrQueue("review the result", []provider.ImageBlock{image}, true)

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
	if len(actions[0].Images) != 1 || string(actions[0].Images[0].Data) != "png-1" {
		t.Fatalf("completion action images = %#v, want queued image", actions[0].Images)
	}
}
