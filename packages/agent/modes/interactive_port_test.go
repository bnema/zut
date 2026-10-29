package modes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func portUserMessage(text string) provider.Message {
	return provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: text}}}
}

// expectPortSubscribe makes Subscribe return a test-owned channel and
// records the afterSeq it was called with.
func expectPortSubscribe(port *sessionmocks.MockPort, events chan session.Event, after *uint64) {
	port.EXPECT().Subscribe(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, seq uint64) (<-chan session.Event, error) {
		if after != nil {
			*after = seq
		}
		return events, nil
	}).Once()
}

type portRun struct {
	term   *alertTestTerminal
	i      *Interactive
	cancel context.CancelFunc
	done   chan error
}

func startPortInteractive(t *testing.T, port session.Port, client SessionClient) *portRun {
	t.Helper()
	term := &alertTestTerminal{}
	i := NewInteractive(InteractiveConfig{Terminal: term, Port: port, Client: client})
	ctx, cancel := context.WithCancel(context.Background())
	r := &portRun{term: term, i: i, cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- i.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return r
}

func (r *portRun) waitOutput(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stripANSIBytes(r.term.String()), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("terminal never showed %q\noutput: %q", want, stripANSIBytes(r.term.String()))
}

func (r *portRun) waitExit(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		r.done <- err // let cleanup observe the exit too
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit")
		return nil
	}
}

func TestInteractivePortStreamingRender(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 16)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", Run: session.RunStateIdle}, Seq: 3}, nil).Once()
	var after uint64
	expectPortSubscribe(port, events, &after)

	r := startPortInteractive(t, port, nil)
	events <- session.Event{Seq: 4, Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{ID: "s1", Run: session.RunStateRunning}}}
	events <- session.Event{Seq: 5, Kind: session.EvAgent, Agent: core.EvUserMessage{Message: portUserMessage("quokkaprompt")}}
	events <- session.Event{Seq: 6, Kind: session.EvAgent, Agent: core.EvAssistantStart{}}
	events <- session.Event{Seq: 7, Kind: session.EvAgent, Agent: core.EvTextDelta{Delta: "zebracorn"}}
	r.waitOutput(t, "zebracorn")
	final := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "zebracorn done"}}}
	events <- session.Event{Seq: 8, Kind: session.EvAgent, Agent: core.EvAssistantMessage{Message: final}}
	events <- session.Event{Seq: 9, Kind: session.EvAgent, Agent: core.EvTurnEnd{Stop: provider.StopEnd}}
	events <- session.Event{Seq: 10, Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{ID: "s1", Run: session.RunStateIdle}}}
	r.waitOutput(t, "zebracorn done")
	r.waitOutput(t, "quokkaprompt")
	require.Equal(t, uint64(3), after, "Subscribe must resume after the snapshot Seq")

	r.i.mu.Lock()
	defer r.i.mu.Unlock()
	require.Len(t, r.i.portMsgs, 2, "user + assistant mirrored into the transcript")
	require.False(t, r.i.busy)
}

func TestInteractivePortHydratesMidTurn(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 4)
	args := json.RawMessage(`{"path":"notes.txt"}`)
	snap := session.Snapshot{
		Info:     session.Info{ID: "s1", Run: session.RunStateRunning, Usage: provider.Usage{InputTokens: 7}, ContextUsed: 1234},
		Seq:      42,
		Messages: []provider.Message{portUserMessage("hydratedquestion")},
		Live: session.LiveTurn{TurnID: "t1", AssistantText: "partialanswer", Tools: []session.LiveTool{
			{ID: "c1", Name: "read", Args: args, State: "running"},
		}},
		Queue:    []session.QueuedInput{{ID: "q1", Input: session.Input{Text: "queuedfollowup"}}},
		Pending:  []session.PendingRequest{{ID: "p1", Kind: "confirm"}},
		Deferred: []session.DeferredAction{{ID: "d1", Tool: "bash"}},
		ExtChrome: session.ExtChrome{Statuses: map[string]map[string]session.ExtStatus{
			"ext": {"k": {Level: "info", Text: "extstatusline"}},
		}},
	}
	port.EXPECT().Snapshot(mock.Anything).Return(snap, nil).Once()
	var after uint64
	expectPortSubscribe(port, events, &after)

	r := startPortInteractive(t, port, nil)
	r.waitOutput(t, "hydratedquestion")
	r.waitOutput(t, "partialanswer")
	r.waitOutput(t, "queuedfollowup")
	r.waitOutput(t, "extstatusline")
	require.Equal(t, uint64(42), after)
	require.Equal(t, session.ID("s1"), r.i.SessionID())
	require.Len(t, r.i.PendingRequests(), 1)
	require.Len(t, r.i.DeferredActions(), 1)

	r.i.mu.Lock()
	defer r.i.mu.Unlock()
	require.True(t, r.i.busy)
	require.Equal(t, 1234, r.i.lastCtxInput)
	require.Equal(t, []string{"c1"}, r.i.toolOrder)
	require.Equal(t, "read", r.i.toolCalls["c1"].Name)
}

func TestInteractivePortResnapshotOnMessagesReplaced(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 4)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1"}, Seq: 1, Messages: []provider.Message{portUserMessage("oldtranscript")}}, nil).Once()
	expectPortSubscribe(port, events, nil)
	resnapped := make(chan struct{})
	port.EXPECT().Snapshot(mock.Anything).RunAndReturn(func(context.Context) (session.Snapshot, error) {
		close(resnapped)
		return session.Snapshot{Info: session.Info{ID: "s1"}, Seq: 5, Messages: []provider.Message{portUserMessage("compactedsummary")}}, nil
	}).Once()

	r := startPortInteractive(t, port, nil)
	r.waitOutput(t, "oldtranscript")
	events <- session.Event{Seq: 5, Kind: session.EvMessagesReplaced, Data: session.MessagesReplacedEvent{}}
	<-resnapped
	r.waitOutput(t, "compactedsummary")

	r.i.mu.Lock()
	defer r.i.mu.Unlock()
	require.Len(t, r.i.portMsgs, 1)
	require.Equal(t, "compactedsummary", r.i.portMsgs[0].Content[0].(provider.TextBlock).Text)
}

func TestInteractivePortDetachedByOtherExits(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 2)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1"}}, nil).Once()
	expectPortSubscribe(port, events, nil)

	r := startPortInteractive(t, port, nil)
	events <- session.Event{Kind: session.EvAttach, Data: session.AttachEvent{State: session.AttachStateDetached, Reason: "detached_by_other"}}
	require.NoError(t, r.waitExit(t), "detached_by_other is a clean exit (status 0)")
	require.Equal(t, detachedByOtherNotice, r.i.ExitNotice())
	out := r.term.String()
	cleanupEnd := strings.LastIndex(out, "\x1b[?25h") // SeqShowCursor ends terminal restore
	notice := strings.LastIndex(out, detachedByOtherNotice)
	require.Greater(t, notice, cleanupEnd, "notice must be printed after the terminal is restored")
}

func TestInteractivePortSuccessorReattach(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 2)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1"}, Messages: []provider.Message{portUserMessage("beforeclear")}}, nil).Once()
	expectPortSubscribe(port, events, nil)

	successor := sessionmocks.NewMockPort(t)
	successorEvents := make(chan session.Event, 2)
	successor.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s2"}, Seq: 9, Messages: []provider.Message{portUserMessage("freshsession")}}, nil).Once()
	var after uint64
	expectPortSubscribe(successor, successorEvents, &after)

	client := newMockSessionClient(t)
	client.EXPECT().Attach(mock.Anything, session.ID("s2")).Return(successor, nil).Once()

	r := startPortInteractive(t, port, client)
	r.waitOutput(t, "beforeclear")
	events <- session.Event{Kind: session.EvClosed, Data: session.ClosedEvent{Reason: "cleared", Successor: "s2"}}
	close(events)
	r.waitOutput(t, "freshsession")
	require.Equal(t, session.ID("s2"), r.i.SessionID())
	require.Equal(t, successor, r.i.SessionPort())
	require.Equal(t, uint64(9), after)

	// The old stream closing must not end Run: the successor stream owns it.
	successorEvents <- session.Event{Seq: 10, Kind: session.EvNote, Data: session.NoteEvent{Note: session.Note{Level: "info", Text: "stillattached"}}}
	r.waitOutput(t, "stillattached")
	select {
	case err := <-r.done:
		t.Fatalf("Run exited after successor reattach: %v", err)
	default:
	}
}
