package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
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

// countingEditTool stands in for the edit tool: the watcher keys on the name.
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
	// The advisor answers by checkpoint: STOP at the first change, a NOTE at
	// the final answer, SILENT in background reviews.
	advisorClient := providermocks.NewMockClient(t)
	var reviewsMu sync.Mutex
	var reviews []string
	advisorClient.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		body := req.Messages[len(req.Messages)-1].Content[0].(provider.TextBlock).Text
		answer := "SILENT"
		switch {
		case strings.Contains(body, "about to make its first change"):
			answer, body = "STOP: the bug is in lexer.go, not parser.go", "change"
		case strings.Contains(body, "final answer"):
			answer, body = "NOTE: mention that tests were not run", "finish"
		default:
			body = "step"
		}
		reviewsMu.Lock()
		reviews = append(reviews, body)
		reviewsMu.Unlock()
		return stream(provider.EventTextDelta{Delta: answer}, provider.EventDone{Stop: provider.StopEnd}), nil
	})

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

	require.EqualValues(t, 1, edit.runs.Load(), "the held edit never runs; the corrected one does")
	require.EqualValues(t, 4, turns.Load(), "held change, corrected change, answer, answer after the note")

	var held, noted bool
	for _, m := range ag.Messages() {
		for _, c := range m.Content {
			if r, ok := c.(provider.ToolResultBlock); ok && r.CallID == "e1" {
				held = r.IsError && strings.Contains(r.Content[0].(provider.TextBlock).Text, "lexer.go")
			}
		}
		if m.Meta[core.WatcherNoteMetaKey] == "true" && strings.Contains(m.Content[0].(provider.TextBlock).Text, "tests were not run") {
			noted = true
		}
	}
	require.True(t, held, "the first change is held with the advisor's reason")
	require.True(t, noted, "the final-answer note reaches the executor")

	reviewsMu.Lock()
	defer reviewsMu.Unlock()
	require.Contains(t, reviews, "change")
	require.Contains(t, reviews, "finish")
}
