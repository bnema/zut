package modes

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/agent/internal/sessionindex"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func writeFinderSession(t *testing.T, root, cwd, title string, texts ...string) string {
	t.Helper()
	s, err := core.NewSession(root, cwd, "p", "m", "v")
	require.NoError(t, err)
	for idx, text := range texts {
		role := provider.RoleUser
		if idx%2 == 1 {
			role = provider.RoleAssistant
		}
		require.NoError(t, s.AppendMessage(jumpTextMsg(role, text)))
	}
	path := s.Path
	require.NoError(t, s.Close())
	if title != "" {
		require.NoError(t, core.RenameSession(path, title))
	}
	return path
}

func newFinderTestIndex(t *testing.T, root string) *sessionindex.Index {
	t.Helper()
	index, err := sessionindex.Open(root, sessionindex.Options{Skip: isHiddenTranscriptMessage})
	require.NoError(t, err)
	t.Cleanup(func() { index.Close() })
	require.NoError(t, index.Update(context.Background(), nil))
	return index
}

func finderKeys(items []finderItem) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, item.Key)
	}
	return keys
}

func TestFinderSessionSourceContentQuery(t *testing.T) {
	root := t.TempDir()
	zut := writeFinderSession(t, root, "/work/zut", "advisor watcher", "let's refactor the watcher", "refactoring done", "more refactor talk")
	gordon := writeFinderSession(t, root, "/work/gordon", "api cleanup", "refactor the api", "ok")
	writeFinderSession(t, root, "/work/other", "unrelated", "nothing here")
	src := &finderSessionSource{index: newFinderTestIndex(t, root), now: time.Now}

	res, err := src.Search(context.Background(), finderQuery{Text: "refacto", CWD: "/work/zut"})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{zut, gordon}, finderKeys(res.Items))
	require.Equal(t, zut, res.Items[0].Key, "more hits rank first")
	require.Equal(t, 3, res.Items[0].Count)
	require.Len(t, res.Items[0].Hits, 3)
	require.Equal(t, "zut · advisor watcher", res.Items[0].Label)
	require.Equal(t, provider.RoleUser, res.Items[0].Hits[0].Role)
	ref, ok := res.Items[0].Hits[0].Ref.(sessionindex.HitRef)
	require.True(t, ok)
	require.Equal(t, zut, ref.Path)

	res, err = src.Search(context.Background(), finderQuery{Text: "refacto @here", Here: true, CWD: "/work/gordon"})
	require.NoError(t, err)
	require.Equal(t, []string{gordon}, finderKeys(res.Items))

	res, err = src.Search(context.Background(), finderQuery{Text: "refactor api", CWD: "/work/zut"})
	require.NoError(t, err)
	require.Equal(t, []string{gordon}, finderKeys(res.Items), "every token must be in the session")
}

func TestFinderSessionSourceTitleFitRanksFirst(t *testing.T) {
	root := t.TempDir()
	busy := writeFinderSession(t, root, "/work/a", "misc", "deploy deploy", "deploy", "deploy again", "deploy")
	titled := writeFinderSession(t, root, "/work/b", "deploy pipeline", "deploy once")
	src := &finderSessionSource{index: newFinderTestIndex(t, root), now: time.Now}

	// Title-only matches need Index.Recent (not available yet); here both
	// sessions match by content and the title fit outranks hit density.
	res, err := src.Search(context.Background(), finderQuery{Text: "deploy"})
	require.NoError(t, err)
	require.Equal(t, []string{titled, busy}, finderKeys(res.Items))
}

func TestFinderMessageSourceRequiresTokensInOneMessage(t *testing.T) {
	root := t.TempDir()
	same := writeFinderSession(t, root, "/work/a", "a", "alpha beta together", "x", "alpha again")
	writeFinderSession(t, root, "/work/b", "b", "alpha only", "beta only")
	src := &finderMessageSource{index: newFinderTestIndex(t, root)}

	res, err := src.Search(context.Background(), finderQuery{Text: "alpha beta"})
	require.NoError(t, err)
	require.Equal(t, []string{same}, finderKeys(res.Items))
	require.Equal(t, 1, res.Items[0].Count)

	res, err = src.Search(context.Background(), finderQuery{Text: "  @here "})
	require.NoError(t, err)
	require.Empty(t, res.Items)
}

func TestRankFinderSessionsFormula(t *testing.T) {
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	items := []finderItem{{Key: "old", Label: "x · misc", Count: 1}, {Key: "new", Label: "y · misc", Count: 1}, {Key: "dense", Label: "z · misc", Count: 50}}
	matches := []sessionindex.SessionMatch{
		{Doc: sessionindex.SessionDoc{Updated: now.Add(-60 * 24 * time.Hour)}},
		{Doc: sessionindex.SessionDoc{Updated: now}},
		{Doc: sessionindex.SessionDoc{Updated: now.Add(-60 * 24 * time.Hour)}},
	}
	rankFinderSessions(items, matches, "", now)
	require.Equal(t, []string{"dense", "new", "old"}, finderKeys(items))
}

func TestFinderOpenUpdatesIndexAndShowsProgress(t *testing.T) {
	root := t.TempDir()
	path := writeFinderSession(t, root, "/work/zut", "t", "needle in text")
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Terminal: &alertTestTerminal{}, CWD: "/work/zut", SessionsRoot: root})
	i.finderDelay, i.finderRetryDelay = 0, 0
	t.Cleanup(i.closeFinderIndex)
	t.Cleanup(i.closeFinder)

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlF})
	require.True(t, i.finder.Active())
	require.True(t, i.finder.indexing)
	require.Contains(t, stripANSIBytes(i.finder.Render(tui.Dark, 120, 20)[1]), "indexing")
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyPaste, Paste: "needle"})

	deadline := time.After(10 * time.Second)
	for len(i.finder.items) == 0 || i.finder.indexing || i.finder.loading {
		select {
		case fn := <-i.loopCalls:
			fn()
		case <-deadline:
			t.Fatal("index update and search did not finish")
		}
	}
	require.Equal(t, path, i.finder.items[0].Key)
	require.False(t, i.finderUpdating)

	i.closeFinderIndex()
	require.Nil(t, i.finderIndex)
}
