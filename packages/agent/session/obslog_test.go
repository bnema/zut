package session

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestSessionLogOmitsPromptAndToolArgs(t *testing.T) {
	const (
		secretPrompt = "sk-live-PROMPTSECRET-1234567890"
		secretArg    = "AKIA-TOOLARGSECRET-0987654321"
		secretErr    = "ghp_ERRORSECRET_abcdef"
	)
	buf := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})).With("session", "s-test")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := NewControllerWithOptions(ctx, Deps{Logger: logger}, Options{ID: "s-test"})
	t.Cleanup(func() { cancel(); <-c.Done() })
	turnSeams(t, c, ctx)

	client := providermocks.NewMockClient(t)
	client.EXPECT().Name().Return("test").Maybe()
	calls := 0
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, provider.Request) (<-chan provider.Event, error) {
		calls++
		ch := make(chan provider.Event, 2)
		switch calls {
		case 1:
			ch <- provider.EventDone{Stop: provider.StopToolUse, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{
				provider.ToolCallBlock{ID: "call-1", Name: "bash", Arguments: []byte(`{"command":"echo ` + secretArg + `"}`)},
			}}}
		case 2:
			ch <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done " + secretPrompt}}}}
		default:
			close(ch)
			return nil, errors.New("request failed: " + secretErr + " " + secretPrompt)
		}
		close(ch)
		return ch, nil
	})
	ag := core.NewAgent(client, "test", "", nil)
	require.NoError(t, c.call(ctx, func() { c.turn.agent = ag }))

	runTurn := func(text string) {
		t.Helper()
		snap, err := c.Snapshot(ctx)
		require.NoError(t, err)
		events, err := c.Subscribe(ctx, snap.Seq)
		require.NoError(t, err)
		require.NoError(t, c.Submit(ctx, Input{Text: text, Origin: InputOriginUser}))
		busy := false
		for {
			select {
			case e := <-events:
				if e.Kind != EvInfo {
					continue
				}
				run := e.Data.(InfoEvent).Info.Run
				if run != RunStateIdle {
					busy = true
				} else if busy {
					return
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	runTurn("deploy with key " + secretPrompt)
	runTurn("again " + secretPrompt)

	out := buf.String()
	require.Contains(t, out, "turn started")
	require.Contains(t, out, "turn ended")
	require.Contains(t, out, "turn failed")
	require.Contains(t, out, "error_category=other")
	require.Contains(t, out, "session=s-test")
	for _, secret := range []string{secretPrompt, secretArg, secretErr, "PROMPTSECRET", "TOOLARGSECRET", "ERRORSECRET", "deploy with key"} {
		require.NotContains(t, out, secret)
	}
}

func TestErrorCategory(t *testing.T) {
	require.Equal(t, "", errorCategory(nil))
	require.Equal(t, "canceled", errorCategory(context.Canceled))
	require.Equal(t, "repetitive_loop", errorCategory(core.ErrRepetitiveLoop))
	require.Equal(t, "rate_limit", errorCategory(errors.New("HTTP 429 secret body")))
	require.Equal(t, "auth", errorCategory(errors.New("http 401 token")))
	require.Equal(t, "other", errorCategory(errors.New("sk-secret")))
}

func TestLogOrigin(t *testing.T) {
	require.Equal(t, "user", logOrigin(InputOriginUser))
	require.Equal(t, "scheduler", logOrigin(InputOriginScheduler))
	require.Equal(t, "inter_session", logOrigin(InputOriginInterSession))
	require.Equal(t, "none", logOrigin(""))
	require.Equal(t, "other", logOrigin("sk-forged origin"))
}

func newLogBuffer() (*lockedBuffer, *slog.Logger) {
	buf := &lockedBuffer{}
	return buf, slog.New(slog.NewTextHandler(buf, nil)).With("session", "s-log")
}

func TestSessionLogsCompaction(t *testing.T) {
	buf, logger := newLogBuffer()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := NewController(ctx, Deps{Logger: logger})
	t.Cleanup(func() { cancel(); <-c.Done() })
	turn := lane9Turn(t, c, ctx)
	ag := lane9Agent(t)
	next := Input{Text: "sk-COMPACTSECRET", Origin: InputOriginUser}
	turn.EXPECT().currentAgent().Return(ag).Maybe()
	continued := make(chan struct{})
	turn.EXPECT().startTurn(next).RunAndReturn(func(Input) error { close(continued); return nil }).Once()
	turn.EXPECT().drainQueued().Return().Once()
	require.NoError(t, c.call(ctx, func() { require.NoError(t, c.beginCompact(&next)) }))
	select {
	case <-continued:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	out := buf.String()
	require.Contains(t, out, `msg="compaction started" session=s-log auto=true`)
	require.Regexp(t, `msg="compaction ended" session=s-log duration=\S+`, out)
	require.NotContains(t, out, "COMPACTSECRET")
	require.NotContains(t, out, "summary")
}

func TestSessionLogsSubagentLifecycle(t *testing.T) {
	buf, logger := newLogBuffer()
	m := newMockResidentHostLocal(t)
	var accepted func(subagents.ResidentChildSpec, string, string)
	var completed func(subagents.ResidentCompletion) bool
	m.EXPECT().SetAcceptedObserver(mock.Anything).Run(func(f func(subagents.ResidentChildSpec, string, string)) { accepted = f })
	m.EXPECT().SetCompletionObserver(mock.Anything).Run(func(f func(subagents.ResidentCompletion) bool) { completed = f })
	m.EXPECT().SetUpdateObserver(mock.Anything)
	m.EXPECT().Snapshot().Return(nil).Maybe()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c := NewController(ctx, Deps{Logger: logger, ResidentManager: m})
	t.Cleanup(func() { cancel(); <-c.Done() })
	accepted(subagents.ResidentChildSpec{ID: "child-1"}, "turn-1", "task with sk-TASKSECRET")
	require.True(t, completed(subagents.ResidentCompletion{ChildID: "child-1", TurnID: "turn-1", Summary: "report sk-SUMMARYSECRET", Err: errors.New("sk-ERRSECRET")}))
	out := buf.String()
	require.Contains(t, out, `msg="subagent spawned" session=s-log agent=child-1 turn=turn-1`)
	require.Contains(t, out, `msg="subagent finished" session=s-log agent=child-1 turn=turn-1 status=failed`)
	for _, secret := range []string{"TASKSECRET", "SUMMARYSECRET", "ERRSECRET"} {
		require.NotContains(t, out, secret)
	}
}
