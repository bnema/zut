package modes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
)

func TestResidentCompletionSlidesIntoBusyParent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		compacting bool
	}{
		{name: "success"},
		{name: "failure", err: errors.New("review failed")},
		{name: "compaction", compacting: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ag := core.NewAgent(nil, "model", "", nil)
			i := &Interactive{agent: ag, busy: true, compacting: tc.compacting, runCtx: ctx}
			release := i.beginCompletionDeliveryHold()
			defer release()
			i.TrackResidentSubagent("finished", "turn-1")
			i.TrackResidentSubagent("still-running", "turn-2")
			i.ReportResidentSubagent(subagents.ResidentCompletion{ChildID: "finished", TurnID: "turn-1", Task: "review", Summary: "saved result", Err: tc.err})

			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				i.mu.Lock()
				queued := ag.PendingQueuedMessages()
				if tc.compacting {
					queued = append([]core.QueuedMessage(nil), i.queued...)
				}
				i.mu.Unlock()
				if len(queued) != 0 {
					if len(queued) != 1 || !queued[0].HostEvent || !strings.Contains(queued[0].Text, "saved result") || !strings.Contains(queued[0].Text, "[auto-subagents update]") {
						t.Fatalf("queued completions = %#v", queued)
					}
					if tc.err != nil && !strings.Contains(queued[0].Text, tc.err.Error()) {
						t.Fatalf("missing failure: %q", queued[0].Text)
					}
					if i.ensureCompletionTracker().Pending() != 1 {
						t.Fatal("unfinished child was not retained")
					}
					return
				}
				select {
				case <-deadline.C:
					t.Fatal("completion did not slide in while parent and sibling were active")
				case <-tick.C:
				}
			}
		})
	}
}
