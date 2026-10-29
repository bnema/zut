package bot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

type sent struct {
	channel, text string
	opts          SendOptions
}

// runnerHarness wires a Runner to generated Port and adapter mocks. The
// adapter's Run captures the handlers and blocks until ctx ends; events are
// pushed into the Port subscription channel.
type runnerHarness struct {
	port     *sessionmocks.MockPort
	adapter  *mockBotAdapter
	events   chan session.Event
	inputs   chan session.Input
	sent     chan sent
	handlers chan [2]any
	info     session.Info
	runErr   chan error
	cancel   context.CancelFunc
}

func newRunnerHarness(t *testing.T) *runnerHarness {
	h := &runnerHarness{
		port:     sessionmocks.NewMockPort(t),
		adapter:  newMockBotAdapter(t),
		events:   make(chan session.Event, 16),
		inputs:   make(chan session.Input, 4),
		sent:     make(chan sent, 8),
		handlers: make(chan [2]any, 1),
		info:     session.Info{ID: "s1", Provider: "p", Model: "m", CWD: "/w", Run: session.RunStateIdle},
		runErr:   make(chan error, 1),
	}
	h.port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Seq: 3}, nil).Once()
	h.port.EXPECT().Subscribe(mock.Anything, uint64(3)).Return((<-chan session.Event)(h.events), nil).Once()
	h.port.EXPECT().Info().RunAndReturn(func() session.Info { return h.info }).Maybe()
	h.port.EXPECT().Submit(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in session.Input) error {
		h.inputs <- in
		return nil
	}).Maybe()
	h.adapter.EXPECT().Run(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, msg func(InboundMessage), cmd func(Command, InboundMessage)) error {
			h.handlers <- [2]any{msg, cmd}
			<-ctx.Done()
			return ctx.Err()
		}).Once()
	h.adapter.EXPECT().Send(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, ch, text string, o SendOptions) error {
			h.sent <- sent{ch, text, o}
			return nil
		}).Maybe()
	h.adapter.EXPECT().IndicateWorking(mock.Anything, mock.Anything).Return(func() {}).Maybe()
	h.adapter.EXPECT().StatusText().Return("@zutbot").Maybe()
	return h
}

func (h *runnerHarness) start(t *testing.T) (func(InboundMessage), func(Command, InboundMessage)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	r := NewRunner(h.adapter, h.port, Config{})
	go func() { h.runErr <- r.Run(ctx) }()
	hs := recv(t, h.handlers)
	t.Cleanup(func() {
		cancel()
		close(h.events)
		if err := recv(t, h.runErr); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	return hs[0].(func(InboundMessage)), hs[1].(func(Command, InboundMessage))
}

func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
	var zero T
	return zero
}

func infoEvent(run session.RunState, origin session.InputOrigin) session.Event {
	return session.Event{Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{Run: run, CurrentOrigin: origin}}}
}

func assistant(text string) session.Event {
	return session.Event{Kind: session.EvAgent, Agent: core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: text}}}}}
}

func TestRunnerSubmitsWithTelegramOriginAndRepliesOnTurnEnd(t *testing.T) {
	h := newRunnerHarness(t)
	onMsg, _ := h.start(t)

	img := []provider.ImageBlock{{MimeType: "image/png", Data: []byte{1}}}
	onMsg(InboundMessage{ChannelID: "42", MessageID: "7", Text: "hello", Images: img})
	in := recv(t, h.inputs)
	if in.Origin != session.InputOriginTelegram || in.Text != "hello" || len(in.Images) != 1 {
		t.Fatalf("submitted %+v", in)
	}

	// A user-originated run must not consume the telegram reply slot.
	h.events <- infoEvent(session.RunStateRunning, session.InputOriginUser)
	h.events <- assistant("tui answer")
	h.events <- infoEvent(session.RunStateIdle, "")
	h.events <- infoEvent(session.RunStateRunning, session.InputOriginTelegram)
	h.events <- assistant("draft")
	h.events <- assistant("final answer")
	h.events <- infoEvent(session.RunStateIdle, "")

	got := recv(t, h.sent)
	if got.channel != "42" || got.text != "final answer" {
		t.Fatalf("reply = %+v", got)
	}
}

func TestRunnerReportsTurnError(t *testing.T) {
	h := newRunnerHarness(t)
	onMsg, _ := h.start(t)
	onMsg(InboundMessage{ChannelID: "1", Text: "x"})
	recv(t, h.inputs)
	h.events <- infoEvent(session.RunStateRunning, session.InputOriginTelegram)
	h.events <- session.Event{Kind: session.EvAgent, Agent: core.EvTurnEnd{Err: errors.New("boom")}}
	h.events <- infoEvent(session.RunStateIdle, "")
	if got := recv(t, h.sent); got.text != "error: boom" {
		t.Fatalf("reply = %q", got.text)
	}
}

func TestRunnerStopAbortsRunningSession(t *testing.T) {
	h := newRunnerHarness(t)
	_, onCmd := h.start(t)

	onCmd(CmdStop, InboundMessage{ChannelID: "1", MessageID: "5"})
	if got := recv(t, h.sent); got.text != "nothing running." || got.opts.ReplyToMessageID != "5" {
		t.Fatalf("idle stop = %+v", got)
	}

	h.info.Run = session.RunStateRunning
	h.port.EXPECT().Abort(mock.Anything).Return(nil).Once()
	onCmd(CmdStop, InboundMessage{ChannelID: "1"})
	if got := recv(t, h.sent); got.text != "cancelled the current turn." {
		t.Fatalf("stop = %q", got.text)
	}
}

func TestRunnerStatusUsesSessionInfo(t *testing.T) {
	h := newRunnerHarness(t)
	_, onCmd := h.start(t)
	onCmd(CmdStatus, InboundMessage{ChannelID: "1"})
	got := recv(t, h.sent).text
	for _, want := range []string{"(p) m", "state: idle", "cwd: /w", "@zutbot"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status %q missing %q", got, want)
		}
	}
}

func TestRunnerSubmitErrorIsReported(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	adapter := newMockBotAdapter(t)
	r := NewRunner(adapter, port, Config{})
	port.EXPECT().Submit(mock.Anything, mock.Anything).Return(session.ErrClosed).Once()
	adapter.EXPECT().IndicateWorking(mock.Anything, "9").Return(func() {}).Once()
	adapter.EXPECT().Send(mock.Anything, "9", "error: session closed", SendOptions{ReplyToMessageID: "3"}).Return(nil).Once()
	r.handleMessage(InboundMessage{ChannelID: "9", MessageID: "3", Text: "x"})
}

func TestRunnerReturnsWhenSessionCloses(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	adapter := newMockBotAdapter(t)
	events := make(chan session.Event, 1)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{}, nil).Once()
	port.EXPECT().Subscribe(mock.Anything, uint64(0)).Return((<-chan session.Event)(events), nil).Once()
	adapter.EXPECT().Run(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, _ func(InboundMessage), _ func(Command, InboundMessage)) error {
			<-ctx.Done()
			return ctx.Err()
		}).Once()
	events <- session.Event{Seq: 1, Kind: session.EvClosed, Data: session.ClosedEvent{Reason: "killed"}}
	r := NewRunner(adapter, port, Config{})
	if err := r.Run(context.Background()); !errors.Is(err, session.ErrClosed) {
		t.Fatalf("Run = %v, want ErrClosed", err)
	}
}

func TestWatchTurnsResubscribesAfterSeqTooOld(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 4)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Seq: 1}, nil).Once()
	port.EXPECT().Subscribe(mock.Anything, uint64(1)).Return(nil, session.ErrSeqTooOld).Once()
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Seq: 9}, nil).Once()
	port.EXPECT().Subscribe(mock.Anything, uint64(9)).Return((<-chan session.Event)(events), nil).Once()
	events <- session.Event{Seq: 10, Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{Run: session.RunStateRunning, CurrentOrigin: session.InputOriginUser}}}
	events <- session.Event{Seq: 11, Kind: session.EvAgent, Agent: core.EvUserMessage{Message: provider.Message{Content: []provider.Content{provider.TextBlock{Text: "typed"}}}}}
	events <- session.Event{Seq: 12, Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{Run: session.RunStateIdle}}}
	events <- session.Event{Seq: 13, Kind: session.EvClosed}
	var got []TurnResult
	err := WatchTurns(context.Background(), port, func(r TurnResult) { got = append(got, r) })
	if !errors.Is(err, session.ErrClosed) {
		t.Fatalf("WatchTurns = %v", err)
	}
	if len(got) != 1 || got[0].Origin != session.InputOriginUser || len(got[0].Prompts) != 1 || got[0].Prompts[0] != "typed" {
		t.Fatalf("turns = %+v", got)
	}
}
