package modes

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/agent/internal/sessionindex"
	"github.com/bnema/zut/packages/tui"
)

func TestFinderCardForItemWithoutHits(t *testing.T) {
	root := t.TempDir()
	writeFinderSession(t, root, "/work/a", "a", "please refactor the parser", "done, parser refactored")
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Terminal: &alertTestTerminal{}, CWD: "/work/a", SessionsRoot: root})
	i.finderDelay, i.finderRetryDelay = 0, 0
	t.Cleanup(i.closeFinderIndex)
	t.Cleanup(i.closeFinder)

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlF})
	driveFinderLoop(t, i, func() bool {
		item, ok := i.finder.selected()
		return !i.finder.indexing && !i.finder.loading && ok && i.finder.hasCard(item.Key)
	})
	joined := stripANSIBytes(strings.Join(i.finder.Render(tui.Dark, 140, 24), "\n"))
	require.Contains(t, joined, "2 messages · you 1 · agent 1")
	require.Contains(t, joined, "p/m")
	require.Contains(t, joined, "first request · ")
	require.Contains(t, joined, "please refactor the parser")
	require.Contains(t, joined, "last reply · ")
	require.Contains(t, joined, "done, parser refactored")
}

func TestRenderFinderCardBudget(t *testing.T) {
	when := time.Date(2026, 11, 20, 14, 2, 11, 0, time.Local)
	card := sessionCard(sessionindex.Overview{
		Doc:      sessionindex.SessionDoc{CWD: "/work/a", Started: when, Updated: when.Add(time.Hour), Provider: "p", Model: "m", Cost: 1.234},
		UserMsgs: 3, AgentMsgs: 4,
		First: &sessionindex.Snippet{Ref: sessionindex.HitRef{Time: when}, Text: strings.Repeat("ask ", 200)},
		Last:  &sessionindex.Snippet{Ref: sessionindex.HitRef{Time: when.Add(time.Hour)}, Text: "short reply"},
	})
	require.Equal(t, []string{"/work/a", "started 2026-11-20 14:02 · last 2026-11-20 15:02", "7 messages · you 3 · agent 4", "p/m · $1.23"}, card.Facts)
	item := finderItem{Key: "k", Label: "zut · title"}
	lines := renderFinderCard(tui.Dark, item, card, 40, 16)
	require.Len(t, lines, 16)
	plain := make([]string, len(lines))
	for n, line := range lines {
		plain[n] = stripANSIBytes(line)
		require.Equal(t, 40, runewidth.StringWidth(plain[n]), plain[n])
	}
	joined := strings.Join(plain, "\n")
	require.Contains(t, joined, "first request · 2026-11-20 14:02:11")
	require.Contains(t, joined, "…", "the long request is cut to its share")
	require.Contains(t, joined, "last reply · 2026-11-20 15:02:11")
	require.Contains(t, joined, "short reply")

	// A card fetch for a session missing from the index renders the detail.
	m := newFinderModel("/work", newTestFinderSource(t, "Sessions"))
	act := m.Open(0)
	require.True(t, m.ApplyResults(act.Gen, act.TabID, finderResults{Items: []finderItem{{Key: "k", Label: "zut · x", Detail: "3d ago"}}}, nil))
	require.True(t, m.SetCard("k", finderCard{}))
	require.Contains(t, stripANSIBytes(strings.Join(m.Render(tui.Dark, 140, 12), "\n")), "3d ago")
}
