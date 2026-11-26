package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/bnema/zut/packages/agent/advisor"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// countingEditTool stands in for the edit tool and counts its runs.
type countingEditTool struct{ runs atomic.Int32 }

func (t *countingEditTool) Name() string            { return "edit" }
func (t *countingEditTool) Description() string     { return "edit a file" }
func (t *countingEditTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *countingEditTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	t.runs.Add(1)
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "edited"}}}, nil
}

func stream(events ...provider.Event) <-chan provider.Event {
	ch := make(chan provider.Event, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch
}

func TestAdvisorWatcherEndToEnd(t *testing.T) {
	pythonTestEnv(t) // isolated, empty config: the decided settings match it
	// Background reviews must not hold either edit or the final answer. They
	// run asynchronously and are canceled at run end, so the advisor may not
	// be called at all; delivery is covered by the watcher unit tests.
	advisorClient := providermocks.NewMockClient(t)
	advisorClient.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		body := req.Messages[len(req.Messages)-1].Content[0].(provider.TextBlock).Text
		require.Contains(t, body, "latest activity")
		return stream(provider.EventTextDelta{Delta: "SILENT"}, provider.EventDone{Stop: provider.StopEnd}), nil
	}).Maybe()

	executor := providermocks.NewMockClient(t)
	executor.EXPECT().Name().Return("mock").Maybe()
	var turns atomic.Int32
	executor.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		switch turns.Add(1) {
		case 1:
			return stream(provider.EventDone{Stop: provider.StopToolUse, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{
				provider.ToolCallBlock{ID: "e1", Name: "edit", Arguments: json.RawMessage(`{"path":"parser.go"}`)},
			}}}), nil
		case 2:
			return stream(provider.EventDone{Stop: provider.StopToolUse, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{
				provider.ToolCallBlock{ID: "e2", Name: "edit", Arguments: json.RawMessage(`{"path":"lexer.go"}`)},
			}}}), nil
		default:
			return stream(provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{
				provider.TextBlock{Text: "fixed in lexer.go"},
			}}}), nil
		}
	})

	edit := &countingEditTool{}
	adv := &tools.AdvisorTool{Target: func(context.Context) (advisor.Target, error) {
		return advisor.Target{Client: advisorClient, Model: provider.Model{ID: "adv"}}, nil
	}}
	reg := core.Registry{"edit": edit, tools.AdvisorToolName: adv}
	ag := core.NewAgent(executor, "exec", "system", reg)
	installAdvisor(ag, clientAdvisorTool(reg), nil, AdvisorSettings{Mode: AdvisorModeAuto})

	require.NoError(t, ag.Prompt(context.Background(), "fix the parse bug", nil, nil))

	require.EqualValues(t, 2, edit.runs.Load(), "both edits run without blocking review")
	require.EqualValues(t, 3, turns.Load(), "two tool batches and one final answer")
	for _, m := range ag.Messages() {
		require.NotEqual(t, "true", m.Meta[core.WatcherNoteMetaKey])
		for _, c := range m.Content {
			if r, ok := c.(provider.ToolResultBlock); ok {
				require.False(t, r.IsError)
			}
		}
	}
}
