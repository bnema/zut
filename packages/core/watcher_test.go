package core

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/provider"
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

func TestWatcherUsageIsFoldedAsExternal(t *testing.T) {
	w := newMockWatcher(t)
	w.EXPECT().RunStarted(mock.Anything).Once()
	w.EXPECT().TakeUsage().Return(provider.Usage{CostUSD: 0.5}, true).Once()
	w.EXPECT().TakeUsage().Return(provider.Usage{}, false)
	w.EXPECT().StepCompleted()
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

func TestEndWatcherRunDropsStaleNotesOnly(t *testing.T) {
	w := newMockWatcher(t)
	w.EXPECT().RunEnded().Once()
	w.EXPECT().TakeUsage().Return(provider.Usage{CostUSD: 0.25}, true).Once()
	a := NewAgent(&queueFakeClient{}, "m", "s", Registry{})
	require.True(t, a.QueueWatcherNote("[advisor note]\nstale"))
	require.True(t, a.QueuePrompt(QueuedMessage{Text: "resident report", HostEvent: true}))
	a.endWatcherRun(w)

	pending := a.PendingQueuedMessages()
	require.Len(t, pending, 1)
	require.Equal(t, "resident report", pending[0].Text)
	require.InDelta(t, 0.25, a.Cost().CostUSD, 1e-9, "late review usage is still recorded")
}

func TestSessionTextHelpersSkipAdvisorNotes(t *testing.T) {
	note := provider.Message{Role: provider.RoleUser, Meta: map[string]string{WatcherNoteMetaKey: "true"},
		Content: []provider.Content{provider.TextBlock{Text: "[advisor note]\nrun the tests"}}}
	user := provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "fix the parser"}}}
	require.Equal(t, "fix the parser", lastUserText([]provider.Message{user, note}))
	require.Equal(t, "fix the parser", firstUserTextFromMessagesAfter([]provider.Message{note, user}, 0))
}
