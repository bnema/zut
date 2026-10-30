package modes

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// keyTerminal feeds scripted bytes to Run's input reader.
type keyTerminal struct {
	alertTestTerminal
	in chan byte
}

func (k *keyTerminal) ReadByte() (byte, error) {
	b, ok := <-k.in
	if !ok {
		return 0, io.EOF
	}
	return b, nil
}

func (k *keyTerminal) send(s string) {
	for i := 0; i < len(s); i++ {
		k.in <- s[i]
	}
}

// startPortKeys runs an Interactive whose keys come from the returned terminal.
func startPortKeys(t *testing.T, port session.Port) (*portRun, *keyTerminal) {
	t.Helper()
	term := &keyTerminal{in: make(chan byte, 64)}
	i := NewInteractive(InteractiveConfig{Terminal: term, Port: port})
	ctx, cancel := context.WithCancel(context.Background())
	r := &portRun{term: &term.alertTestTerminal, i: i, cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- i.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		close(term.in)
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return r, term
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s was not called", what)
	}
}

func TestInteractivePortSubmitSendsInput(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 4)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", Run: session.RunStateIdle}}, nil).Once()
	expectPortSubscribe(port, events, nil)
	called := make(chan struct{})
	var got session.Input
	port.EXPECT().Submit(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in session.Input) error {
		got = in
		close(called)
		return nil
	}).Once()

	_, term := startPortKeys(t, port)
	term.send("hello port\r")
	waitSignal(t, called, "Submit")
	require.Equal(t, "hello port", got.Text)
	require.Equal(t, session.InputOriginUser, got.Origin)
}

func TestInteractivePortEscAbortsRunningTurn(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 4)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", Run: session.RunStateRunning}}, nil).Once()
	expectPortSubscribe(port, events, nil)
	called := make(chan struct{})
	port.EXPECT().SlideBackQueued(mock.Anything).Return(session.Input{}, session.ErrUnknownRequest).Once()
	port.EXPECT().Abort(mock.Anything).RunAndReturn(func(context.Context) error {
		close(called)
		return nil
	}).Once()

	r, term := startPortKeys(t, port)
	require.Eventually(t, r.i.portRunning, 5*time.Second, 10*time.Millisecond)
	term.send("\x1b")
	waitSignal(t, called, "Abort")
}

func TestInteractivePortSubmitErrorShown(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 4)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", Run: session.RunStateIdle}}, nil).Once()
	expectPortSubscribe(port, events, nil)
	port.EXPECT().Submit(mock.Anything, mock.Anything).Return(errors.New("daemonrefused")).Once()

	r, term := startPortKeys(t, port)
	term.send("boom\r")
	r.waitOutput(t, "submit: daemonrefused")
}

func TestInteractivePortSlideBackRestoresEditor(t *testing.T) {
	for _, esc := range []bool{true, false} {
		name := "AltUp"
		if esc {
			name = "Esc"
		}
		t.Run(name, func(t *testing.T) {
			port := sessionmocks.NewMockPort(t)
			events := make(chan session.Event, 4)
			port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", Run: session.RunStateRunning}, Queue: []session.QueuedInput{{Input: session.Input{Text: "queued draft"}}}}, nil).Once()
			expectPortSubscribe(port, events, nil)
			slid := make(chan struct{})
			port.EXPECT().SlideBackQueued(mock.Anything).RunAndReturn(func(context.Context) (session.Input, error) {
				close(slid)
				return session.Input{Text: "queued draft"}, nil
			}).Once()
			if esc {
				port.EXPECT().Abort(mock.Anything).RunAndReturn(func(context.Context) error {
					select {
					case <-slid:
					default:
						t.Error("Abort called before SlideBackQueued")
					}
					return nil
				}).Once()
			}
			r, term := startPortKeys(t, port)
			require.Eventually(t, r.i.portRunning, 5*time.Second, 10*time.Millisecond)
			if esc {
				term.send("\x1b")
			} else {
				term.send("\x1b[1;3A")
			}
			require.Eventually(t, func() bool {
				got := make(chan string, 1)
				select {
				case r.i.loopCalls <- func() { got <- r.i.ed.Value() }:
				case <-time.After(time.Second):
					return false
				}
				select {
				case value := <-got:
					return value == "queued draft"
				case <-time.After(time.Second):
					return false
				}
			}, 5*time.Second, 10*time.Millisecond)
		})
	}
}

// onPortLoop synchronizes test actions with the editor's owner.
func onPortLoop(t *testing.T, i *Interactive, fn func()) {
	t.Helper()
	done := make(chan struct{})
	select {
	case i.loopCalls <- func() { fn(); close(done) }:
	case <-time.After(5 * time.Second):
		t.Fatal("loop call not accepted")
	}
	waitSignal(t, done, "loop call")
}

func TestInteractivePortRecallKeepsTypingAndSerializes(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 4)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", Run: session.RunStateRunning}}, nil).Once()
	expectPortSubscribe(port, events, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	returned := make(chan struct{})
	port.EXPECT().SlideBackQueued(mock.Anything).RunAndReturn(func(ctx context.Context) (session.Input, error) {
		close(entered)
		select {
		case <-release:
			close(returned)
			return session.Input{Text: "saved draft", Images: []provider.ImageBlock{{}}}, nil
		case <-ctx.Done():
			return session.Input{}, ctx.Err()
		}
	}).Once()
	aborted := make(chan struct{})
	port.EXPECT().Abort(mock.Anything).RunAndReturn(func(context.Context) error {
		select {
		case <-returned:
		default:
			t.Error("Abort ran before SlideBackQueued returned")
		}
		close(aborted)
		return nil
	}).Once()
	r, _ := startPortKeys(t, port)
	ctx := context.Background()
	onPortLoop(t, r.i, func() { r.i.portSlideBackQueued(ctx, false) })
	waitSignal(t, entered, "recall")
	onPortLoop(t, r.i, func() {
		r.i.handleKey(ctx, tui.Key{Kind: tui.KeyRune, Rune: 'x'})
		r.i.ed.SetValue("x [clipboard image #1]")
		r.i.clipboardImages = []clipboardImageAttachment{{Marker: "[clipboard image #1]", Image: provider.ImageBlock{}}}
		r.i.handleKey(ctx, tui.Key{Kind: tui.KeyUp, Alt: true})
		// A second recall is ignored; Escape requests an ordered abort.
		r.i.portSlideBackQueued(ctx, true)
	})
	select {
	case <-aborted:
		t.Fatal("Abort ran before recall returned")
	default:
	}
	// Queue a barrier behind the result and completion callbacks without polling.
	finished := make(chan struct{})
	onPortLoop(t, r.i, func() {
		close(release)
	})
	waitSignal(t, aborted, "Abort")
	// Observe the pending flag on the loop; requeue until the worker's completion
	// callback has run (no timing assumptions about goroutine scheduling).
	var check func()
	check = func() {
		if r.i.portRecall != nil {
			r.i.loopCalls <- check
			return
		}
		close(finished)
	}
	r.i.loopCalls <- check
	waitSignal(t, finished, "recall completion")
	onPortLoop(t, r.i, func() {
		require.Equal(t, "x [clipboard image #1]\nsaved draft [clipboard image #2]", r.i.ed.Value())
		require.Len(t, r.i.clipboardImages, 2)
	})
}

func TestInteractivePortEmptyQueueAltUpNavigatesEditor(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	i := NewInteractive(InteractiveConfig{Terminal: &alertTestTerminal{}, Port: port})
	i.ed.SetValue("first\nsecond")
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyUp, Alt: true})
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: 'X'})
	require.Equal(t, "firstX\nsecond", i.ed.Value())
	require.Nil(t, i.portRecall)
}

func TestInteractivePortRecallDropsStaleSessionResult(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	entered, release := make(chan struct{}), make(chan struct{})
	port.EXPECT().SlideBackQueued(mock.Anything).RunAndReturn(func(context.Context) (session.Input, error) {
		close(entered)
		<-release
		return session.Input{Text: "old session"}, nil
	}).Once()
	i := NewInteractive(InteractiveConfig{Terminal: &alertTestTerminal{}, Port: port})
	i.portSlideBackQueued(context.Background(), false)
	waitSignal(t, entered, "recall")
	i.sessionGen++
	newer := &portRecallOperation{gen: i.sessionGen}
	i.portRecall = newer
	i.ed.SetValue("new session")
	close(release)
	select {
	case fn := <-i.loopCalls:
		fn()
	case <-time.After(5 * time.Second):
		t.Fatal("missing recall callback")
	}
	require.Equal(t, "new session", i.ed.Value())
	require.Same(t, newer, i.portRecall)
}

func TestInteractivePortLateEscBeforeRecallCallbackAborts(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	returned := make(chan struct{})
	port.EXPECT().SlideBackQueued(mock.Anything).RunAndReturn(func(context.Context) (session.Input, error) {
		close(returned)
		return session.Input{Text: "queued draft"}, nil
	}).Once()
	aborted := make(chan struct{})
	port.EXPECT().Abort(mock.Anything).RunAndReturn(func(context.Context) error {
		close(aborted)
		return nil
	}).Once()
	i := NewInteractive(InteractiveConfig{Terminal: &alertTestTerminal{}, Port: port})
	i.portInfo.Run = session.RunStateRunning
	i.portQueue = []session.QueuedInput{{Input: session.Input{Text: "queued draft"}}}
	ctx := context.Background()
	i.handleKey(ctx, tui.Key{Kind: tui.KeyUp, Alt: true})
	waitSignal(t, returned, "SlideBackQueued")
	var apply func()
	select {
	case apply = <-i.loopCalls:
	case <-time.After(5 * time.Second):
		t.Fatal("missing recall callback")
	}
	// Retrieval is complete, but its callback has not run. Escape must still
	// be observed by that callback, not by a worker's earlier one-shot check.
	i.handleKey(ctx, tui.Key{Kind: tui.KeyEsc})
	require.True(t, i.portRecall.abortRequested)
	apply()
	waitSignal(t, aborted, "Abort")
	require.Equal(t, "queued draft", i.ed.Value())
	require.Nil(t, i.portRecall)
}

func TestInteractivePortRecallAbortsOriginalSessionAfterSwitch(t *testing.T) {
	oldPort := sessionmocks.NewMockPort(t)
	newPort := sessionmocks.NewMockPort(t)
	entered, release := make(chan struct{}), make(chan struct{})
	oldPort.EXPECT().SlideBackQueued(mock.Anything).RunAndReturn(func(ctx context.Context) (session.Input, error) {
		close(entered)
		select {
		case <-release:
			return session.Input{Text: "old draft"}, nil
		case <-ctx.Done():
			return session.Input{}, ctx.Err()
		}
	}).Once()
	aborted := make(chan struct{})
	oldPort.EXPECT().Abort(mock.Anything).RunAndReturn(func(context.Context) error {
		close(aborted)
		return nil
	}).Once()
	i := NewInteractive(InteractiveConfig{Terminal: &alertTestTerminal{}, Port: oldPort})
	i.portInfo.Run = session.RunStateRunning
	ctx := context.Background()
	i.handleKey(ctx, tui.Key{Kind: tui.KeyEsc})
	waitSignal(t, entered, "old session recall")
	// Apply the port/generation replacement performed by the session switch.
	i.mu.Lock()
	i.sessionGen++
	i.port = newPort
	i.mu.Unlock()
	i.ed.SetValue("new session input")
	close(release)
	select {
	case apply := <-i.loopCalls:
		apply()
	case <-time.After(5 * time.Second):
		t.Fatal("missing old session callback")
	}
	waitSignal(t, aborted, "old session Abort")
	require.Equal(t, "new session input", i.ed.Value())
	require.Nil(t, i.portRecall)
	// The new port has no expected Abort or recall calls.
}
