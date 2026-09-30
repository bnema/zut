package modes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestInteractiveWaitReportAcceptance(t *testing.T) {
	for _, reset := range []bool{false, true} {
		name := "accepted"
		if reset {
			name = "cancelled tracking"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ag := core.NewAgent(nil, "test", "", nil)
			i := &Interactive{agent: ag, busy: true, runCtx: ctx}
			release := i.beginCompletionDeliveryHold()
			defer release()
			manager := subagents.NewResidentManager(t.TempDir(), func(_ subagents.ResidentChildSpec, journal *subagents.ResidentJournal) (subagents.ResidentRuntime, error) {
				return subagents.ResidentTurnRunner(func(context.Context, string) error {
					if reset {
						i.cancelCoordinator()
					}
					return journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "interactive report"}}}})
				}), nil
			})
			t.Cleanup(func() { _ = manager.Close(context.Background()) })
			manager.SetAcceptedObserver(func(spec subagents.ResidentChildSpec, turn, _ string) { i.TrackResidentSubagent(spec.ID, turn) })
			manager.SetCompletionObserver(i.ReportResidentSubagent)
			spawn := &tools.SubagentSpawnTool{
				ResidentManager: manager, Enabled: func() bool { return true },
				BuildResidentSpec: func(context.Context, tools.ResidentSpawnRequest) (subagents.ResidentChildSpec, error) {
					return subagents.ResidentChildSpec{ID: "worker", SessionID: "session", Provider: "openai", Model: "test"}, nil
				},
			}
			result, err := spawn.Execute(ctx, json.RawMessage(`{"task":"report","wait":5}`), nil)
			if err != nil || result.IsError {
				t.Fatalf("spawn = (%#v, %v)", result, err)
			}
			encoded, err := json.Marshal(result.Content)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "interactive report") != reset {
				t.Fatalf("reset=%t tool result=%s", reset, encoded)
			}
			if reset {
				if strings.Contains(string(encoded), "host_update") || ag.QueuedMessageCount() != 0 {
					t.Fatalf("rejected completion claimed host delivery: %s", encoded)
				}
				return
			}
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				queued := ag.PendingQueuedMessages()
				if len(queued) != 0 {
					if len(queued) != 1 || !queued[0].HostEvent || strings.Count(queued[0].Text, "interactive report") != 1 || !strings.Contains(queued[0].Text, "[auto-subagents update]") {
						t.Fatalf("queued host report = %#v", queued)
					}
					return
				}
				select {
				case <-deadline.C:
					t.Fatal("accepted completion did not slide in")
				case <-tick.C:
				}
			}
		})
	}
}
