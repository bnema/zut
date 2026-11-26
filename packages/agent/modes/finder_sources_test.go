package modes

import (
	"context"
	"os"
	"strings"
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

	res, err = src.Search(context.Background(), finderQuery{Text: "refacto @here", CWD: "/work/gordon"})
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

	res, err := src.Search(context.Background(), finderQuery{Text: "deploy pipeline"})
	require.NoError(t, err)
	require.Equal(t, []string{titled}, finderKeys(res.Items), "title-only match")
	require.Zero(t, res.Items[0].Count)
	require.Empty(t, res.Items[0].Hits)

	res, err = src.Search(context.Background(), finderQuery{Text: "gordon"})
	require.NoError(t, err)
	require.Empty(t, res.Items)

	// Both match by content; the title fit outranks hit density.
	res, err = src.Search(context.Background(), finderQuery{Text: "deploy"})
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
	require.Equal(t, "type to search messages", res.Hint)
}

func TestFinderSessionSourceEmptyQueryListsRecent(t *testing.T) {
	root := t.TempDir()
	older := writeFinderSession(t, root, "/work/zut", "older", "x")
	newer := writeFinderSession(t, root, "/work/gordon", "newer", "y")
	past := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(older, past, past))
	src := &finderSessionSource{index: newFinderTestIndex(t, root), now: time.Now}

	res, err := src.Search(context.Background(), finderQuery{CWD: "/work/zut"})
	require.NoError(t, err)
	require.Equal(t, []string{newer, older}, finderKeys(res.Items))
	require.Equal(t, "gordon · newer", res.Items[0].Label)
	require.Equal(t, "2 d ago", res.Items[1].Detail)

	res, err = src.Search(context.Background(), finderQuery{Text: "@here", CWD: "/work/zut"})
	require.NoError(t, err)
	require.Equal(t, []string{older}, finderKeys(res.Items))
}

func driveFinderLoop(t *testing.T, i *Interactive, done func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !done() {
		select {
		case fn := <-i.loopCalls:
			fn()
		case <-deadline:
			t.Fatal("finder loop did not settle")
		}
	}
}

func TestFinderSnippetsForSelectedItem(t *testing.T) {
	root := t.TempDir()
	first := writeFinderSession(t, root, "/work/a", "a", "needle alpha", "reply", "needle beta")
	second := writeFinderSession(t, root, "/work/b", "b", "needle gamma")
	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(second, past, past))
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Terminal: &alertTestTerminal{}, CWD: "/work/a", SessionsRoot: root})
	i.finderDelay, i.finderRetryDelay = 0, 0
	t.Cleanup(i.closeFinderIndex)
	t.Cleanup(i.closeFinder)
	ctx := context.Background()

	i.handleKey(ctx, tui.Key{Kind: tui.KeyCtrlF})
	i.handleKey(ctx, tui.Key{Kind: tui.KeyPaste, Paste: "needle"})
	hitTexts := func(idx int) []string {
		if len(i.finder.items) <= idx {
			return nil
		}
		var out []string
		for _, hit := range i.finder.items[idx].Hits {
			out = append(out, hit.Text)
		}
		return out
	}
	driveFinderLoop(t, i, func() bool {
		texts := hitTexts(0)
		return !i.finder.indexing && len(i.finder.items) == 2 && len(texts) == 2 && texts[0] != "" && texts[1] != ""
	})
	require.Equal(t, first, i.finder.items[0].Key)
	require.Equal(t, []string{"needle alpha", "needle beta"}, hitTexts(0))
	require.Empty(t, hitTexts(1)[0], "only the selected item is fetched")

	i.handleKey(ctx, tui.Key{Kind: tui.KeyDown})
	driveFinderLoop(t, i, func() bool { return hitTexts(1)[0] != "" })
	require.Equal(t, []string{"needle gamma"}, hitTexts(1))
	require.Len(t, i.finderSnippets.cache, 3)
	require.Contains(t, stripANSIBytes(strings.Join(i.finder.Render(tui.Dark, 120, 20), "\n")), "needle gamma")

	// Cached snippets apply synchronously after a refresh.
	i.mu.Lock()
	act := i.finder.Refresh()
	i.mu.Unlock()
	i.dispatchFinderSearch(ctx, act, 0)
	driveFinderLoop(t, i, func() bool { return !i.finder.loading })
	require.Equal(t, second, i.finder.items[1].Key)
	require.Equal(t, []string{"needle gamma"}, hitTexts(1))
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
	require.Contains(t, stripANSIBytes(i.finder.Render(tui.Dark, 120, 20)[finderTabsRow]), "indexing")
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
