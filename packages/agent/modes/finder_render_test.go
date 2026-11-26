package modes

import (
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func renderFinderFixture(t *testing.T) *finderModel {
	t.Helper()
	m := newFinderModel("/work", newTestFinderSource(t, "Sessions"), newTestFinderSource(t, "Messages"))
	act := m.Open(0)
	finderRunes(m, "refacto")
	act = m.search()
	now := time.Now()
	items := []finderItem{
		{Key: "a", Label: "zut · advisor \x1b[31mwatcher", Detail: "3 days ago", Count: 12, Live: true, Hits: []finderHit{
			{Role: provider.RoleUser, Time: now.Add(-72 * time.Hour), Text: strings.Repeat("padding words ", 20) + "let's do a refactoring of the watcher " + strings.Repeat("tail ", 30)},
			{Role: provider.RoleAssistant, Time: now.Add(-71 * time.Hour), Compacted: true},
		}},
		{Key: "b", Label: "界面 · 日本語のセッション名前がとても長い場合のテスト", Detail: "gordon", Count: 8},
		{Key: "c", Label: "plain", Count: 0},
	}
	require.True(t, m.ApplyResults(act.Gen, act.TabID, finderResults{Items: items, Partial: true}, nil))
	return m
}

func requireExactWidth(t *testing.T, lines []string, width int) {
	t.Helper()
	require.NotEmpty(t, lines)
	for i, line := range lines {
		require.Equal(t, width, runewidth.StringWidth(stripANSIBytes(line)), "line %d: %q", i, stripANSIBytes(line))
	}
}

func TestFinderRenderWidthInvariants(t *testing.T) {
	for _, width := range []int{40, 60, 100, 160} {
		for _, height := range []int{8, 24} {
			m := renderFinderFixture(t)
			lines := m.Render(tui.Dark, width, height)
			requireExactWidth(t, lines, width)
			require.Equal(t, height, len(lines), "width %d", width)
			m.HandleKey(tui.Key{Kind: tui.KeyDown})
			m.HandleKey(tui.Key{Kind: tui.KeyCtrlD})
			requireExactWidth(t, m.Render(tui.Dark, width, height), width)
			m.HandleKey(tui.Key{Kind: tui.KeyEsc})
			m.err = "boom \x1b[2J"
			requireExactWidth(t, m.Render(tui.Dark, width, height), width)
		}
	}
}

func TestFinderRenderHeaderAndList(t *testing.T) {
	m := renderFinderFixture(t)
	lines := m.Render(tui.Dark, 160, 20)
	top := stripANSIBytes(lines[0])
	require.Contains(t, top, "Find")
	require.Contains(t, top, "[Sessions] Messages")
	query := stripANSIBytes(lines[1])
	require.Contains(t, query, "> refacto")
	require.Contains(t, query, "20 hits · 3 sessions · partial")
	body := stripANSIBytes(lines[3])
	require.Contains(t, body, "▸ zut · advisor")
	require.Contains(t, body, "12 ●")
	require.NotContains(t, strings.Join(lines, "\n"), "\x1b[31mwatcher")
	require.Contains(t, stripANSIBytes(lines[len(lines)-2]), "tab source")

	m.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: 'x'})
	require.Contains(t, stripANSIBytes(m.Render(tui.Dark, 160, 20)[1]), "searching")
}

func TestFinderRenderPreviewHighlights(t *testing.T) {
	m := renderFinderFixture(t)
	lines := m.Render(tui.Dark, 160, 20)
	joined := stripANSIBytes(strings.Join(lines, "\n"))
	require.Contains(t, joined, "you · 3 d ago")
	require.Contains(t, joined, "agent · 2 d ago compacted")
	require.Contains(t, joined, "refactoring")
	snippet := ""
	for _, line := range lines {
		if strings.Contains(stripANSIBytes(line), "refactoring") {
			snippet = line
		}
	}
	require.Contains(t, snippet, tui.Dark.SelectionStyle()+"refacto")
	_, matched := tui.HighlightTerms(snippet, []string{"zzz"}, tui.Dark)
	require.False(t, matched)
	require.Contains(t, joined, " …") // empty snippet placeholder
}

func TestFinderRenderLayouts(t *testing.T) {
	m := renderFinderFixture(t)
	wide := m.Render(tui.Dark, 120, 20)
	require.Contains(t, stripANSIBytes(wide[2]), "┬")
	require.Contains(t, stripANSIBytes(wide[3]), "you ·")

	stacked := m.Render(tui.Dark, 80, 20)
	require.NotContains(t, stripANSIBytes(strings.Join(stacked, "")), "┬")
	listRow, previewRow := -1, -1
	for i, line := range stacked {
		plain := stripANSIBytes(line)
		if strings.Contains(plain, "▸ zut") {
			listRow = i
		}
		if strings.Contains(plain, "you ·") {
			previewRow = i
		}
	}
	require.Greater(t, previewRow, listRow)
	require.GreaterOrEqual(t, listRow, 0)

	narrow := m.Render(tui.Dark, 40, 20)
	joined := stripANSIBytes(strings.Join(narrow, "\n"))
	require.NotContains(t, joined, "you ·")
	require.Contains(t, joined, "refacto")
}

func TestFinderRenderDeleteConfirmFooter(t *testing.T) {
	m := renderFinderFixture(t)
	m.HandleKey(tui.Key{Kind: tui.KeyCtrlD})
	lines := m.Render(tui.Dark, 100, 20)
	footer := stripANSIBytes(lines[len(lines)-2])
	require.Contains(t, footer, "delete zut · advisor")
	require.Contains(t, footer, "y confirm · any key cancel")
	require.NotContains(t, footer, "tab source")
}

func TestFinderRenderScrollsToCursor(t *testing.T) {
	m := newFinderModel("/work", newTestFinderSource(t, "Sessions"))
	act := m.Open(0)
	var items []finderItem
	for i := range 30 {
		items = append(items, finderItem{Key: string(rune('A' + i)), Label: "item " + string(rune('A'+i))})
	}
	m.ApplyResults(act.Gen, act.TabID, finderResults{Items: items}, nil)
	for range 25 {
		m.HandleKey(tui.Key{Kind: tui.KeyDown})
	}
	joined := stripANSIBytes(strings.Join(m.Render(tui.Dark, 120, 12), "\n"))
	require.Contains(t, joined, "▸ item Z")
	require.NotContains(t, joined, "item A")
}

func TestFinderApplySnippets(t *testing.T) {
	m := renderFinderFixture(t)
	require.False(t, m.ApplySnippets(m.Generation()-1, "a", map[int]string{1: "x"}))
	require.False(t, m.ApplySnippets(m.Generation(), "missing", map[int]string{0: "x"}))
	require.True(t, m.ApplySnippets(m.Generation(), "a", map[int]string{1: "agent text", 9: "ignored"}))
	require.Equal(t, "agent text", m.items[0].Hits[1].Text)
}

func TestFinderCursorPos(t *testing.T) {
	m := newFinderModel("/work", newTestFinderSource(t, "Sessions"))
	row, col := m.CursorPos()
	require.Equal(t, -1, row)
	require.Equal(t, -1, col)
	m.Open(0)
	finderRunes(m, "界a")
	m.HandleKey(tui.Key{Kind: tui.KeyLeft})
	row, col = m.CursorPos()
	require.Equal(t, 1, row)
	require.Equal(t, 6, col)
	line := stripANSIBytes(m.Render(tui.Dark, 60, 10)[1])
	require.Equal(t, "│ > 界", runewidth.Truncate(line, 6, ""))
}
