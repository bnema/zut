package modes

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/agent/internal/sessionindex"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func finderExecMessages() []provider.Message {
	return []provider.Message{
		jumpTextMsg(provider.RoleUser, "start"),
		jumpTextMsg(provider.RoleAssistant, "first refactoring idea"),
		jumpTextMsg(provider.RoleUser, "ok"),
		jumpTextMsg(provider.RoleAssistant, "second refactoring pass"),
		jumpTextMsg(provider.RoleUser, "done"),
	}
}

// finderExecHit points at message 3 of finderExecMessages.
func finderExecItem(path string) finderItem {
	msgs := finderExecMessages()
	ref := sessionindex.HitRef{Path: path, MsgSeq: 3, TotalMsgs: len(msgs), Fingerprint: core.MessageTextFingerprint(msgs[3])}
	return finderItem{Key: path, Hits: []finderHit{{Ref: ref}}}
}

type finderExecLocal struct {
	i      *Interactive
	root   string
	loaded chan string
}

// newFinderExecLocal builds a local-mode Interactive whose LoadSession
// reads the session file into a real agent.
func newFinderExecLocal(t *testing.T, cwd string) *finderExecLocal {
	t.Helper()
	root := t.TempDir()
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Terminal: &alertTestTerminal{}, SessionsRoot: root, CWD: cwd, Version: "test"})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	i.runCtx = ctx
	i.agent = core.NewAgent(nil, "model", "system", nil)
	l := &finderExecLocal{i: i, root: root, loaded: make(chan string, 4)}
	current := ""
	i.cfg.CurrentSessionPath = func() string { return current }
	i.cfg.LoadSession = func(path string) error {
		snap, err := core.ReadSessionSnapshot(path)
		if err != nil {
			return err
		}
		i.agent.SetMessages(snap.Messages)
		current = path
		l.loaded <- path
		return nil
	}
	i.finder = newFinderModel(cwd)
	return l
}

func (l *finderExecLocal) finishJump(t *testing.T) string {
	t.Helper()
	path := <-l.loaded
	runLoopCall(t, l.i)
	return path
}

func finderSearchQuery(t *testing.T, i *Interactive, q string) {
	t.Helper()
	i.finder.Open(0)
	finderRunes(i.finder, q)
}

func TestFinderOpenLocalSameDirResumesAndJumps(t *testing.T) {
	cwd := t.TempDir()
	l := newFinderExecLocal(t, cwd)
	path := newTreeTestSession(t, l.root, cwd, finderExecMessages())
	finderSearchQuery(t, l.i, "refacto")

	l.i.finderOpen(finderExecItem(path), 0)
	require.Equal(t, path, l.finishJump(t))
	cur, ok := chatSearchCur(l.i)
	require.True(t, ok)
	require.Equal(t, 3, cur)
	require.Nil(t, l.i.pendingFinderJump)
	require.False(t, l.i.finder.Active())
}

func TestFinderOpenLocalSwitchResumes(t *testing.T) {
	cwd, other := t.TempDir(), t.TempDir()
	l := newFinderExecLocal(t, cwd)
	path := newTreeTestSession(t, l.root, other, finderExecMessages())
	finderSearchQuery(t, l.i, "refacto")

	l.i.finderOpen(finderExecItem(path), 0)
	require.NotNil(t, l.i.finderResume, "other directory asks first")
	l.i.handleFinderResumeKey(tui.Key{Kind: tui.KeyRune, Rune: 'y'})
	require.Equal(t, path, l.finishJump(t))
	cur, ok := chatSearchCur(l.i)
	require.True(t, ok)
	require.Equal(t, 3, cur)
}

func TestFinderOpenLocalForkKeepsOriginal(t *testing.T) {
	cwd, other := t.TempDir(), t.TempDir()
	l := newFinderExecLocal(t, cwd)
	path := newTreeTestSession(t, l.root, other, finderExecMessages())
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	finderSearchQuery(t, l.i, "refacto")

	l.i.finderOpen(finderExecItem(path), 0)
	l.i.handleFinderResumeKey(tui.Key{Kind: tui.KeyRune, Rune: 'n'})
	newPath := l.finishJump(t)
	require.NotEqual(t, path, newPath)
	meta, err := core.ManagedSessionMeta(context.Background(), l.root, newPath)
	require.NoError(t, err)
	require.Equal(t, cwd, meta.CWD)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after, "original is byte-identical")
	cur, ok := chatSearchCur(l.i)
	require.True(t, ok)
	require.Equal(t, 3, cur)
	require.Contains(t, l.i.statusOK, "forked into")
}

func TestFinderOpenRejectsUnmanagedPath(t *testing.T) {
	cwd := t.TempDir()
	l := newFinderExecLocal(t, cwd)
	stray := filepath.Join(t.TempDir(), "stray.jsonl")
	require.NoError(t, os.WriteFile(stray, nil, 0o600))
	finderSearchQuery(t, l.i, "refacto")

	l.i.finderOpen(finderExecItem(stray), 0)
	require.Contains(t, l.i.statusErr, "cannot open session")
	require.Nil(t, l.i.pendingFinderJump)
	require.Empty(t, l.loaded)
}

func TestFinderResumeCancelAndMismatchDropPendingJump(t *testing.T) {
	cwd, other := t.TempDir(), t.TempDir()
	l := newFinderExecLocal(t, cwd)
	path := newTreeTestSession(t, l.root, other, finderExecMessages())
	finderSearchQuery(t, l.i, "refacto")
	l.i.finderOpen(finderExecItem(path), 0)
	require.NotNil(t, l.i.pendingFinderJump)
	l.i.finderResumeChoice(l.i.finderResume.plan, resumeChoiceCancel)
	require.Nil(t, l.i.pendingFinderJump)

	l.i.pendingFinderJump = &pendingFinderJump{path: path, terms: []string{"refacto"}}
	l.i.runPendingFinderJump(filepath.Join(l.root, "other.jsonl"))
	require.Nil(t, l.i.pendingFinderJump)
	_, ok := chatSearchCur(l.i)
	require.False(t, ok)
}

// newFinderExecPort builds a port-mode Interactive attached to a session
// in cwd.
func newFinderExecPort(t *testing.T, cwd string) (*Interactive, *mockSessionClient, string) {
	t.Helper()
	root := t.TempDir()
	i, _, client, _ := newTreePortInteractive(t, root, cwd, []provider.Message{jumpTextMsg(provider.RoleUser, "here")})
	i.cfg.Theme, i.cfg.Terminal, i.cfg.CWD, i.cfg.Version = tui.Dark, &alertTestTerminal{}, cwd, "test"
	i.finder = newFinderModel(cwd)
	return i, client, root
}

func expectPortCreate(t *testing.T, client *mockSessionClient, path string) chan session.CreateSpec {
	t.Helper()
	specs := make(chan session.CreateSpec, 1)
	client.EXPECT().List(mock.Anything).Return(nil, nil).Once()
	client.EXPECT().Create(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, spec session.CreateSpec) (session.Info, error) {
		specs <- spec
		return session.Info{}, context.Canceled
	}).Once()
	return specs
}

func TestFinderPortSwitchSendsSessionCWD(t *testing.T) {
	cwd, other := t.TempDir(), t.TempDir()
	i, client, root := newFinderExecPort(t, cwd)
	path := newTreeTestSession(t, root, other, finderExecMessages())
	specs := expectPortCreate(t, client, path)

	i.finderOpen(finderExecItem(path), 0)
	i.handleFinderResumeKey(tui.Key{Kind: tui.KeyRune, Rune: 'y'})
	spec := <-specs
	require.Equal(t, other, spec.CWD)
	require.Equal(t, path, spec.ResumePath)
	res := awaitResumeResult(t, i)
	require.Error(t, res.err)
	i.applySessionResult(context.Background(), res)
	require.Nil(t, i.pendingFinderJump, "a failed switch drops the jump")
}

func TestFinderPortForkSendsCurrentCWD(t *testing.T) {
	cwd, other := t.TempDir(), t.TempDir()
	i, client, root := newFinderExecPort(t, cwd)
	path := newTreeTestSession(t, root, other, finderExecMessages())
	specs := expectPortCreate(t, client, path)

	i.finderOpen(finderExecItem(path), 0)
	i.handleFinderResumeKey(tui.Key{Kind: tui.KeyRune, Rune: 'n'})
	spec := <-specs
	require.Equal(t, cwd, spec.CWD)
	require.NotEqual(t, path, spec.ResumePath)
	meta, err := core.ManagedSessionMeta(context.Background(), root, spec.ResumePath)
	require.NoError(t, err)
	require.Equal(t, cwd, meta.CWD)
	require.Equal(t, spec.ResumePath, i.pendingFinderJump.path)
	awaitResumeResult(t, i)
}

func TestFinderPortAttachUsesLiveID(t *testing.T) {
	cwd, other := t.TempDir(), t.TempDir()
	i, client, root := newFinderExecPort(t, cwd)
	path := newTreeTestSession(t, root, other, finderExecMessages())
	attached := make(chan session.ID, 1)
	client.EXPECT().Attach(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, id session.ID) (session.Port, error) {
		attached <- id
		return nil, context.Canceled
	}).Once()
	item := finderExecItem(path)
	item.Live, item.LiveID = true, "live-7"

	i.finderOpen(item, 0)
	i.handleFinderResumeKey(tui.Key{Kind: tui.KeyRune, Rune: 'y'})
	require.Equal(t, session.ID("live-7"), <-attached)
	awaitResumeResult(t, i)
}

func TestFinderResumeEscDropsPendingJump(t *testing.T) {
	cwd, other := t.TempDir(), t.TempDir()
	l := newFinderExecLocal(t, cwd)
	path := newTreeTestSession(t, l.root, other, finderExecMessages())
	finderSearchQuery(t, l.i, "refacto")
	l.i.finderOpen(finderExecItem(path), 0)
	require.NotNil(t, l.i.pendingFinderJump)

	l.i.handleFinderResumeKey(tui.Key{Kind: tui.KeyEsc})
	require.False(t, l.i.finderResume.Active())
	require.Nil(t, l.i.pendingFinderJump)
	require.Empty(t, l.loaded)
}

func TestFinderPortSwitchRunsJumpAfterHydrate(t *testing.T) {
	cwd, other := t.TempDir(), t.TempDir()
	i, client, root := newFinderExecPort(t, cwd)
	path := newTreeTestSession(t, root, other, finderExecMessages())
	finderSearchQuery(t, i, "refacto")

	client.EXPECT().List(mock.Anything).Return(nil, nil).Once()
	client.EXPECT().Create(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, spec session.CreateSpec) (session.Info, error) {
		require.Equal(t, other, spec.CWD)
		return session.Info{ID: "target"}, nil
	}).Once()
	target := sessionmocks.NewMockPort(t)
	target.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{
		Info:     session.Info{ID: "target", CWD: other, SessionPath: path, Run: session.RunStateIdle},
		Seq:      3,
		Messages: finderExecMessages(),
	}, nil).Once()
	expectPortSubscribe(target, make(chan session.Event), nil)
	client.EXPECT().Attach(mock.Anything, session.ID("target")).Return(target, nil).Once()

	i.finderOpen(finderExecItem(path), 0)
	i.handleFinderResumeKey(tui.Key{Kind: tui.KeyRune, Rune: 'y'})
	res := awaitResumeResult(t, i)
	require.NoError(t, res.err)
	require.False(t, i.applySessionResult(context.Background(), res))

	require.Equal(t, session.ID("target"), i.SessionID())
	cur, ok := chatSearchCur(i)
	require.True(t, ok)
	require.Equal(t, 3, cur)
	require.Nil(t, i.pendingFinderJump)
	require.Contains(t, i.statusOK, `search "refacto"`)
}
