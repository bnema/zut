package modes

import (
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestInteractiveExtensionInsertAppliedOnRunLoop(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 4)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", Run: session.RunStateIdle}}, nil).Once()
	expectPortSubscribe(port, events, nil)
	r, _ := startPortKeys(t, port)

	go r.i.Insert("from extension") // off-loop caller
	require.Eventually(t, func() bool {
		got := make(chan string, 1)
		select {
		case r.i.loopCalls <- func() { got <- r.i.ed.Value() }:
		default:
			return false
		}
		return <-got == "from extension"
	}, 5*time.Second, 10*time.Millisecond)
}

func TestInteractiveExtensionInsertFullQueueReportsError(t *testing.T) {
	i := NewInteractive(InteractiveConfig{Terminal: &alertTestTerminal{}})
	for n := 0; n < loopCallsCap; n++ {
		require.NoError(t, i.queueEditorInsert("x"))
	}
	require.ErrorIs(t, i.queueEditorInsert("x"), errLoopCallsFull)

	i.Insert("dropped") // must not block
	i.mu.Lock()
	defer i.mu.Unlock()
	require.True(t, strings.Contains(i.statusErr, errLoopCallsFull.Error()), i.statusErr)
	require.Empty(t, i.ed.Value())
}
