package modes

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/tui"
)

func TestFinderLiveItems(t *testing.T) {
	items := finderLiveItems([]session.Info{
		{ID: "l1", SessionPath: "/s/a/../a/one.jsonl", RepoRoot: "/work/zut", Title: "Run\x1b[31m", UpdatedAt: time.Now()},
		{ID: "l2", CWD: "/work/gordon"}, // not persisted
		{ID: "l3", SessionPath: "/s/b.jsonl", CWD: "/work/gordon"},
	})
	require.Len(t, items, 2)
	require.Equal(t, finderItem{Key: "/s/a/one.jsonl", Label: "zut · Run", Detail: "just now", Live: true, LiveID: "l1", Title: "Run"}, items[0])
	require.Equal(t, "gordon · (new session)", items[1].Label)
	require.Equal(t, "l3", items[1].LiveID)
	require.Equal(t, "/work/gordon", items[1].LiveCWD)
}

func TestFinderDecorateLive(t *testing.T) {
	m := newFinderModel("/work", &finderSessionSource{}, newTestFinderSource(t, "messages"))
	act := m.Open(0)
	raw := []finderItem{{Key: "/s/old.jsonl", Label: "old"}, {Key: "/s/./live.jsonl", Label: "indexed live", Hits: []finderHit{{Ref: 1}}}}
	require.True(t, m.ApplyResults(act.Gen, act.TabID, finderResults{Items: raw}, nil))
	m.HandleKey(tui.Key{Kind: tui.KeyDown})
	require.True(t, m.ApplySnippets(m.ResultsVersion(), "/s/./live.jsonl", map[int]string{0: "snippet"}))

	m.SetLive([]finderItem{
		{Key: "/s/live.jsonl", Live: true, LiveID: "l1", LiveCWD: "/work/live"},
		{Key: "/s/new.jsonl", Label: "unindexed", Live: true, LiveID: "l2"},
	})
	require.Equal(t, []string{"/s/new.jsonl", "/s/old.jsonl", "/s/./live.jsonl"}, finderKeys(m.items))
	require.Equal(t, "l2", m.items[0].LiveID)
	require.False(t, m.items[1].Live)
	require.True(t, m.items[2].Live)
	require.Equal(t, "l1", m.items[2].LiveID)
	require.Equal(t, "/work/live", m.items[2].LiveCWD, "indexed live rows carry the daemon cwd")
	require.Equal(t, 2, m.cursor, "cursor stays on the same key")
	require.Equal(t, "snippet", m.items[2].Hits[0].Text, "loaded snippets survive")
	require.Equal(t, "indexed live", m.items[2].Label)

	// A one-rune query has no index tokens: still the recent list, so live
	// sessions stay on top.
	act = finderRunes(m, "a")
	require.True(t, m.ApplyResults(act.Gen, act.TabID, finderResults{Items: raw}, nil))
	require.Equal(t, "/s/new.jsonl", m.items[0].Key)
	m.HandleKey(tui.Key{Kind: tui.KeyCtrlU})

	// A query keeps live marks but does not prepend unindexed sessions.
	act = finderRunes(m, "xy")
	require.True(t, m.ApplyResults(act.Gen, act.TabID, finderResults{Items: raw}, nil))
	require.Equal(t, []string{"/s/old.jsonl", "/s/./live.jsonl"}, finderKeys(m.items))
	require.True(t, m.items[1].Live)

	// Other tabs never prepend.
	m.HandleKey(tui.Key{Kind: tui.KeyCtrlU})
	act = m.HandleKey(tui.Key{Kind: tui.KeyTab})
	require.Equal(t, "messages", act.TabID)
	require.True(t, m.ApplyResults(act.Gen, act.TabID, finderResults{Items: raw}, nil))
	require.Equal(t, []string{"/s/old.jsonl", "/s/./live.jsonl"}, finderKeys(m.items))
}

func newFinderPortInteractive(t *testing.T, client SessionClient, root string) *Interactive {
	t.Helper()
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Terminal: &alertTestTerminal{}, Port: sessionmocks.NewMockPort(t), Client: client, CWD: "/work/a", SessionsRoot: root})
	i.finderDelay, i.finderRetryDelay = 0, 0
	t.Cleanup(i.closeFinderIndex)
	t.Cleanup(i.closeFinder)
	return i
}

func TestFinderPortModeMarksLiveSessions(t *testing.T) {
	root := t.TempDir()
	indexed := writeFinderSession(t, root, "/work/a", "indexed", "hello")
	client := newMockSessionClient(t)
	client.EXPECT().List(mock.Anything).Return([]session.Info{
		{ID: "live-indexed", SessionPath: indexed, CWD: "/work/a"},
		{ID: "live-new", SessionPath: root + "/sessions/x/new.jsonl", CWD: "/work/b", Title: "fresh"},
	}, nil).Once()
	i := newFinderPortInteractive(t, client, root)

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlF})
	driveFinderLoop(t, i, func() bool {
		return !i.finder.indexing && !i.finder.loading && len(i.finder.items) == 2
	})
	require.Equal(t, []string{root + "/sessions/x/new.jsonl", indexed}, finderKeys(i.finder.items))
	require.Equal(t, "b · fresh", i.finder.items[0].Label)
	require.Equal(t, "live-new", i.finder.items[0].LiveID)
	require.True(t, i.finder.items[1].Live)
	require.Equal(t, "live-indexed", i.finder.items[1].LiveID)
}

func TestFinderPortModeListErrorLeavesNoLiveMarks(t *testing.T) {
	root := t.TempDir()
	writeFinderSession(t, root, "/work/a", "indexed", "hello")
	client := newMockSessionClient(t)
	listed := make(chan struct{})
	client.EXPECT().List(mock.Anything).RunAndReturn(func(context.Context) ([]session.Info, error) {
		close(listed)
		return nil, errors.New("daemon down")
	}).Once()
	i := newFinderPortInteractive(t, client, root)

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlF})
	<-listed
	driveFinderLoop(t, i, func() bool { return !i.finder.indexing && !i.finder.loading && len(i.finder.items) == 1 })
	require.False(t, i.finder.items[0].Live)
	driveFinderLoop(t, i, func() bool { return i.statusErr != "" })
	require.Equal(t, "finder: list running sessions: daemon down", i.statusErr)
	require.False(t, i.finder.liveLoaded)

	// Without the live list, delete and rename stay refused.
	path := i.finder.items[0].Key
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlD})
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: 'y'})
	require.Equal(t, "finder: running sessions are not known yet; try again", i.statusErr)
	require.FileExists(t, path)
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlR})
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: '!'})
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	require.Equal(t, "finder: running sessions are not known yet; try again", i.statusErr)
}

func TestFinderLocalModeSkipsLiveList(t *testing.T) {
	root := t.TempDir()
	writeFinderSession(t, root, "/work/a", "indexed", "hello")
	client := newMockSessionClient(t) // no List expectation: must not be called
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Terminal: &alertTestTerminal{}, Client: client, CWD: "/work/a", SessionsRoot: root})
	i.finderDelay, i.finderRetryDelay = 0, 0
	t.Cleanup(i.closeFinderIndex)
	t.Cleanup(i.closeFinder)

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlF})
	driveFinderLoop(t, i, func() bool { return !i.finder.indexing && !i.finder.loading && len(i.finder.items) == 1 })
	require.False(t, i.finder.items[0].Live)
}
