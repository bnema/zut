package modes

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/agent/internal/sessionindex"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
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

// openReader opens the reader and applies its background build.
func openReader(t *testing.T, i *Interactive, terms []string, focus int) {
	t.Helper()
	i.openSearchReader(searchReaderTarget{terms: terms, focus: focus})
	runLoopCall(t, i)
}

func readerCur(i *Interactive) (int, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.searchReader == nil || !i.searchReader.ready {
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
	openReader(t, i, []string{"Refacto", "watch"}, 20)
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
	}
	require.NotEmpty(t, tinted)
	require.NotContains(t, strings.Join(frame, "\n"), "unrefactored", "message 30 is windowed out here")
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
	openReader(t, i, []string{"refacto"}, 41)
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
	openReader(t, i, []string{"refacto"}, 20)
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
	openReader(t, i, []string{"watcher"}, 0)
	require.True(t, i.sessionTreeEscapeBlocked())
	require.False(t, i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}))
	_, ok := readerCur(i)
	require.False(t, ok)
	require.False(t, i.doubleEscape.armed, "Esc that closes the reader must not arm double-Esc")

	// A running turn: the mock has no Abort expectation, so a cancel fails.
	openReader(t, i, []string{"watcher"}, 0)
	i.mu.Lock()
	i.portInfo.Run = session.RunStateRunning
	i.mu.Unlock()
	require.False(t, i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc}))
	_, ok = readerCur(i)
	require.False(t, ok)
}

func TestSearchReaderClosedOnSessionReplace(t *testing.T) {
	i, _ := readerFixture(t)
	openReader(t, i, []string{"watcher"}, 0)
	i.mu.Lock()
	i.hydrateLocked(session.Snapshot{Messages: []provider.Message{jumpTextMsg(provider.RoleUser, "x")}})
	i.mu.Unlock()
	_, ok := readerCur(i)
	require.False(t, ok)
}

func TestSearchReaderNoMatch(t *testing.T) {
	i, _ := readerFixture(t)
	openReader(t, i, []string{"zzzmissing"}, 0)
	_, ok := readerCur(i)
	require.False(t, ok)
	require.Contains(t, i.statusErr, "no match")
}

func TestSearchReaderNarrowPaneFitsHeight(t *testing.T) {
	i, _ := readerFixture(t)
	i.mu.Lock()
	i.view.Messages = append(i.view.Messages, jumpTextMsg(provider.RoleUser, "refactor 日本語のテキスト 🎉🎉 wide"))
	i.mu.Unlock()
	openReader(t, i, []string{"refacto"}, 42)
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, h := range []int{1, 2, 3, 5, 9} {
		lines := i.searchReader.Render(tui.Dark, 12, h)
		require.Len(t, lines, h)
		for _, line := range lines {
			require.LessOrEqual(t, runewidth.StringWidth(stripANSIBytes(line)), 12, "%q", stripANSIBytes(line))
		}
	}
}

func TestSearchReaderHighlightsOnlyWordPrefixes(t *testing.T) {
	i, _ := readerFixture(t)
	i.mu.Lock()
	i.view.Messages = append(i.view.Messages, jumpTextMsg(provider.RoleUser, "refactor it but keep unrefactored parts"))
	i.mu.Unlock()
	openReader(t, i, []string{"refacto"}, 42)
	sel := tui.Dark.SelectionStyle()
	found := false
	for _, line := range readerFrame(i) {
		plain := stripANSIBytes(line)
		if !strings.Contains(plain, "unrefactored") {
			continue
		}
		found = true
		require.Contains(t, line, sel+"refacto", "the word-start match is highlighted")
		require.Equal(t, 1, strings.Count(line, sel), "the substring inside unrefactored is not")
	}
	require.True(t, found)
}

func TestSearchReaderRebuildsOnWidthChange(t *testing.T) {
	i, _ := readerFixture(t)
	openReader(t, i, []string{"refacto"}, 20)
	i.mu.Lock()
	before := i.searchReader.width
	i.searchReader.top = 0 // scrolled away
	lines := i.renderSearchReaderLocked(tui.Dark, 40, 20)
	building := i.searchReader.building
	i.mu.Unlock()
	require.Len(t, lines, 20, "old rows show while rebuilding")
	require.True(t, building)
	runLoopCall(t, i)
	i.mu.Lock()
	require.Equal(t, 40, i.searchReader.width)
	require.NotEqual(t, before, i.searchReader.width)
	lines = i.renderSearchReaderLocked(tui.Dark, 40, 20)
	i.mu.Unlock()
	joined := stripANSIBytes(strings.Join(lines, "\n"))
	require.Contains(t, joined, "more", "recentered on the current match")
}

func TestSearchReaderShowsLoadingUntilBuilt(t *testing.T) {
	i, _ := readerFixture(t)
	i.openSearchReader(searchReaderTarget{terms: []string{"refacto"}, focus: 20})
	frame := readerFrame(i)
	require.Contains(t, stripANSIBytes(strings.Join(frame, "\n")), "searching…")
	runLoopCall(t, i)
	require.True(t, readerVisible(i, "more refactoring"))
}

func TestSearchReaderSkipsCompactionSummaryMatches(t *testing.T) {
	i, _ := readerFixture(t)
	summary := jumpTextMsg(provider.RoleUser, "summary: refactoring the watcher")
	summary.Meta = map[string]string{"compaction": "true"}
	i.mu.Lock()
	i.view.Messages = append([]provider.Message{summary}, i.view.Messages...)
	i.mu.Unlock()
	openReader(t, i, []string{"refacto"}, 0)
	i.mu.Lock()
	r := i.searchReader
	rows := anchorRows(r.anchors, len(r.rows), 0)
	matches := append([]int(nil), r.matches...)
	i.mu.Unlock()
	require.Positive(t, rows, "fixture: the summary renders as its separator")
	require.NotContains(t, matches, 0, "a collapsed summary is not a match")
	require.Len(t, matches, 3)
}

func TestSearchReaderStripsInlineImages(t *testing.T) {
	i, _ := readerFixture(t)
	png := []byte("\x89PNG\r\n\x1a\n")
	i.mu.Lock()
	i.view.ImageProto = tui.ImageProtocolKitty
	i.view.Messages = append(i.view.Messages, provider.Message{Role: provider.RoleUser, Content: []provider.Content{
		provider.TextBlock{Text: "refactor this screenshot"},
		provider.ImageBlock{MimeType: "image/png", Data: png},
	}})
	i.mu.Unlock()
	openReader(t, i, []string{"refacto"}, 42)
	i.mu.Lock()
	require.Equal(t, tui.ImageProtocolNone, i.searchReader.view.ImageProto)
	rows := append([]string(nil), i.searchReader.rows...)
	i.mu.Unlock()
	for _, row := range append(rows, readerFrame(i)...) {
		require.NotContains(t, row, "\x1b_G")
		require.NotContains(t, row, "\x1b]1337")
	}
}

func TestSearchReaderCompactedHitWithoutOtherMatch(t *testing.T) {
	i, _ := readerFixture(t)
	hit := &sessionindex.HitRef{Fingerprint: 12345, MsgSeq: 1, TotalMsgs: 100}
	i.openSearchReader(searchReaderTarget{terms: []string{"zzznomatch"}, hit: hit, note: "forked into ~/x (new session)"})
	runLoopCall(t, i)
	_, ok := readerCur(i)
	require.False(t, ok)
	require.Empty(t, i.statusErr)
	require.Contains(t, i.statusOK, "forked into ~/x")
	require.Contains(t, i.statusOK, "match is no longer in the session history")
	require.Contains(t, i.statusOK, "no match in this session")
}

func TestSearchReaderConfirmationTakesOverKeysAndScreen(t *testing.T) {
	i, term := readerFixture(t)
	openReader(t, i, []string{"refacto"}, 20)
	resp := make(chan core.ConfirmDecision, 1)
	i.confirmDialog.Enqueue(&confirmRequest{toolName: "bash", preview: "rm -rf build", resp: resp})
	require.True(t, i.confirmDialog.Focused())

	i.redraw()
	out := stripANSIBytes(term.String())
	require.Contains(t, out, "rm -rf build", "the confirmation that owns the keys is what is shown")

	// "/" does not blur the shown confirmation behind the reader, and
	// nothing reaches the hidden editor.
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyRune, Rune: '/'})
	require.True(t, i.confirmDialog.Focused())
	require.True(t, i.ed.IsEmpty())
	i.redraw()
	require.Contains(t, stripANSIBytes(term.String()), "rm -rf build")

	// Esc answers the visible confirmation, not the hidden reader.
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEsc})
	select {
	case d := <-resp:
		require.False(t, d.Allow)
	default:
		t.Fatal("esc did not reach the shown confirmation")
	}
	_, ok := readerCur(i)
	require.True(t, ok, "the reader is still open behind it")
}

func TestSearchReaderClosedBySessionChanges(t *testing.T) {
	for _, kind := range []sessionResultKind{sessionResultSwitch, sessionResultSuccessor} {
		t.Run(fmt.Sprintf("session result %d", kind), func(t *testing.T) {
			i, _ := readerFixture(t)
			openReader(t, i, []string{"watcher"}, 0)
			next := sessionmocks.NewMockPort(t)
			res := sessionResult{kind: kind, gen: i.sessionGen, id: "next", port: next, events: make(chan session.Event), cancel: func() {},
				snap: session.Snapshot{Info: session.Info{ID: "next"}, Messages: []provider.Message{jumpTextMsg(provider.RoleUser, "x")}}}
			i.applySessionResult(context.Background(), res)
			_, ok := readerCur(i)
			require.False(t, ok)
		})
	}
	t.Run("session result error keeps it", func(t *testing.T) {
		i, _ := readerFixture(t)
		openReader(t, i, []string{"watcher"}, 0)
		i.applySessionResult(context.Background(), sessionResult{kind: sessionResultSwitch, gen: i.sessionGen, err: context.Canceled})
		_, ok := readerCur(i)
		require.True(t, ok, "a failed switch leaves the transcript, and the reader, in place")
	})
	t.Run("SwitchSession", func(t *testing.T) {
		i, _ := readerFixture(t)
		openReader(t, i, []string{"watcher"}, 0)
		require.NoError(t, i.SwitchSession(context.Background(), "other", targetPort(t, "other")))
		_, ok := readerCur(i)
		require.False(t, ok)
	})

	local := func(t *testing.T) (*finderExecLocal, string) {
		t.Helper()
		cwd := t.TempDir()
		l := newFinderExecLocal(t, cwd)
		path := newTreeTestSession(t, l.root, cwd, finderExecMessages())
		l.i.applySessionSelection(path)
		<-l.loaded
		require.Eventually(t, func() bool { return !l.i.sessionLoadingNow() }, 5*time.Second, time.Millisecond)
		openReader(t, l.i, []string{"refacto"}, 3)
		_, ok := readerCur(l.i)
		require.True(t, ok)
		return l, path
	}
	t.Run("local load", func(t *testing.T) {
		l, path := local(t)
		l.i.finishSessionSelection(path, nil)
		_, ok := readerCur(l.i)
		require.False(t, ok)
	})
	t.Run("import", func(t *testing.T) {
		l, path := local(t)
		l.i.doSessionImport(path)
		<-l.loaded
		_, ok := readerCur(l.i)
		require.False(t, ok, l.i.statusErr)
	})
	t.Run("tree", func(t *testing.T) {
		l, path := local(t)
		target := sessionTreeTarget{SourcePath: path, EffectiveIndex: 1, SelectionBoundary: 2, Role: provider.RoleAssistant, Boundary: sessionTreeMessageBoundary}
		l.i.applySessionTreeTarget(target, 1)
		require.Empty(t, l.i.statusErr)
		<-l.loaded
		_, ok := readerCur(l.i)
		require.False(t, ok)
	})
}

func (i *Interactive) sessionLoadingNow() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.sessionLoading
}

// drainReader applies a pending reader build, if one is in flight.
func drainReader(t *testing.T, i *Interactive) {
	t.Helper()
	i.mu.Lock()
	pending := i.searchReader != nil && i.searchReader.building
	i.mu.Unlock()
	if pending {
		runLoopCall(t, i)
	}
}
