package modes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/agent/internal/sessionindex"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/tui"
)

func TestFinderRenameKeys(t *testing.T) {
	m := openFinderWithItems(t, finderItem{Key: "a", Label: "zut · old", Title: "old"}, finderItem{Key: "b"})
	m.HandleKey(tui.Key{Kind: tui.KeyCtrlR})
	require.Equal(t, "old", string(m.rename))
	for _, k := range []tui.Key{{Kind: tui.KeyBackspace}, {Kind: tui.KeyRune, Rune: 'X'}, {Kind: tui.KeyPaste, Paste: " new\nname"}} {
		require.Equal(t, finderActionNone, m.HandleKey(k).Kind)
	}
	require.Equal(t, "olX new name", string(m.rename))
	require.Empty(t, m.Query(), "rename keys do not edit the query")

	lines := m.Render(tui.Dark, 80, 12)
	require.Contains(t, stripANSIBytes(lines[finderQueryRow]), "rename: olX new name · enter save · esc cancel")
	row, col := m.CursorPos()
	require.Equal(t, finderQueryRow, row)
	require.Equal(t, len(" rename: olX new name"), col)

	// Results arriving mid-rename keep the target.
	act := m.Refresh()
	require.True(t, m.ApplyResults(act.Gen, act.TabID, finderResults{Items: []finderItem{{Key: "b"}, {Key: "a", Title: "old"}}}, nil))
	act = m.HandleKey(tui.Key{Kind: tui.KeyEnter})
	require.Equal(t, finderActionRename, act.Kind)
	require.Equal(t, "a", act.Item.Key)
	require.Equal(t, "olX new name", act.Title)
	require.NotEqual(t, finderModeRename, m.mode)

	m.HandleKey(tui.Key{Kind: tui.KeyCtrlR})
	require.Equal(t, finderActionNone, m.HandleKey(tui.Key{Kind: tui.KeyEsc}).Kind)
	require.NotEqual(t, finderModeRename, m.mode)
	require.True(t, m.Active(), "esc cancels the rename, not the finder")
}

func TestFinderRenameLongTitleKeepsCaretVisible(t *testing.T) {
	m := openFinderWithItems(t, finderItem{Key: "a", Title: strings.Repeat("long title ", 20)})
	m.HandleKey(tui.Key{Kind: tui.KeyCtrlR})
	lines := m.Render(tui.Dark, 60, 12)
	_, col := m.CursorPos()
	require.Less(t, col, 59)
	require.Contains(t, stripANSIBytes(lines[finderQueryRow]), "long title")
	requireExactWidth(t, lines, 60)
}

func TestFinderRemoveAndRenameItem(t *testing.T) {
	m := openFinderWithItems(t, finderItem{Key: "a", Label: "p · x"}, finderItem{Key: "b", Label: "q · y"})
	m.HandleKey(tui.Key{Kind: tui.KeyDown})
	m.RenameItem("b", "new")
	require.Equal(t, "q · new", m.items[1].Label)
	require.Equal(t, "q · new", m.raw[1].Label)
	m.RemoveItem("b")
	require.Equal(t, []string{"a"}, finderKeys(m.items))
	require.Equal(t, 0, m.cursor)
	m.RemoveItem("a")
	require.Empty(t, m.items)
	require.Equal(t, 0, m.cursor)
}

func newFinderActionInteractive(t *testing.T, root string, current string) *Interactive {
	t.Helper()
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Terminal: &alertTestTerminal{}, CWD: "/work/a", SessionsRoot: root,
		CurrentSessionPath: func() string { return current }})
	i.finderDelay, i.finderRetryDelay = 0, 0
	t.Cleanup(i.closeFinderIndex)
	t.Cleanup(i.closeFinder)
	return i
}

func openSettledFinder(t *testing.T, i *Interactive, query string, n int) {
	t.Helper()
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlF})
	if query != "" {
		i.handleKey(context.Background(), tui.Key{Kind: tui.KeyPaste, Paste: query})
	}
	driveFinderLoop(t, i, func() bool { return !i.finder.indexing && !i.finder.loading && len(i.finder.items) == n })
}

func TestFinderRenameLocalSession(t *testing.T) {
	root := t.TempDir()
	path := writeFinderSession(t, root, "/work/a", "old title", "marker text")
	i := newFinderActionInteractive(t, root, "")
	openSettledFinder(t, i, "", 1)
	ctx := context.Background()

	i.handleKey(ctx, tui.Key{Kind: tui.KeyCtrlR})
	i.handleKey(ctx, tui.Key{Kind: tui.KeyCtrlU})
	i.handleKey(ctx, tui.Key{Kind: tui.KeyPaste, Paste: "brand new"})
	i.handleKey(ctx, tui.Key{Kind: tui.KeyEnter})
	require.Empty(t, i.statusErr)
	require.Equal(t, "renamed session: brand new", i.statusOK)
	require.Equal(t, "a · brand new", i.finder.items[0].Label)

	require.Equal(t, "brand new", core.DescribeSession(path).Title)

	// The background index update makes the new title searchable.
	driveFinderLoop(t, i, func() bool { return !i.finderUpdating && !i.finder.loading })
	i.handleKey(ctx, tui.Key{Kind: tui.KeyPaste, Paste: "brand"})
	driveFinderLoop(t, i, func() bool { return !i.finder.loading })
	require.Equal(t, []string{path}, finderKeys(i.finder.items))
}

func TestFinderRenameRejectsEmptyTitle(t *testing.T) {
	root := t.TempDir()
	path := writeFinderSession(t, root, "/work/a", "keep", "x")
	i := newFinderActionInteractive(t, root, "")
	openSettledFinder(t, i, "", 1)
	ctx := context.Background()

	i.handleKey(ctx, tui.Key{Kind: tui.KeyCtrlR})
	i.handleKey(ctx, tui.Key{Kind: tui.KeyCtrlU})
	i.handleKey(ctx, tui.Key{Kind: tui.KeyPaste, Paste: "   "})
	i.handleKey(ctx, tui.Key{Kind: tui.KeyEnter})
	require.Equal(t, "rename: session title is empty", i.statusErr)
	require.Equal(t, "keep", core.DescribeSession(path).Title)
}

func TestFinderDeleteRemovesFileItemAndIndexEntry(t *testing.T) {
	root := t.TempDir()
	gone := writeFinderSession(t, root, "/work/a", "doomed", "uniquemarker")
	keep := writeFinderSession(t, root, "/work/a", "kept", "uniquemarker too")
	i := newFinderActionInteractive(t, root, "")
	openSettledFinder(t, i, "uniquemarker", 2)
	ctx := context.Background()
	for i.finder.items[i.finder.cursor].Key != gone {
		i.handleKey(ctx, tui.Key{Kind: tui.KeyDown})
	}

	i.handleKey(ctx, tui.Key{Kind: tui.KeyCtrlD})
	i.handleKey(ctx, tui.Key{Kind: tui.KeyRune, Rune: 'y'})
	require.Empty(t, i.statusErr)
	require.NoFileExists(t, gone)
	require.Equal(t, []string{keep}, finderKeys(i.finder.items))
	require.Equal(t, 0, i.finder.cursor)

	require.Eventually(t, func() bool {
		res, err := i.finderIndex.Query(ctx, sessionindex.Query{Text: "uniquemarker"})
		return err == nil && len(res.Sessions) == 1 && res.Sessions[0].Doc.Path == keep
	}, 5*time.Second, 10*time.Millisecond)
}

func TestFinderDeleteProtectsCurrentSession(t *testing.T) {
	root := t.TempDir()
	path := writeFinderSession(t, root, "/work/a", "current", "x")
	i := newFinderActionInteractive(t, root, path)
	openSettledFinder(t, i, "", 1)

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlD})
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: 'y'})
	require.Equal(t, "cannot delete the active session; resume another session first", i.statusErr)
	require.FileExists(t, path)
	require.Len(t, i.finder.items, 1)
}

func TestFinderProtectsLiveSessionInPortMode(t *testing.T) {
	root := t.TempDir()
	path := writeFinderSession(t, root, "/work/a", "running elsewhere", "x")
	client := newMockSessionClient(t)
	client.EXPECT().List(mock.Anything).Return([]session.Info{{ID: "other", SessionPath: path}}, nil).Once()
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Terminal: &alertTestTerminal{}, Port: sessionmocks.NewMockPort(t), Client: client, CWD: "/work/a", SessionsRoot: root})
	i.finderDelay, i.finderRetryDelay = 0, 0
	t.Cleanup(i.closeFinderIndex)
	t.Cleanup(i.closeFinder)
	ctx := context.Background()
	i.handleKey(ctx, tui.Key{Kind: tui.KeyCtrlF})
	driveFinderLoop(t, i, func() bool {
		return !i.finder.indexing && !i.finder.loading && len(i.finder.items) == 1 && i.finder.items[0].Live
	})

	i.handleKey(ctx, tui.Key{Kind: tui.KeyCtrlD})
	i.handleKey(ctx, tui.Key{Kind: tui.KeyRune, Rune: 'y'})
	require.Equal(t, "cannot delete a running session; close it first", i.statusErr)
	require.FileExists(t, path)

	i.handleKey(ctx, tui.Key{Kind: tui.KeyCtrlR})
	i.handleKey(ctx, tui.Key{Kind: tui.KeyRune, Rune: '!'})
	i.handleKey(ctx, tui.Key{Kind: tui.KeyEnter})
	require.Equal(t, "cannot rename a running session from here; attach to it first", i.statusErr)
	require.Equal(t, "running elsewhere", core.DescribeSession(path).Title)
}

func TestFinderRefreshKeepsHitAndOverlays(t *testing.T) {
	hits := []finderHit{{Ref: 1}, {Ref: 2}, {Ref: 3}}
	stale := []finderItem{{Key: "a", Label: "p · old", Hits: hits}, {Key: "gone", Label: "q · x"}, {Key: "c", Label: "r · y"}}
	m := openFinderWithItems(t, stale...)
	m.HandleKey(tui.Key{Kind: tui.KeyCtrlN})
	m.HandleKey(tui.Key{Kind: tui.KeyCtrlN})
	require.Equal(t, 2, m.hit)
	seq := m.overlaySeq
	m.RenameItem("a", "new")
	m.RemoveItem("gone")

	// A refresh from an index generation older than the actions.
	act := m.Refresh()
	require.True(t, m.ApplyResults(act.Gen, act.TabID, finderResults{Items: stale}, nil))
	require.Equal(t, []string{"a", "c"}, finderKeys(m.items))
	require.Equal(t, "p · new", m.items[0].Label)
	require.Equal(t, "new", m.items[0].Title)
	require.Equal(t, 2, m.hit, "hit cursor survives a refresh of the same item")

	require.False(t, m.ClearOverlay(seq), "an update that started before the actions does not clear them")
	require.True(t, m.ClearOverlay(m.overlaySeq))
	act = m.Refresh()
	require.True(t, m.ApplyResults(act.Gen, act.TabID, finderResults{Items: stale[:1]}, nil))
	require.Equal(t, "p · old", m.items[0].Label)

	// Fewer hits than the old cursor: reset.
	act = m.Refresh()
	require.True(t, m.ApplyResults(act.Gen, act.TabID, finderResults{Items: []finderItem{{Key: "a", Hits: hits[:1]}}}, nil))
	require.Equal(t, 0, m.hit)
}

func TestFinderDeleteErrorKeepsRow(t *testing.T) {
	root := t.TempDir()
	path := writeFinderSession(t, root, "/work/a", "keep", "x")
	i := newFinderActionInteractive(t, root, "")
	openSettledFinder(t, i, "", 1)
	// A directory where the file was makes DeleteSession fail.
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	require.NoError(t, os.WriteFile(path+"/child", nil, 0o600))

	item := i.finder.items[0]
	i.finderDeleteNow(context.Background(), item)
	require.Contains(t, i.statusErr, "delete session:")
	require.Equal(t, []string{path}, finderKeys(i.finder.items))
}

func TestFinderDeleteSurvivesStaleIndexRefresh(t *testing.T) {
	root := t.TempDir()
	gone := writeFinderSession(t, root, "/work/a", "doomed", "x")
	keep := writeFinderSession(t, root, "/work/a", "kept", "y")
	i := newFinderActionInteractive(t, root, "")
	openSettledFinder(t, i, "", 2)
	ctx := context.Background()
	for i.finder.items[i.finder.cursor].Key != gone {
		i.handleKey(ctx, tui.Key{Kind: tui.KeyDown})
	}
	i.handleKey(ctx, tui.Key{Kind: tui.KeyCtrlD})
	i.handleKey(ctx, tui.Key{Kind: tui.KeyRune, Rune: 'y'})
	require.Equal(t, []string{keep}, finderKeys(i.finder.items))

	// Results that still list the deleted session (an older generation).
	act := i.finder.Refresh()
	require.True(t, i.finder.ApplyResults(act.Gen, act.TabID, finderResults{Items: []finderItem{{Key: gone}, {Key: keep}}}, nil))
	require.Equal(t, []string{keep}, finderKeys(i.finder.items))

	// The queued index update clears the overlay once it has run.
	driveFinderLoop(t, i, func() bool { return i.finder.deleted == nil && !i.finderUpdating && !i.finder.loading })
	require.Equal(t, []string{keep}, finderKeys(i.finder.items))
}

func TestFinderRefusesKeysOutsideSessionsRoot(t *testing.T) {
	root := t.TempDir()
	writeFinderSession(t, root, "/work/a", "inside", "x")
	outside := filepath.Join(t.TempDir(), "other.jsonl")
	require.NoError(t, os.WriteFile(outside, []byte("{}\n"), 0o600))
	i := newFinderActionInteractive(t, root, "")
	openSettledFinder(t, i, "", 1)

	item := finderItem{Key: outside, Label: "x · y"}
	i.finderDelete(context.Background(), item)
	require.Contains(t, i.statusErr, "finder: refusing to modify")
	require.FileExists(t, outside)
	i.finderRename(context.Background(), item, "new")
	require.Contains(t, i.statusErr, "finder: refusing to modify")
}

func TestFinderPortDeleteRelistsBeforeDeleting(t *testing.T) {
	root := t.TempDir()
	path := writeFinderSession(t, root, "/work/a", "idle at open", "x")
	client := newMockSessionClient(t)
	client.EXPECT().List(mock.Anything).Return(nil, nil).Once()
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Terminal: &alertTestTerminal{}, Port: sessionmocks.NewMockPort(t), Client: client, CWD: "/work/a", SessionsRoot: root})
	i.finderDelay, i.finderRetryDelay = 0, 0
	t.Cleanup(i.closeFinderIndex)
	t.Cleanup(i.closeFinder)
	ctx := context.Background()
	i.handleKey(ctx, tui.Key{Kind: tui.KeyCtrlF})
	driveFinderLoop(t, i, func() bool {
		return !i.finder.indexing && !i.finder.loading && len(i.finder.items) == 1 && i.finder.liveLoaded
	})
	require.False(t, i.finder.items[0].Live)

	// It started running elsewhere since the finder opened.
	client.EXPECT().List(mock.Anything).Return([]session.Info{{ID: "other", SessionPath: path}}, nil).Once()
	i.handleKey(ctx, tui.Key{Kind: tui.KeyCtrlD})
	i.handleKey(ctx, tui.Key{Kind: tui.KeyRune, Rune: 'y'})
	driveFinderLoop(t, i, func() bool { return i.statusErr != "" })
	require.Equal(t, "cannot delete a running session; close it first", i.statusErr)
	require.FileExists(t, path)
	require.True(t, i.finder.items[0].Live)

	// A failed re-list refuses too.
	i.finder.items[0].Live = false
	i.statusErr = ""
	client.EXPECT().List(mock.Anything).Return(nil, errors.New("down")).Once()
	i.finderDelete(ctx, i.finder.items[0])
	driveFinderLoop(t, i, func() bool { return i.statusErr != "" })
	require.Equal(t, "finder: list running sessions: down; delete refused", i.statusErr)
	require.FileExists(t, path)
}

func TestFinderSearchFromPreviousModelIsDropped(t *testing.T) {
	src := newTestFinderSource(t, "sessions")
	release := make(chan struct{})
	src.EXPECT().Search(mock.Anything, finderQueryText("")).RunAndReturn(func(context.Context, finderQuery) (finderResults, error) {
		<-release
		return finderResults{Items: []finderItem{{Key: "stale"}}}, nil
	}).Once()
	i := newFinderInteractive(t, src)
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlF})
	first := i.finder
	// Reopen without cancelling: a new model whose gen matches the old one.
	i.finder = newFinderModel("/work", src)
	act := i.finder.Open(0)
	require.Equal(t, first.Generation(), act.Gen)
	i.finderCancel = nil // keep the first search running
	close(release)
	runFinderLoopCall(t, i)
	require.Empty(t, i.finder.items)
	require.True(t, i.finder.loading)
}

func TestFinderAnimatesWhileSearchingOrIndexing(t *testing.T) {
	i := &Interactive{}
	redraws := 0
	tick := func() { i.redrawAnimationTick(func() { redraws++ }) }
	tick()
	require.Zero(t, redraws)
	i.finder = newFinderModel("/w", newTestFinderSource(t, "sessions"))
	i.finder.Open(0)
	tick()
	require.Equal(t, 1, redraws, "searching")
	i.finder.loading = false
	i.finder.SetIndexing(true, 1, 2)
	tick()
	require.Equal(t, 2, redraws, "indexing")
	i.finder.SetIndexing(false, 0, 0)
	tick()
	require.Equal(t, 2, redraws)
}
