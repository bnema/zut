package modes

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/tui"
)

func finderQueryText(text string) any {
	return mock.MatchedBy(func(q finderQuery) bool { return q.Text == text })
}

func newFinderInteractive(t *testing.T, src *mockFinderSource) *Interactive {
	t.Helper()
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Terminal: &alertTestTerminal{}, CWD: "/work"})
	i.finderDelay = 0
	i.finderSources = func() []finderSource { return []finderSource{src} }
	t.Cleanup(i.closeFinder)
	return i
}

// runFinderLoopCall runs the next Run-loop call posted by a finder search.
func runFinderLoopCall(t *testing.T, i *Interactive) {
	t.Helper()
	select {
	case fn := <-i.loopCalls:
		fn()
	case <-time.After(5 * time.Second):
		t.Fatal("finder search result was not delivered")
	}
}

func TestFinderCtrlFOpensAndSearches(t *testing.T) {
	src := newTestFinderSource(t, "sessions")
	src.EXPECT().Search(mock.Anything, finderQueryText("")).Return(finderResults{Items: []finderItem{{Key: "a", Label: "first"}}}, nil).Once()
	i := newFinderInteractive(t, src)

	require.False(t, i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlF}))
	require.True(t, i.finder.Active())
	runFinderLoopCall(t, i)
	require.Equal(t, "a", i.finder.items[0].Key)
}

func TestFinderSlashOpens(t *testing.T) {
	src := newTestFinderSource(t, "sessions")
	src.EXPECT().Search(mock.Anything, mock.Anything).Return(finderResults{}, nil).Once()
	i := newFinderInteractive(t, src)

	i.runSlash(context.Background(), "/find")
	require.True(t, i.finder.Active())
	runFinderLoopCall(t, i)
	require.Contains(t, slashCatalog, slashCommand{Name: "/find", Desc: "search sessions and messages across all projects"})
}

func TestFinderOpenRejectedWithoutSessionsOrSources(t *testing.T) {
	i := NewInteractive(InteractiveConfig{SessionsDisabled: true})
	i.runSlash(context.Background(), "/find")
	require.False(t, i.finder.Active())
	require.Equal(t, "sessions are disabled by --no-session", i.statusErr)

	i = NewInteractive(InteractiveConfig{})
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlF})
	require.False(t, i.finder.Active())
	require.Equal(t, "finder: sessionindex: empty root", i.statusErr)

	i = NewInteractive(InteractiveConfig{})
	i.finderSources = func() []finderSource { return nil }
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlF})
	require.Equal(t, "finder unavailable", i.statusErr)
}

func TestFinderTypingDropsSupersededSearch(t *testing.T) {
	src := newTestFinderSource(t, "sessions")
	src.EXPECT().Search(mock.Anything, finderQueryText("")).Return(finderResults{}, nil).Once()
	stale := make(chan context.Context, 1)
	src.EXPECT().Search(mock.Anything, finderQueryText("a")).RunAndReturn(func(ctx context.Context, _ finderQuery) (finderResults, error) {
		stale <- ctx
		<-ctx.Done()
		return finderResults{Items: []finderItem{{Key: "stale"}}}, nil
	}).Once()
	src.EXPECT().Search(mock.Anything, finderQueryText("ab")).Return(finderResults{Items: []finderItem{{Key: "fresh"}}}, nil).Once()
	i := newFinderInteractive(t, src)
	ctx := context.Background()

	i.handleKey(ctx, tui.Key{Kind: tui.KeyCtrlF})
	runFinderLoopCall(t, i)
	i.handleKey(ctx, tui.Key{Kind: tui.KeyRune, Rune: 'a'})
	staleCtx := <-stale
	i.handleKey(ctx, tui.Key{Kind: tui.KeyRune, Rune: 'b'})
	<-staleCtx.Done()
	runFinderLoopCall(t, i)
	require.Len(t, i.finder.items, 1)
	require.Equal(t, "fresh", i.finder.items[0].Key)
	select {
	case fn := <-i.loopCalls:
		t.Fatalf("superseded search posted a result: %p", fn)
	default:
	}
}

func TestFinderEscClosesAndCancelsSearch(t *testing.T) {
	src := newTestFinderSource(t, "sessions")
	started := make(chan context.Context, 1)
	src.EXPECT().Search(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, _ finderQuery) (finderResults, error) {
		started <- ctx
		<-ctx.Done()
		return finderResults{}, ctx.Err()
	}).Once()
	i := newFinderInteractive(t, src)

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlF})
	searchCtx := <-started
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc})
	require.False(t, i.finder.Active())
	<-searchCtx.Done()
	require.ErrorIs(t, searchCtx.Err(), context.Canceled)
}

func TestFinderBlocksDoubleEscapeAndGatesInput(t *testing.T) {
	src := newTestFinderSource(t, "sessions")
	src.EXPECT().Search(mock.Anything, mock.Anything).Return(finderResults{}, nil).Once()
	i := newFinderInteractive(t, src)
	require.False(t, i.confirmChildActive())

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlF})
	runFinderLoopCall(t, i)
	require.True(t, i.sessionTreeEscapeBlocked())
	require.True(t, i.confirmChildActive())
}

func TestFinderRendersInFrameWithVisibleCaret(t *testing.T) {
	src := newTestFinderSource(t, "sessions")
	src.EXPECT().Search(mock.Anything, mock.Anything).Return(finderResults{}, nil).Maybe()
	i := newFinderInteractive(t, src)
	term := i.cfg.Terminal.(*alertTestTerminal)
	i.rend.Resize(80, 24)

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlF})
	long := strings.Repeat("abcdefghij", 12) + "END"
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyPaste, Paste: long})
	for len(i.loopCalls) > 0 {
		runFinderLoopCall(t, i)
	}
	i.redraw()
	require.Contains(t, stripANSIBytes(term.String()), "Find")
	require.Equal(t, 1, strings.Count(stripANSIBytes(term.String()), "╭"), "the pane frame is the only box")

	lines := i.finder.Render(tui.Dark, 80, 20)
	query := stripANSIBytes(lines[finderQueryRow])
	require.Contains(t, query, "END")
	_, col := i.finder.CursorPos()
	require.Less(t, col, 80)
	require.Equal(t, 3+runewidth.StringWidth(string(i.finder.query.text[i.finder.query.scroll:])), col)

	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyHome})
	lines = i.finder.Render(tui.Dark, 80, 20)
	_, col = i.finder.CursorPos()
	require.Equal(t, 3, col)
	require.Contains(t, stripANSIBytes(lines[finderQueryRow]), "> abcdefghij")
}
