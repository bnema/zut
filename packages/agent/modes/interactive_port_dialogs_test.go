package modes

import (
	"context"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// dialogKeyTerminal feeds raw input bytes to the render loop.
type dialogKeyTerminal struct {
	alertTestTerminal
	in chan byte
}

func (k *dialogKeyTerminal) ReadByte() (byte, error) { return <-k.in, nil }

func startPortDialogs(t *testing.T) (*sessionmocks.MockPort, chan session.Event, *portRun) {
	t.Helper()
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 8)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", Run: session.RunStateRunning}, Seq: 1}, nil).Once()
	expectPortSubscribe(port, events, nil)
	return port, events, startPortInteractive(t, port, nil)
}

type keyedPortRun struct {
	*portRun
	keys chan byte
}

func startKeyedPortDialogs(t *testing.T) (*sessionmocks.MockPort, chan session.Event, keyedPortRun) {
	t.Helper()
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 8)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", Run: session.RunStateRunning}, Seq: 1}, nil).Once()
	expectPortSubscribe(port, events, nil)
	term := &dialogKeyTerminal{in: make(chan byte, 4)}
	i := NewInteractive(InteractiveConfig{Terminal: term, Port: port})
	ctx, cancel := context.WithCancel(context.Background())
	r := &portRun{term: &term.alertTestTerminal, i: i, cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- i.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return port, events, keyedPortRun{r, term.in}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	require.Eventually(t, cond, 5*time.Second, 10*time.Millisecond, "timed out waiting for %s", what)
}

// loopBarrier pushes a sentinel through the Run loop and waits for it, so
// every loop call queued before it has run.
func loopBarrier(t *testing.T, i *Interactive) {
	t.Helper()
	ran := make(chan struct{})
	select {
	case i.loopCalls <- func() { close(ran) }:
	case <-time.After(5 * time.Second):
		t.Fatal("loop barrier not accepted")
	}
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("loop barrier not run")
	}
}

func TestInteractivePortConfirmDialogAnswersThroughPort(t *testing.T) {
	port, events, r := startPortDialogs(t)
	answered := make(chan core.ConfirmDecision, 1)
	port.EXPECT().AnswerConfirmation(mock.Anything, "p1", mock.Anything).RunAndReturn(func(_ context.Context, _ string, d core.ConfirmDecision) error {
		answered <- d
		return nil
	}).Once()
	events <- session.Event{Seq: 2, Kind: session.EvPending, Data: session.PendingEvent{Pending: session.PendingRequest{ID: "p1", Kind: "confirm", Confirm: &core.ToolCallConfirmation{Name: "bash", Summary: "ls"}}}}
	r.waitOutput(t, "confirm tool call")
	waitFor(t, "confirm dialog", r.i.confirmDialog.Active)
	r.i.confirmDialog.HandleKey(tui.Key{Kind: tui.KeyEnter})
	select {
	case d := <-answered:
		require.Equal(t, core.ConfirmDecision{Allow: true}, d)
	case <-time.After(5 * time.Second):
		t.Fatal("AnswerConfirmation not called")
	}
	require.False(t, r.i.confirmDialog.Active())
}

func TestInteractivePortQuestionDialogAnswersThroughPort(t *testing.T) {
	port, events, r := startPortDialogs(t)
	answered := make(chan []tools.QuestionAnswer, 1)
	port.EXPECT().AnswerQuestion(mock.Anything, "q1", mock.Anything).RunAndReturn(func(_ context.Context, _ string, a []tools.QuestionAnswer) error {
		answered <- a
		return nil
	}).Once()
	qs := []tools.Question{{Question: "Pick?", Options: []tools.QuestionOption{{Label: "red"}, {Label: "blue"}}}}
	events <- session.Event{Seq: 2, Kind: session.EvPending, Data: session.PendingEvent{Pending: session.PendingRequest{ID: "q1", Kind: "question", Questions: qs}}}
	waitFor(t, "question dialog", r.i.questionDialog.Active)
	r.i.questionDialog.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: '2'})
	select {
	case a := <-answered:
		require.Equal(t, []tools.QuestionAnswer{{Selected: []string{"blue"}}}, a)
	case <-time.After(5 * time.Second):
		t.Fatal("AnswerQuestion not called")
	}
}

func TestInteractivePortRescueDialogAnswersThroughPort(t *testing.T) {
	port, events, r := startKeyedPortDialogs(t)
	answered := make(chan []tools.QuestionAnswer, 1)
	port.EXPECT().AnswerQuestion(mock.Anything, "r1", mock.Anything).RunAndReturn(func(_ context.Context, _ string, a []tools.QuestionAnswer) error {
		answered <- a
		return nil
	}).Once()
	events <- session.Event{Seq: 2, Kind: session.EvPending, Data: session.PendingEvent{Pending: session.PendingRequest{ID: "r1", Kind: "rescue", Rescue: &session.RescueOptions{Error: "boom", Candidates: []string{"openai/gpt-x"}}}}}
	r.waitOutput(t, "rescue turn")
	// Route through the render loop's key path so rescueDialog stays single-threaded.
	r.keys <- '\r'
	select {
	case a := <-answered:
		require.Equal(t, []tools.QuestionAnswer{{Selected: []string{"openai/gpt-x"}}}, a)
	case <-time.After(5 * time.Second):
		t.Fatal("AnswerQuestion not called")
	}
}

func TestInteractivePortPendingResolvedClosesDialog(t *testing.T) {
	_, events, r := startPortDialogs(t)
	events <- session.Event{Seq: 2, Kind: session.EvPending, Data: session.PendingEvent{Pending: session.PendingRequest{ID: "p1", Kind: "confirm", Confirm: &core.ToolCallConfirmation{Name: "bash"}}}}
	events <- session.Event{Seq: 3, Kind: session.EvPending, Data: session.PendingEvent{Pending: session.PendingRequest{ID: "q1", Kind: "question", Questions: []tools.Question{{Question: "free?"}}}}}
	waitFor(t, "dialogs open", func() bool { return r.i.confirmDialog.Active() && r.i.questionDialog.Active() })
	events <- session.Event{Seq: 4, Kind: session.EvPendingResolved, Data: session.PendingResolvedEvent{ID: "p1"}}
	events <- session.Event{Seq: 5, Kind: session.EvPendingResolved, Data: session.PendingResolvedEvent{ID: "q1"}}
	waitFor(t, "dialogs closed", func() bool { return !r.i.confirmDialog.Active() && !r.i.questionDialog.Active() })
}
