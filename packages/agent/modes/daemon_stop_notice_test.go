package modes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestDaemonStopExitNotice(t *testing.T) {
	for _, exists := range []bool{true, false} {
		for _, reason := range []string{"daemon_stop", "user_exit"} {
			attach := session.Event{Kind: session.EvAttach, Data: session.AttachEvent{State: session.AttachStateDetached, Reason: reason}}
			closed := session.Event{Kind: session.EvClosed, Data: session.ClosedEvent{Reason: reason}}
			for name, events := range map[string][]session.Event{
				"detach only": {attach}, "closed only": {closed},
				"detach then closed": {attach, closed}, "closed then detach": {closed, attach},
			} {
				t.Run(reason+"/"+name+"/exists="+map[bool]string{true: "true", false: "false"}[exists], func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "session.jsonl")
					if exists {
						require.NoError(t, os.WriteFile(path, []byte("saved\n"), 0600))
					}
					i := NewInteractive(InteractiveConfig{Port: sessionmocks.NewMockPort(t)})
					i.sessionID = "s1"
					i.portInfo.SessionPath = path
					for _, ev := range events {
						require.True(t, i.handleSessionEvent(context.Background(), ev))
					}
					want := "session closed: daemon stopped"
					if reason != "daemon_stop" {
						if events[len(events)-1].Kind == session.EvAttach || events[0].Kind == session.EvAttach {
							want = "detached: user_exit"
						} else {
							want = "session closed: user_exit"
						}
					} else if exists {
						want += "\nResume this session: zut --resume s1"
					}
					require.Equal(t, want, i.ExitNotice())
				})
			}
		}
	}
}

func TestDaemonStopPrintsAfterTerminalRestore(t *testing.T) {
	for _, kind := range []session.EventKind{session.EvAttach, session.EvClosed} {
		t.Run(string(kind), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			require.NoError(t, os.WriteFile(path, []byte("saved\n"), 0600))
			events := make(chan session.Event, 1)
			ev := session.Event{Kind: kind, Data: session.ClosedEvent{Reason: "daemon_stop"}}
			if kind == session.EvAttach {
				ev.Data = session.AttachEvent{State: session.AttachStateDetached, Reason: "daemon_stop"}
			}
			events <- ev
			close(events) // No Closed event need follow a daemon-stop detach.
			port := sessionmocks.NewMockPort(t)
			port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", SessionPath: path}}, nil).Once()
			expectPortSubscribe(port, events, nil)
			term := &keyTerminal{in: make(chan byte)}
			defer close(term.in)
			i := NewInteractive(InteractiveConfig{Port: port, Terminal: term})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, i.Run(ctx))
			require.Equal(t, "session closed: daemon stopped\nResume this session: zut --resume s1", i.ExitNotice())
			require.True(t, strings.HasSuffix(term.String(), tui.SeqShowCursor+i.ExitNotice()+"\n"))
		})
	}
}

func TestDaemonStopDuringPendingQuit(t *testing.T) {
	for _, exists := range []bool{true, false} {
		for _, reason := range []string{"daemon_stop", "user_exit"} {
			for _, result := range []struct {
				name string
				err  error
			}{
				{name: "success"}, {name: "closed", err: session.ErrClosed},
			} {
				t.Run(fmt.Sprintf("%s/exists=%t/%s", reason, exists, result.name), func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "session.jsonl")
					if exists {
						require.NoError(t, os.WriteFile(path, []byte("saved\n"), 0600))
					}
					i := NewInteractive(InteractiveConfig{Port: sessionmocks.NewMockPort(t)})
					i.sessionID = "s1"
					i.portInfo.SessionPath = path
					i.quitClosing = true
					done := i.handleSessionEvent(context.Background(), session.Event{Kind: session.EvClosed, Data: session.ClosedEvent{Reason: reason}})
					if reason != "daemon_stop" {
						require.False(t, done)
						require.Empty(t, i.ExitNotice())
						require.True(t, i.quitClosing)
						return
					}
					require.True(t, done)
					want := "session closed: daemon stopped"
					if exists {
						want += "\nResume this session: zut --resume s1"
					}
					require.Equal(t, want, i.ExitNotice())
					require.True(t, i.applySessionResult(context.Background(), sessionResult{kind: sessionResultExit, gen: i.sessionGen, err: result.err}))
					require.Equal(t, want, i.ExitNotice())
					require.Empty(t, i.statusErr)
					require.False(t, i.quitClosing)
				})
			}
		}
	}
}
