package modes

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func chatSearchFixture(t *testing.T) (*Interactive, *sessionmocks.MockPort) {
	t.Helper()
	msgs := []provider.Message{
		jumpTextMsg(provider.RoleUser, "please refactor the watcher"),          // 0 match
		jumpTextMsg(provider.RoleAssistant, "done with unrelated work"),        // 1
		jumpTextMsg(provider.RoleUser, "more refactoring of the watcher loop"), // 2 match
		jumpTextMsg(provider.RoleAssistant, "watcher refactored"),              // 3 match
		jumpTextMsg(provider.RoleUser, "unrefactored watcher stays"),           // 4 no (not a word prefix)
	}
	i, port, _, _ := newTreePortInteractive(t, t.TempDir(), "/workspace/chat-search", msgs)
	i.cfg.Theme = tui.Dark
	i.cfg.Terminal = &alertTestTerminal{}
	i.mu.Lock()
	i.cachedChatLocked(80) // populate view.Messages from the port transcript
	i.mu.Unlock()
	return i, port
}

func chatSearchCur(i *Interactive) (int, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.chatSearch.active {
		return -1, false
	}
	return i.chatSearch.matches[i.chatSearch.cur], true
}

func TestChatSearchStartHighlightsMatchedRows(t *testing.T) {
	i, _ := chatSearchFixture(t)
	i.startChatSearch([]string{"Refacto", "watch"}, 2)
	cur, ok := chatSearchCur(i)
	require.True(t, ok)
	require.Equal(t, 2, cur)
	i.mu.Lock()
	require.Equal(t, []int{0, 2, 3}, i.chatSearch.matches)
	require.Contains(t, i.statusOK, `search "refacto watch" 2/3`)
	chat := i.cachedChatLocked(80)
	out := i.applyChatSearchLocked(chat, 0, i.chatRowBase, i.stableChatLen)
	cachedCopy := i.cachedChatLocked(80)
	i.mu.Unlock()

	require.Equal(t, cachedCopy, chat, "cached rows are not mutated")
	sel := tui.Dark.SelectionStyle()
	lineBG := strings.TrimSuffix(tui.HighlightLine("x", tui.Dark), "x\x1b[0m")
	require.NotEmpty(t, lineBG)

	var tinted, highlighted []string
	for _, line := range out {
		if strings.Contains(line, sel) {
			highlighted = append(highlighted, stripANSIBytes(line))
		}
		if strings.HasPrefix(line, lineBG) {
			tinted = append(tinted, stripANSIBytes(line))
		}
	}
	require.NotEmpty(t, tinted)
	tintedText := strings.Join(tinted, "\n")
	require.Contains(t, tintedText, "more refactoring")
	require.NotContains(t, tintedText, "please refactor", "only the current message is tinted")
	require.NotContains(t, tintedText, "unrelated work")
	joined := strings.Join(highlighted, "\n")
	require.Contains(t, joined, "please refactor")
	require.Contains(t, joined, "watcher refactored")
	require.NotContains(t, joined, "unrefactored", "non-matching messages are not highlighted")
	require.Contains(t, strings.Join(out, ""), sel+"refacto", "selection SGR wraps the term")
}

func TestChatSearchNextPrevWrap(t *testing.T) {
	i, _ := chatSearchFixture(t)
	i.startChatSearch([]string{"refacto"}, 3)
	cur, _ := chatSearchCur(i)
	require.Equal(t, 3, cur)
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlN})
	cur, _ = chatSearchCur(i)
	require.Equal(t, 0, cur, "next wraps to the first match")
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlP})
	cur, _ = chatSearchCur(i)
	require.Equal(t, 3, cur, "prev wraps to the last match")
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlP})
	cur, _ = chatSearchCur(i)
	require.Equal(t, 2, cur)
	require.Contains(t, i.statusOK, "2/3")
}

func TestChatSearchEscClearsWithoutArmingOrCancelling(t *testing.T) {
	i, _ := chatSearchFixture(t)
	now := time.Unix(500, 0)
	i.clock = func() time.Time { return now }
	require.True(t, i.canArmSessionTreeEscape(), "fixture must be double-Esc eligible")
	i.startChatSearch([]string{"watcher"}, 0)
	require.False(t, i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}))
	_, ok := chatSearchCur(i)
	require.False(t, ok)
	require.False(t, i.doubleEscape.armed, "Esc that clears search must not arm double-Esc")
	require.NotContains(t, i.statusOK, "search")

	// A running turn: the mock has no Abort expectation, so a cancel fails.
	i.startChatSearch([]string{"watcher"}, 0)
	i.mu.Lock()
	i.portInfo.Run = session.RunStateRunning
	i.mu.Unlock()
	require.False(t, i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}))
	_, ok = chatSearchCur(i)
	require.False(t, ok)
}

func TestChatSearchSubmitClears(t *testing.T) {
	i, port := chatSearchFixture(t)
	submitted := make(chan struct{})
	port.EXPECT().Submit(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, session.Input) error {
		close(submitted)
		return nil
	}).Once()
	i.startChatSearch([]string{"watcher"}, 0)
	i.Submit("next question")
	waitSignal(t, submitted, "Submit")
	_, ok := chatSearchCur(i)
	require.False(t, ok)
}

func TestChatSearchNoMatch(t *testing.T) {
	i, _ := chatSearchFixture(t)
	i.startChatSearch([]string{"zzzmissing"}, 0)
	_, ok := chatSearchCur(i)
	require.False(t, ok)
	require.Contains(t, i.statusErr, "no match")
	// Keys fall through to normal handling when inactive.
	require.False(t, i.handleChatSearchKey(tui.Key{Kind: tui.KeyCtrlN}))
}
