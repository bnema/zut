package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/agent/tools"
)

func TestDaemonForkInheritsParentConfirmationPolicy(t *testing.T) {
	s1, s2 := newTestSession(t, "s1"), newTestSession(t, "s2")
	s1.set(func(i *session.Info) { i.Provider, i.Model = "p", "m" })
	perms := &tools.PermissionSet{}
	f := sessionmocks.NewMockFactory(t)
	f.EXPECT().Create(mock.Anything, mock.Anything).Return(s1.h, nil).Once()
	specs := make(chan session.CreateSpec, 1)
	f.EXPECT().Create(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, spec session.CreateSpec) (session.Handle, error) {
		specs <- spec
		return s2.h, nil
	}).Once()
	r := startDaemon(t, f, t.TempDir(), Options{})
	a := dial(t, r.sock)
	launch := session.CreateSpec{CWD: "/w", Env: []string{"E=1"}, Zutfile: &session.ZutfileSpec{Name: "agent", Permissions: perms, Pre: "setup"},
		Args: []string{"--no-yolo", "--tools", "read,bash", "--ext", "guard.js", "--session", "/x.jsonl", "--model", "m0"}}
	require.NoError(t, a.call(t, daemonproto.CreateRequest{Type: "create", Spec: launch}, nil))

	parent := s1.h.Info().SessionPath
	fork := session.CreateSpec{CWD: "/w", Fork: &session.ForkSpec{ParentPath: parent, UpToMessage: 2}, Headless: true}
	require.NoError(t, a.call(t, daemonproto.CreateRequest{Type: "create", Spec: fork}, nil))
	got := recv(t, specs)
	require.Equal(t, "--no-yolo --tools read,bash --ext guard.js --provider p --model m", strings.Join(got.Args, " "))
	require.Equal(t, []string{"E=1"}, got.Env)
	require.NotNil(t, got.Zutfile)
	require.Equal(t, "agent", got.Zutfile.Name)
	require.Empty(t, got.Zutfile.Pre, "a fork must not rerun the parent's entry.pre")
	require.NotNil(t, got.Zutfile.Permissions)
	require.Equal(t, fork.Fork, got.Fork)
	require.True(t, got.Headless)
}

func TestDaemonForkOfUnknownParentRequiresConfirmation(t *testing.T) {
	s1 := newTestSession(t, "s1")
	f := sessionmocks.NewMockFactory(t)
	specs := make(chan session.CreateSpec, 1)
	f.EXPECT().Create(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, spec session.CreateSpec) (session.Handle, error) {
		specs <- spec
		return s1.h, nil
	}).Once()
	r := startDaemon(t, f, t.TempDir(), Options{})
	a := dial(t, r.sock)
	fork := session.CreateSpec{CWD: "/w", Args: []string{"--tools", "bash", "--", "hi"}, Fork: &session.ForkSpec{ParentPath: "/elsewhere/p.jsonl"}}
	require.NoError(t, a.call(t, daemonproto.CreateRequest{Type: "create", Spec: fork}, nil))
	got := recv(t, specs)
	require.Equal(t, "--tools bash --no-yolo -- hi", strings.Join(got.Args, " "))
}
