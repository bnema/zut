package modes

import (
	"context"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Daemon attachment supplies neither a local agent nor persistence callbacks.
// Exercise the gesture, actual tree selection, switch completion and resend.
func TestPortDoubleEscapeRestoresEditableDraft(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	file, err := core.NewSession(root, cwd, "test", "model", "test")
	require.NoError(t, err)
	msgs := []provider.Message{portUserMessage("first"), {Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "answer"}}}, portUserMessage("edit me")}
	for _, msg := range msgs {
		require.NoError(t, file.AppendMessage(msg))
	}
	path := file.Path
	require.NoError(t, file.Close())
	client := newMockSessionClient(t)
	oldPort, nextPort := sessionmocks.NewMockPort(t), sessionmocks.NewMockPort(t)
	client.EXPECT().Create(mock.Anything, session.CreateSpec{CWD: cwd, Fork: &session.ForkSpec{ParentPath: path, UpToMessage: 2, Hidden: true}}).Return(session.Info{ID: "branch"}, nil).Once()
	client.EXPECT().Attach(mock.Anything, session.ID("branch")).Return(nextPort, nil).Once()
	nextPort.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "branch", CWD: cwd, Run: session.RunStateIdle}, Messages: msgs[:2]}, nil).Once()
	nextPort.EXPECT().Subscribe(mock.Anything, mock.Anything).Return(make(chan session.Event), nil).Once()
	i := NewInteractive(InteractiveConfig{Port: oldPort, Client: client, SessionsRoot: root})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	i.runCtx = ctx
	i.mu.Lock()
	i.hydrateLocked(session.Snapshot{Info: session.Info{ID: "original", CWD: cwd, SessionPath: path, Run: session.RunStateIdle}, Messages: msgs})
	i.mu.Unlock()
	require.Nil(t, i.agent)
	require.Nil(t, i.cfg.CurrentSessionPath)
	require.Nil(t, i.cfg.LoadSession)
	now := time.Unix(100, 0)
	i.clock = func() time.Time { return now }
	i.handleKey(ctx, tui.Key{Kind: tui.KeyEsc})
	require.False(t, i.sessionTreeDialog.Active())
	now = now.Add(100 * time.Millisecond)
	i.handleKey(ctx, tui.Key{Kind: tui.KeyEsc})
	require.True(t, i.sessionTreeDialog.Active())
	require.NoError(t, applySessionTreeLoads(i.sessionTreeDialog, i.sessionTreeLoads))
	found := false
	for idx, item := range i.sessionTreeDialog.items {
		if item.target.Role == provider.RoleUser && item.target.EffectiveIndex == 2 {
			i.sessionTreeDialog.cursor = idx
			found = true
			break
		}
	}
	require.True(t, found, "tree must contain the selected user message")
	i.handleKey(ctx, tui.Key{Kind: tui.KeyEnter})
	require.True(t, i.sessionLoading)
	require.Empty(t, i.ed.Value(), "draft must wait for successful checkout")
	i.ed.SetValue("input while switching")
	i.handleKey(ctx, tui.Key{Kind: tui.KeyEnter})
	require.Equal(t, "input while switching", i.ed.Value(), "Enter must not submit to the old daemon session")
	select {
	case res := <-i.sessionResults:
		require.NoError(t, res.err)
		require.False(t, i.applySessionResult(ctx, res))
	case <-time.After(5 * time.Second):
		t.Fatal("switch did not complete")
	}
	require.Equal(t, session.ID("branch"), i.SessionID())
	require.Equal(t, msgs[:2], i.view.Messages)
	require.Equal(t, "edit me", i.ed.Value())
	require.False(t, i.sessionLoading)
	i.ed.SetValue("edited draft")
	submitted := make(chan session.Input, 1)
	nextPort.EXPECT().Submit(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in session.Input) error { submitted <- in; return nil }).Once()
	i.handleKey(ctx, tui.Key{Kind: tui.KeyEnter})
	select {
	case in := <-submitted:
		require.Equal(t, "edited draft", in.Text)
	case <-time.After(5 * time.Second):
		t.Fatal("edited draft not submitted to daemon")
	}
}

func TestPortSessionTreeGatesUseDaemonState(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	client := newMockSessionClient(t)
	i := NewInteractive(InteractiveConfig{Port: port, Client: client, CWD: t.TempDir(), SessionsRoot: t.TempDir()})
	i.portInfo = session.Info{Run: session.RunStateIdle, SessionPath: "/session.jsonl"}
	for _, gate := range []func() bool{i.canArmSessionTreeEscape, i.canOpenSessionTree, i.canCommitSessionTreeSelection} {
		require.True(t, gate())
		i.portQueue = []session.QueuedInput{{}}
		require.False(t, gate(), "daemon queue must block checkout")
		i.portQueue = nil
		i.portInfo.Run = session.RunStateRunning
		require.False(t, gate())
		i.portInfo.Run = session.RunStateIdle
		i.sessionLoading = true
		require.False(t, gate())
		i.sessionLoading = false
		i.portInfo.SessionPath = ""
		require.False(t, gate())
		i.portInfo.SessionPath = "/session.jsonl"
	}
}

func TestPortTreeSwitchFailureDoesNotRestoreDraft(t *testing.T) {
	i := NewInteractive(InteractiveConfig{Port: sessionmocks.NewMockPort(t)})
	i.sessionID = "original"
	i.sessionLoading = true
	i.ed.SetValue("existing draft")
	i.applySessionResult(context.Background(), sessionResult{kind: sessionResultSwitch, gen: i.sessionGen, treeSelection: &sessionTreeSelectionResult{restoreDraft: true, draftText: "selected draft"}, err: context.Canceled})
	require.Equal(t, session.ID("original"), i.SessionID())
	require.Equal(t, "existing draft", i.ed.Value())
	require.False(t, i.sessionLoading)
	require.Contains(t, i.statusErr, "canceled")
}

func TestPortTreeCheckoutBlocksSlashSuggestionAndReleasesStaleGate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := newMockSessionClient(t)
	oldPort := sessionmocks.NewMockPort(t)
	i := NewInteractive(InteractiveConfig{Port: oldPort, Client: client})
	i.runCtx = ctx
	i.sessionID = "original"
	entered, release := make(chan struct{}), make(chan struct{})
	client.EXPECT().Create(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, session.CreateSpec) (session.Info, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return session.Info{}, context.Canceled
	}).Once()
	i.portSwitchWithTreeSelection("tree", func(ctx context.Context, client SessionClient) (session.ID, error) {
		info, err := client.Create(ctx, session.CreateSpec{})
		return session.ID(info.ID), err
	}, &sessionTreeSelectionResult{restoreDraft: true, draftText: "selected draft"})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("checkout did not start")
	}
	i.ed.SetValue("/clear")
	require.True(t, i.suggest.Active(i.ed.Value()), "exercise slash-suggestion dispatch")
	require.False(t, i.handleKey(ctx, tui.Key{Kind: tui.KeyEnter}))
	require.Equal(t, "/clear", i.ed.Value())
	require.True(t, i.sessionLoading)
	// Modified Enter still belongs to the multiline editor during checkout.
	i.handleKey(ctx, tui.Key{Kind: tui.KeyEnter, Shift: true})
	require.Contains(t, i.ed.Value(), "\n")
	// Model a successor/takeover arriving before the pending checkout result.
	i.mu.Lock()
	i.sessionGen++
	i.sessionID = "successor"
	i.mu.Unlock()
	close(release)
	select {
	case res := <-i.sessionResults:
		released := false
		res.cancel = func() { released = true }
		require.False(t, i.applySessionResult(ctx, res))
		require.True(t, released)
	case <-time.After(5 * time.Second):
		t.Fatal("checkout result did not arrive")
	}
	require.False(t, i.sessionLoading, "stale checkout must release the Enter gate")
	require.Equal(t, session.ID("successor"), i.SessionID())
	require.NotEqual(t, "selected draft", i.ed.Value())
	// No Clear or Submit expectation exists on either mock: dispatching to the
	// old session fails this test, including asynchronous calls.
}

func TestPortTreeUnknownParentKeepsSessionAndDraft(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	file, err := core.NewSession(root, cwd, "test", "model", "test")
	require.NoError(t, err)
	require.NoError(t, file.AppendMessage(portUserMessage("first")))
	path := file.Path
	require.NoError(t, file.Close())
	client := newMockSessionClient(t)
	client.EXPECT().Create(mock.Anything, session.CreateSpec{CWD: cwd, Fork: &session.ForkSpec{ParentPath: path, UpToMessage: 0, Hidden: true}}).Return(session.Info{}, session.ErrForkParentNotFound).Once()
	i := NewInteractive(InteractiveConfig{Port: sessionmocks.NewMockPort(t), Client: client, SessionsRoot: root})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	i.runCtx = ctx
	i.mu.Lock()
	i.hydrateLocked(session.Snapshot{Info: session.Info{ID: "original", CWD: cwd, SessionPath: path, Run: session.RunStateIdle}, Messages: []provider.Message{portUserMessage("first")}})
	i.mu.Unlock()
	i.ed.SetValue("existing draft")
	i.applySessionTreeMessageSelection(path, 0, 1, provider.RoleUser, "first")
	select {
	case res := <-i.sessionResults:
		require.ErrorIs(t, res.err, session.ErrForkParentNotFound)
		require.False(t, i.applySessionResult(ctx, res))
	case <-time.After(5 * time.Second):
		t.Fatal("tree fork did not complete")
	}
	require.Equal(t, session.ID("original"), i.SessionID())
	require.Equal(t, "existing draft", i.ed.Value())
	require.Contains(t, i.statusErr, "parent session not found")
	require.False(t, i.sessionLoading)
}

func TestPortHistoricalTreeCarriesOriginalParentIdentity(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	file, err := core.NewSession(root, cwd, "test", "model", "test")
	require.NoError(t, err)
	require.NoError(t, file.AppendMessage(portUserMessage("old prompt")))
	require.NoError(t, file.AppendCompaction([]provider.Message{portUserMessage("summary")}))
	path := file.Path
	require.NoError(t, file.Close())
	client := newMockSessionClient(t)
	client.EXPECT().Create(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, spec session.CreateSpec) (session.Info, error) {
		require.NotNil(t, spec.Fork)
		require.Equal(t, path, spec.Fork.ParentPath)
		require.True(t, spec.Fork.Hidden)
		require.NotEmpty(t, spec.ResumePath)
		require.NotEqual(t, path, spec.ResumePath)
		snapshot, err := core.ReadSessionSnapshot(spec.ResumePath)
		require.NoError(t, err)
		require.Len(t, snapshot.Messages, 1)
		return session.Info{}, session.ErrForkParentNotFound
	}).Once()
	i := NewInteractive(InteractiveConfig{Port: sessionmocks.NewMockPort(t), Client: client, SessionsRoot: root, CWD: cwd})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	i.runCtx = ctx
	i.mu.Lock()
	i.hydrateLocked(session.Snapshot{Info: session.Info{ID: "original", CWD: cwd, SessionPath: path, Run: session.RunStateIdle}, Messages: []provider.Message{portUserMessage("summary")}})
	i.mu.Unlock()
	i.applySessionTreeTarget(sessionTreeTarget{SourcePath: path, EffectiveIndex: 0, SelectionBoundary: 1, Role: provider.RoleAssistant, Boundary: sessionTreeMessageBoundary, Historical: true, HistorySegment: 0}, 1)
	select {
	case res := <-i.sessionResults:
		require.ErrorIs(t, res.err, session.ErrForkParentNotFound)
		require.False(t, i.applySessionResult(ctx, res))
	case <-time.After(5 * time.Second):
		t.Fatal("historical tree fork did not complete")
	}
	require.Equal(t, session.ID("original"), i.SessionID())
	require.Contains(t, i.statusErr, "parent session not found")
}
