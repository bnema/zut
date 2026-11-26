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

	l.i.finderOpenItem(finderExecItem(path), 0)
	runLoopCall(t, l.i)
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

	l.i.finderOpenItem(finderExecItem(path), 0)
	runLoopCall(t, l.i)
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

	l.i.finderOpenItem(finderExecItem(path), 0)
	runLoopCall(t, l.i)
	l.i.handleFinderResumeKey(tui.Key{Kind: tui.KeyRune, Rune: 'n'})
	runLoopCall(t, l.i)
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

	l.i.finderOpenItem(finderExecItem(stray), 0)
	runLoopCall(t, l.i)
	require.Contains(t, l.i.statusErr, "cannot open session")
	require.Nil(t, l.i.pendingFinderJump)
	require.Empty(t, l.loaded)
}

func TestFinderResumeCancelAndMismatchDropPendingJump(t *testing.T) {
	cwd, other := t.TempDir(), t.TempDir()
	l := newFinderExecLocal(t, cwd)
	path := newTreeTestSession(t, l.root, other, finderExecMessages())
	finderSearchQuery(t, l.i, "refacto")
	l.i.finderOpenItem(finderExecItem(path), 0)
	runLoopCall(t, l.i)
	require.NotNil(t, l.i.pendingFinderJump)
	l.i.finderExecuteResume(l.i.finderResume.plan, resumeChoiceCancel)
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

	i.finderOpenItem(finderExecItem(path), 0)
	runLoopCall(t, i)
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

	i.finderOpenItem(finderExecItem(path), 0)
	runLoopCall(t, i)
	i.handleFinderResumeKey(tui.Key{Kind: tui.KeyRune, Rune: 'n'})
	runLoopCall(t, i)
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
	i, client, _ := newFinderExecPort(t, cwd)
	// A live session outside the namespace root (agents/, --session files):
	// attaching must not require ManagedSessionMeta.
	path := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	attached := make(chan session.ID, 1)
	client.EXPECT().Attach(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, id session.ID) (session.Port, error) {
		attached <- id
		return nil, context.Canceled
	}).Once()
	item := finderExecItem(path)
	item.Live, item.LiveID, item.LiveCWD = true, "live-7", other

	i.finderOpenItem(item, 0)
	require.True(t, i.finderResume.Active())
	require.Equal(t, other, i.finderResume.plan.TargetCWD)
	require.False(t, i.finderResume.plan.CWDExists)
	i.handleFinderResumeKey(tui.Key{Kind: tui.KeyRune, Rune: 'y'})
	require.Equal(t, session.ID("live-7"), <-attached)
	awaitResumeResult(t, i)
}

func TestFinderResumeEscDropsPendingJump(t *testing.T) {
	cwd, other := t.TempDir(), t.TempDir()
	l := newFinderExecLocal(t, cwd)
	path := newTreeTestSession(t, l.root, other, finderExecMessages())
	finderSearchQuery(t, l.i, "refacto")
	l.i.finderOpenItem(finderExecItem(path), 0)
	runLoopCall(t, l.i)
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

	i.finderOpenItem(finderExecItem(path), 0)
	runLoopCall(t, i)
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

func TestFinderResumeRefusedWhileBusy(t *testing.T) {
	cwd, other := t.TempDir(), t.TempDir()
	l := newFinderExecLocal(t, cwd)
	same := newTreeTestSession(t, l.root, cwd, finderExecMessages())
	away := newTreeTestSession(t, l.root, other, finderExecMessages())
	finderSearchQuery(t, l.i, "refacto")
	before := countJSONL(t, l.root)

	// Busy at open: no confirm, no load, jump dropped.
	l.i.busy = true
	l.i.finderOpenItem(finderExecItem(same), 0)
	require.Equal(t, finderResumeBusyMsg, l.i.statusErr)
	require.Nil(t, l.i.pendingFinderJump)
	require.False(t, l.i.finderResume.Active())
	l.i.finderOpenItem(finderExecItem(away), 0)
	require.False(t, l.i.finderResume.Active(), "no y/n prompt while busy")

	// Idle at open, busy by the time the user picks: switch and fork refused.
	for _, key := range []rune{'y', 'n'} {
		l.i.busy = false
		l.i.statusErr = ""
		l.i.finderOpenItem(finderExecItem(away), 0)
		runLoopCall(t, l.i)
		require.True(t, l.i.finderResume.Active())
		l.i.busy = true
		l.i.handleFinderResumeKey(tui.Key{Kind: tui.KeyRune, Rune: key})
		require.Equal(t, finderResumeBusyMsg, l.i.statusErr)
		require.Nil(t, l.i.pendingFinderJump)
	}
	require.Empty(t, l.loaded)
	require.Equal(t, before, countJSONL(t, l.root), "fork wrote no new session")
}

func countJSONL(t *testing.T, root string) int {
	t.Helper()
	n := 0
	require.NoError(t, filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) == ".jsonl" {
			n++
		}
		return err
	}))
	return n
}

func TestFinderOpenCurrentSessionJumpsInPlaceLocal(t *testing.T) {
	cwd := t.TempDir()
	l := newFinderExecLocal(t, cwd)
	path := newTreeTestSession(t, l.root, cwd, finderExecMessages())
	finderSearchQuery(t, l.i, "refacto")
	l.i.finderOpenItem(finderExecItem(path), 0)
	runLoopCall(t, l.i)
	l.finishJump(t)

	// Pick the same session again: no reload, the jump runs directly.
	l.i.clearChatSearch()
	finderSearchQuery(t, l.i, "refacto")
	l.i.finderOpenItem(finderExecItem(path), 0)
	require.Empty(t, l.loaded)
	require.Empty(t, l.i.loopCalls)
	cur, ok := chatSearchCur(l.i)
	require.True(t, ok)
	require.Equal(t, 3, cur)
	require.Nil(t, l.i.pendingFinderJump)
	require.False(t, l.i.finder.Active())
}

func TestFinderOpenCurrentSessionJumpsInPlacePort(t *testing.T) {
	cwd := t.TempDir()
	root := t.TempDir()
	path := newTreeTestSession(t, root, cwd, finderExecMessages())
	i, _, _, _ := newTreePortInteractive(t, root, cwd, finderExecMessages())
	i.cfg.Theme, i.cfg.Terminal, i.cfg.CWD = tui.Dark, &alertTestTerminal{}, cwd
	i.mu.Lock()
	i.portInfo.SessionPath = path
	i.mu.Unlock()
	i.finder = newFinderModel(cwd)
	finderSearchQuery(t, i, "refacto")

	// Live row for the attached session itself: no attach, no client call.
	item := finderExecItem(path)
	item.Live, item.LiveID, item.LiveCWD = true, string(i.SessionID()), cwd
	i.finderOpenItem(item, 0)
	cur, ok := chatSearchCur(i)
	require.True(t, ok)
	require.Equal(t, 3, cur)
	require.Nil(t, i.pendingFinderJump)
}

func TestFinderOpenMarksLoadingUntilResolved(t *testing.T) {
	cwd := t.TempDir()
	l := newFinderExecLocal(t, cwd)
	path := newTreeTestSession(t, l.root, cwd, finderExecMessages())
	finderSearchQuery(t, l.i, "refacto")
	l.i.finderOpenItem(finderExecItem(path), 0)
	require.False(t, l.i.finder.Active())
	l.i.mu.Lock()
	loading, status := l.i.sessionLoading, l.i.statusOK
	l.i.mu.Unlock()
	require.True(t, loading, "busy guard covers the background lookup")
	require.Equal(t, "opening session…", status)
	require.False(t, l.i.canResumeSessionSelection())
	runLoopCall(t, l.i)
	l.finishJump(t)
	require.False(t, l.i.sessionLoading)
}

func TestFinderJumpWithoutHitSkipsChatSearch(t *testing.T) {
	cwd := t.TempDir()
	l := newFinderExecLocal(t, cwd)
	path := newTreeTestSession(t, l.root, cwd, finderExecMessages())
	finderSearchQuery(t, l.i, "zzznomatch")
	item := finderExecItem(path)
	item.Hits = nil // title-only match
	l.i.finderOpenItem(item, -1)
	runLoopCall(t, l.i)
	l.finishJump(t)
	_, ok := chatSearchCur(l.i)
	require.False(t, ok)
	require.Empty(t, l.i.statusErr, "no misleading no-match error")

	// A fork note is shown even without a message hit.
	l.i.pendingFinderJump = &pendingFinderJump{path: path, terms: []string{"zzznomatch"}, note: "forked into ~/x (new session)"}
	l.i.runPendingFinderJump(path)
	require.Equal(t, "forked into ~/x (new session)", l.i.statusOK)
	require.Empty(t, l.i.statusErr)
}

func TestChatSearchClearedWhenTranscriptReplaced(t *testing.T) {
	i, _ := chatSearchFixture(t)
	i.startChatSearch([]string{"watcher"}, 0)
	i.mu.Lock()
	i.hydrateLocked(session.Snapshot{Messages: []provider.Message{jumpTextMsg(provider.RoleUser, "x")}})
	i.mu.Unlock()
	_, ok := chatSearchCur(i)
	require.False(t, ok)
	// A stale step after the reset is a no-op, not an index panic.
	i.chatSearchNext()
	i.chatSearchJump()
}

func TestFinderPortForkFailureRemovesOrphan(t *testing.T) {
	cwd, other := t.TempDir(), t.TempDir()
	i, client, root := newFinderExecPort(t, cwd)
	path := newTreeTestSession(t, root, other, finderExecMessages())
	specs := expectPortCreate(t, client, path)
	i.finderOpenItem(finderExecItem(path), 0)
	runLoopCall(t, i)
	i.handleFinderResumeKey(tui.Key{Kind: tui.KeyRune, Rune: 'n'})
	runLoopCall(t, i)
	spec := <-specs
	_, err := os.Stat(spec.ResumePath)
	require.NoError(t, err, "fork written before resume")
	res := awaitResumeResult(t, i)
	require.Error(t, res.err)
	i.applySessionResult(context.Background(), res)
	_, err = os.Stat(spec.ResumePath)
	require.True(t, os.IsNotExist(err), "orphan fork removed")
	_, err = os.Stat(path)
	require.NoError(t, err, "original kept")
}
