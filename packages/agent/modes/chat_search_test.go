package modes

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// readerFixture is a port-mode Interactive whose transcript has matches at
// messages 0, 20 and 41 among long filler, so windowing is observable.
func readerFixture(t *testing.T) (*Interactive, *alertTestTerminal) {
	t.Helper()
	var msgs []provider.Message
	for n := range 42 {
		text := fmt.Sprintf("filler line %d about nothing", n)
		switch n {
		case 0:
			text = "please refactor the watcher"
		case 20:
			text = "more refactoring of the watcher loop"
		case 41:
			text = "watcher refactored at last"
		case 30:
			text = "unrefactored watcher stays" // not a word prefix
		}
		role := provider.RoleUser
		if n%2 == 1 {
			role = provider.RoleAssistant
		}
		msgs = append(msgs, jumpTextMsg(role, text))
	}
	i, _, _, _ := newTreePortInteractive(t, t.TempDir(), "/workspace/reader", msgs)
	term := &alertTestTerminal{}
	i.cfg.Theme, i.cfg.Terminal = tui.Dark, term
	i.rend = tui.NewRenderer(term)
	i.rend.Resize(80, 24)
	i.mu.Lock()
	i.cachedChatLocked(80) // populate view.Messages from the port transcript
	i.mu.Unlock()
	return i, term
}

func readerCur(i *Interactive) (int, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.searchReader == nil {
		return -1, false
	}
	return i.searchReader.matches[i.searchReader.cur], true
}

// readerFrame renders the reader the way redraw does.
func readerFrame(i *Interactive) []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	pane := tui.FloatingPaneMaxRect(80, 24)
	return i.searchReader.Render(i.cfg.Theme, pane.ContentWidth(), pane.ContentHeight())
}

func readerVisible(i *Interactive, text string) bool {
	for _, line := range readerFrame(i) {
		if strings.Contains(stripANSIBytes(line), text) {
			return true
		}
	}
	return false
}

func TestSearchReaderOpensOnHitWithHighlights(t *testing.T) {
	i, term := readerFixture(t)
	i.openSearchReader([]string{"Refacto", "watch"}, 20)
	cur, ok := readerCur(i)
	require.True(t, ok)
	require.Equal(t, 20, cur)
	require.True(t, readerVisible(i, "more refactoring"), "target row in view")
	require.False(t, readerVisible(i, "please refactor"), "windowed away from the top")

	frame := readerFrame(i)
	pane := tui.FloatingPaneMaxRect(80, 24)
	require.Len(t, frame, pane.ContentHeight(), "never exceeds the pane")
	require.Contains(t, stripANSIBytes(frame[0]), "refacto watch · 2/3")
	require.Contains(t, stripANSIBytes(frame[len(frame)-1]), "^n/^p match")

	sel := tui.Dark.SelectionStyle()
	lineBG := strings.TrimSuffix(tui.HighlightLine("x", tui.Dark), "x\x1b[0m")
	var tinted []string
	for _, line := range frame {
		if strings.HasPrefix(line, lineBG) {
			tinted = append(tinted, stripANSIBytes(line))
		}
		if strings.Contains(stripANSIBytes(line), "unrefactored") {
			require.NotContains(t, line, sel, "non-matching message not highlighted")
		}
	}
	require.NotEmpty(t, tinted)
	require.Contains(t, strings.Join(tinted, "\n"), "more refactoring")
	require.NotContains(t, strings.Join(tinted, "\n"), "filler", "only the current match is tinted")

	// The composed frame the renderer draws carries the reader styling.
	i.redraw()
	out := term.String()
	require.Contains(t, out, sel+"refacto")
	require.Contains(t, out, lineBG)
	require.Contains(t, stripANSIBytes(out), "refacto watch · 2/3")
}

func TestSearchReaderNextPrevWrapAndRewindow(t *testing.T) {
	i, _ := readerFixture(t)
	i.openSearchReader([]string{"refacto"}, 41)
	readerFrame(i)
	require.True(t, readerVisible(i, "refactored at last"))
	key := func(kind tui.KeyKind) { i.handleKey(context.Background(), tui.Key{Kind: kind}) }

	key(tui.KeyCtrlN)
	cur, _ := readerCur(i)
	require.Equal(t, 0, cur, "next wraps to the first match")
	require.True(t, readerVisible(i, "please refactor"))
	require.Contains(t, stripANSIBytes(readerFrame(i)[0]), "1/3")

	key(tui.KeyCtrlP)
	cur, _ = readerCur(i)
	require.Equal(t, 41, cur, "prev wraps to the last match")
	require.True(t, readerVisible(i, "refactored at last"))
	key(tui.KeyCtrlP)
	cur, _ = readerCur(i)
	require.Equal(t, 20, cur)
	require.True(t, readerVisible(i, "more refactoring"))
}

func TestSearchReaderScrollKeys(t *testing.T) {
	i, _ := readerFixture(t)
	i.openSearchReader([]string{"refacto"}, 20)
	readerFrame(i)
	top := func() int { i.mu.Lock(); defer i.mu.Unlock(); return i.searchReader.top }
	key := func(kind tui.KeyKind) { i.handleKey(context.Background(), tui.Key{Kind: kind}); readerFrame(i) }
	start := top()
	key(tui.KeyDown)
	require.Equal(t, start+1, top())
	key(tui.KeyUp)
	require.Equal(t, start, top())
	page := i.searchReader.page
	key(tui.KeyPageDown)
	require.Equal(t, start+page, top())
	key(tui.KeyPageUp)
	require.Equal(t, start, top())
	key(tui.KeyHome)
	require.Equal(t, 0, top())
	require.True(t, readerVisible(i, "please refactor"))
	key(tui.KeyEnd)
	require.Equal(t, len(i.searchReader.rows)-page, top())
	require.True(t, readerVisible(i, "refactored at last"))
	// Typing is ignored: the reader is modal.
	key(tui.KeyRune)
	require.True(t, i.ed.IsEmpty())
}

func TestSearchReaderEscClosesWithoutArmingOrCancelling(t *testing.T) {
	i, _ := readerFixture(t)
	now := time.Unix(500, 0)
	i.clock = func() time.Time { return now }
	require.True(t, i.canArmSessionTreeEscape(), "fixture must be double-Esc eligible")
	i.openSearchReader([]string{"watcher"}, 0)
	require.True(t, i.sessionTreeEscapeBlocked())
	require.False(t, i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}))
	_, ok := readerCur(i)
	require.False(t, ok)
	require.False(t, i.doubleEscape.armed, "Esc that closes the reader must not arm double-Esc")

	// A running turn: the mock has no Abort expectation, so a cancel fails.
	i.openSearchReader([]string{"watcher"}, 0)
	i.mu.Lock()
	i.portInfo.Run = session.RunStateRunning
	i.mu.Unlock()
	require.False(t, i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}))
	_, ok = readerCur(i)
	require.False(t, ok)
}

func TestSearchReaderClosedOnSessionReplace(t *testing.T) {
	i, _ := readerFixture(t)
	i.openSearchReader([]string{"watcher"}, 0)
	i.mu.Lock()
	i.hydrateLocked(session.Snapshot{Messages: []provider.Message{jumpTextMsg(provider.RoleUser, "x")}})
	i.mu.Unlock()
	_, ok := readerCur(i)
	require.False(t, ok)
}

func TestSearchReaderNoMatch(t *testing.T) {
	i, _ := readerFixture(t)
	i.openSearchReader([]string{"zzzmissing"}, 0)
	_, ok := readerCur(i)
	require.False(t, ok)
	require.Contains(t, i.statusErr, "no match")
}

func TestSearchReaderNarrowPaneFitsHeight(t *testing.T) {
	i, _ := readerFixture(t)
	i.openSearchReader([]string{"refacto"}, 20)
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, h := range []int{1, 2, 3, 5} {
		lines := i.searchReader.Render(tui.Dark, 12, h)
		require.LessOrEqual(t, len(lines), h)
		for _, line := range lines {
			require.LessOrEqual(t, len([]rune(stripANSIBytes(line))), 12)
		}
	}
}
