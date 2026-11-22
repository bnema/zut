package core

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func watcherTestAgent(t *testing.T, w Watcher) (*Agent, *recordingTool) {
	t.Helper()
	tool := &recordingTool{}
	a := NewAgent(&queueFakeClient{}, "fake-model", "system", Registry{"echo": tool})
	a.SetWatcher(w)
	return a, tool
}

func TestWatcherHoldReplacesToolCallWithNote(t *testing.T) {
	w := newMockWatcher(t)
	w.EXPECT().RunStarted(mock.Anything).Once()
	w.EXPECT().TakeUsage().Return(provider.Usage{}, false)
	w.EXPECT().BeforeTool(mock.Anything, mock.MatchedBy(func(c provider.ToolCallBlock) bool { return c.ID == "t1" })).
		Return(WatcherVerdict{Hold: true, Note: "advisor: check the caller first"}).Once()
	w.EXPECT().StepCompleted().Once()
	w.EXPECT().BeforeFinish(mock.Anything).Return(WatcherVerdict{}).Once()
	w.EXPECT().RunEnded().Once()

	a, tool := watcherTestAgent(t, w)
	require.NoError(t, a.Prompt(context.Background(), "go", nil, nil))
	require.Nil(t, tool.lastArgs, "held tool must not run")

	msgs := a.Messages()
	var result provider.ToolResultBlock
	for _, m := range msgs {
		for _, c := range m.Content {
			if r, ok := c.(provider.ToolResultBlock); ok {
				result = r
			}
		}
	}
	require.Equal(t, "t1", result.CallID)
	require.True(t, result.IsError)
	require.Equal(t, "advisor: check the caller first", result.Content[0].(provider.TextBlock).Text)
}

func TestWatcherBeforeFinishHoldContinuesWithTaggedNote(t *testing.T) {
	w := newMockWatcher(t)
	w.EXPECT().RunStarted(mock.Anything).Once()
	w.EXPECT().TakeUsage().Return(provider.Usage{}, false)
	w.EXPECT().BeforeTool(mock.Anything, mock.Anything).Return(WatcherVerdict{})
	w.EXPECT().StepCompleted()
	w.EXPECT().BeforeFinish(mock.Anything).Return(WatcherVerdict{Hold: true, Note: "advisor: tests not run"}).Once()
	w.EXPECT().BeforeFinish(mock.Anything).Return(WatcherVerdict{}).Once()
	w.EXPECT().RunEnded().Once()

	a, _ := watcherTestAgent(t, w)
	require.NoError(t, a.Prompt(context.Background(), "go", nil, nil))

	msgs := a.Messages()
	var note *provider.Message
	for i := range msgs {
		if msgs[i].Meta[WatcherNoteMetaKey] == "true" {
			note = &msgs[i]
		}
	}
	require.NotNil(t, note, "held finish must deliver a tagged note")
	require.Equal(t, provider.RoleUser, note.Role)
	require.Equal(t, "advisor: tests not run", note.Content[0].(provider.TextBlock).Text)
	last := msgs[len(msgs)-1]
	require.Equal(t, provider.RoleAssistant, last.Role, "executor answers after the note")
}

func TestWatcherUsageIsFoldedAsExternal(t *testing.T) {
	w := newMockWatcher(t)
	w.EXPECT().RunStarted(mock.Anything).Once()
	w.EXPECT().TakeUsage().Return(provider.Usage{CostUSD: 0.5}, true).Once()
	w.EXPECT().TakeUsage().Return(provider.Usage{}, false)
	w.EXPECT().BeforeTool(mock.Anything, mock.Anything).Return(WatcherVerdict{})
	w.EXPECT().StepCompleted()
	w.EXPECT().BeforeFinish(mock.Anything).Return(WatcherVerdict{})
	w.EXPECT().RunEnded().Once()

	a, _ := watcherTestAgent(t, w)
	var external int
	require.NoError(t, a.Prompt(context.Background(), "go", nil, func(ev AgentEvent) {
		if u, ok := ev.(EvUsage); ok && u.External {
			external++
		}
	}))
	require.Equal(t, 1, external)
	require.InDelta(t, 0.5, a.Cost().CostUSD, 1e-9)
}

func TestWatcherHoldSkipsRestOfBatch(t *testing.T) {
	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("mock").Maybe()
	calls := 0
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, provider.Request) (<-chan provider.Event, error) {
		calls++
		ch := make(chan provider.Event, 1)
		if calls == 1 {
			ch <- provider.EventDone{Stop: provider.StopToolUse, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{
				provider.ToolCallBlock{ID: "a", Name: "echo", Arguments: []byte(`{}`)},
				provider.ToolCallBlock{ID: "b", Name: "echo", Arguments: []byte(`{}`)},
			}}}
		} else {
			ch <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "ok"}}}}
		}
		close(ch)
		return ch, nil
	})
	w := newMockWatcher(t)
	w.EXPECT().RunStarted(mock.Anything).Once()
	w.EXPECT().TakeUsage().Return(provider.Usage{}, false)
	w.EXPECT().BeforeTool(mock.Anything, mock.Anything).Return(WatcherVerdict{Hold: true, Note: "held"}).Once()
	w.EXPECT().StepCompleted().Once()
	w.EXPECT().BeforeFinish(mock.Anything).Return(WatcherVerdict{}).Once()
	w.EXPECT().RunEnded().Once()

	tool := &recordingTool{}
	a := NewAgent(client, "m", "s", Registry{"echo": tool})
	a.SetWatcher(w)
	require.NoError(t, a.Prompt(context.Background(), "go", nil, nil))
	require.Nil(t, tool.lastArgs, "no call of a held batch runs")

	var results []provider.ToolResultBlock
	for _, m := range a.Messages() {
		for _, c := range m.Content {
			if r, ok := c.(provider.ToolResultBlock); ok {
				results = append(results, r)
			}
		}
	}
	require.Len(t, results, 2)
	require.Equal(t, "held", results[0].Content[0].(provider.TextBlock).Text)
	require.Contains(t, results[1].Content[0].(provider.TextBlock).Text, "held by the advisor")
}
