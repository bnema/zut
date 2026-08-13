package subagents

import (
	"encoding/json"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestResidentLiveProjectionPublishesCopiesAndFinalizesLiveItems(t *testing.T) {
	projection := newResidentLiveProjection()
	projection.Start("turn-1")
	projection.Apply(core.EvTextDelta{Delta: "draft"})
	projection.Apply(core.EvToolUseStart{ID: "call-1", Name: "bash"})
	projection.Apply(core.EvToolUseArgs{ID: "call-1", Delta: `{"command":"pwd"}`})
	projection.Apply(core.EvToolExecutionStarted{ID: "call-1", Name: "bash"})

	live := projection.Snapshot()
	if live.TurnID != "turn-1" || live.State != ResidentRunning || live.AssistantText != "draft" || len(live.Tools) != 1 || live.Tools[0].State != ResidentLiveToolRunning || string(live.Tools[0].Args) != `{"command":"pwd"}` || live.Revision == 0 {
		t.Fatalf("live projection = %#v", live)
	}
	live.Tools[0].Args[0] = 'x'
	if got := projection.Snapshot(); string(got.Tools[0].Args) != `{"command":"pwd"}` {
		t.Fatalf("snapshot mutated projection: %#v", got)
	}

	projection.Apply(core.EvToolResult{ID: "call-1", Result: core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "done"}}}})
	projection.Apply(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "final"}}}})
	projection.Finish(ResidentIdle)
	final := projection.Snapshot()
	if final.AssistantText != "" || len(final.Tools) != 0 || final.State != ResidentIdle || final.Revision <= live.Revision {
		t.Fatalf("final projection = %#v", final)
	}
}

func TestResidentLiveProjectionBoundsToolArguments(t *testing.T) {
	projection := newResidentLiveProjection()
	projection.Start("turn-1")
	projection.Apply(core.EvToolCall{ID: "call-1", Name: "bash", Args: json.RawMessage(`{"command":"pwd"}`)})
	if got := projection.Snapshot(); len(got.Tools) != 1 || string(got.Tools[0].Args) != `{"command":"pwd"}` {
		t.Fatalf("live tool = %#v", got)
	}
}
