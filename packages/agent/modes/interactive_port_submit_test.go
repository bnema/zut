package modes

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
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
			port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", Run: session.RunStateRunning}}, nil).Once()
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
