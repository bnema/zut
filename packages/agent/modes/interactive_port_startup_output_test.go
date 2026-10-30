package modes

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Create-time pre output is shown once in the notes area, never as status,
// and a later resnapshot does not add it again.
func TestPortStartupOutputShownOnce(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	snap := session.Snapshot{Info: session.Info{ID: "s1", Run: session.RunStateIdle}, Seq: 1}
	port.EXPECT().Snapshot(mock.Anything).Return(snap, nil).Twice()
	port.EXPECT().Subscribe(mock.Anything, mock.Anything).Return(make(chan session.Event), nil).Twice()
	i := NewInteractive(InteractiveConfig{Port: port, StartupOutput: "line one\nline two\n"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, i.attachPort(ctx))
	i.showStartupOutput()
	i.mu.Lock()
	require.Empty(t, i.statusOK)
	require.Len(t, i.extNotes, 2)
	require.Contains(t, i.extNotes[0], "line one")
	require.Contains(t, i.extNotes[1], "line two")
	i.mu.Unlock()

	require.NoError(t, i.attachPort(ctx))
	i.showStartupOutput()
	i.mu.Lock()
	defer i.mu.Unlock()
	require.Len(t, i.extNotes, 2)
	require.False(t, strings.Contains(i.statusOK, "line one"))
}

// Run's startup path (attach, then initial resets) keeps the output visible.
func TestPortStartupOutputSurvivesRunStartup(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", Run: session.RunStateIdle}}, nil).Once()
	expectPortSubscribe(port, make(chan session.Event, 1), nil)
	term := &keyTerminal{in: make(chan byte, 1)}
	i := NewInteractive(InteractiveConfig{Terminal: term, Port: port, StartupOutput: "pre-run-output\n"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- i.Run(ctx) }()
	defer func() {
		cancel()
		close(term.in)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return")
		}
	}()
	require.Eventually(t, func() bool {
		i.mu.Lock()
		defer i.mu.Unlock()
		return len(i.extNotes) == 1 && strings.Contains(i.extNotes[0], "pre-run-output") && i.statusOK == ""
	}, 5*time.Second, 10*time.Millisecond)
}
