package modes_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent"
	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type resumeTerminal struct {
	bytes.Buffer
	input *strings.Reader
}

func (*resumeTerminal) Size() (int, int)                                  { return 80, 24 }
func (*resumeTerminal) OnResize(func())                                   {}
func (*resumeTerminal) EnterRaw() (func() error, error)                   { return func() error { return nil }, nil }
func (t *resumeTerminal) ReadByte() (byte, error)                         { return t.input.ReadByte() }
func (*resumeTerminal) PeekByteTimeout(time.Duration) (byte, bool, error) { return 0, false, io.EOF }
func (*resumeTerminal) SetNonblock(bool) error                            { return nil }

func TestPrintedResumeCommandResolvesTranscript(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	stored, err := core.NewSession(home, cwd, "test", "test", "test")
	require.NoError(t, err)
	const daemonID = "abcdefghijkl"
	require.NoError(t, stored.AddDaemonID(daemonID))
	require.NoError(t, stored.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "saved"}}}))
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: daemonID, SessionPath: stored.Path}}, nil).Once()
	events := make(chan session.Event)
	port.EXPECT().Subscribe(mock.Anything, mock.Anything).Return((<-chan session.Event)(events), nil).Once()
	client := modes.NewResumeTestClient(t)
	client.EXPECT().Close(mock.Anything, session.ID(daemonID), "user_exit").RunAndReturn(func(context.Context, session.ID, string) error { return stored.Close() }).Once()
	term := &resumeTerminal{input: strings.NewReader("\x03q")}
	ui := modes.NewInteractive(modes.InteractiveConfig{Terminal: term, Port: port, Client: client, Detach: func() { t.Error("quit must not detach") }})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, ui.Run(ctx))
	require.Contains(t, term.String(), "Resume this session: zut --resume "+daemonID+"\n")
	command := strings.TrimPrefix(ui.ExitNotice(), "Resume this session: zut ")
	args, err := agent.ParseArgs(strings.Fields(command))
	require.NoError(t, err)
	require.Equal(t, daemonID, args.ResumeSessionID)
	path, err := core.FindManagedSessionByID(ctx, home, args.ResumeSessionID)
	require.NoError(t, err)
	require.Equal(t, stored.Path, path)
	reopened, _, err := core.OpenSession(path)
	require.NoError(t, err)
	require.Equal(t, stored.ID, reopened.ID)
	require.NoError(t, reopened.Close())
}
