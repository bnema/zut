package modes

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func archiveSummaryMsg(tokens string) provider.Message {
	m := jumpTextMsg(provider.RoleUser, "## Context Summary (compacted)\n\nsummary text")
	m.Meta = map[string]string{"compaction": "true", "tokens_before": tokens}
	return m
}

// writeArchiveSession writes archived messages, a compaction keeping tail,
// and returns the path plus the live (post-compaction) messages.
func writeArchiveSession(t *testing.T, archived []string, tail ...string) (string, []provider.Message) {
	t.Helper()
	sess, err := core.NewSession(t.TempDir(), "/work", "test", "model", "test")
	require.NoError(t, err)
	role := func(n int) provider.Role {
		if n%2 == 0 {
			return provider.RoleUser
		}
		return provider.RoleAssistant
	}
	for n, text := range archived {
		require.NoError(t, sess.AppendMessage(jumpTextMsg(role(n), text)))
	}
	live := []provider.Message{archiveSummaryMsg("182000")}
	for n, text := range tail {
		m := jumpTextMsg(role(n), text)
		require.NoError(t, sess.AppendMessage(m))
		live = append(live, m)
	}
	require.NoError(t, sess.AppendCompaction(live))
	require.NoError(t, sess.Close())
	return sess.Path, live
}

func newArchiveInteractive(t *testing.T) *Interactive {
	t.Helper()
	i := NewInteractive(InteractiveConfig{Theme: tui.Dark, Terminal: &alertTestTerminal{}, Port: sessionmocks.NewMockPort(t)})
	t.Cleanup(i.cancelChatArchive)
	return i
}

func hydrateArchive(i *Interactive, path string, msgs []provider.Message) {
	i.mu.Lock()
	i.hydrateLocked(session.Snapshot{Info: session.Info{ID: "s", SessionPath: path}, Seq: 1, Messages: msgs})
	i.mu.Unlock()
}

func waitArchive(t *testing.T, i *Interactive, rev uint64) {
	t.Helper()
	driveFinderLoop(t, i, func() bool {
		i.mu.Lock()
		defer i.mu.Unlock()
		return i.chatArchive.rev > rev
	})
}

func archiveChat(i *Interactive) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return stripANSIBytes(strings.Join(i.stableChatRowsLocked(100), "\n"))
}

func TestChatArchiveShownAboveSeparator(t *testing.T) {
	path, live := writeArchiveSession(t, []string{"old question", "old answer"}, "kept tail text")
	i := newArchiveInteractive(t)
	hydrateArchive(i, path, live)
	waitArchive(t, i, 0)

	chat := archiveChat(i)
	oldAt, sepAt := strings.Index(chat, "old question"), strings.Index(chat, "compacted")
	require.True(t, oldAt >= 0 && sepAt > oldAt, chat)
	require.Contains(t, chat, "old answer")
	require.Equal(t, 1, strings.Count(chat, "kept tail text"), chat)
	require.Greater(t, strings.Index(chat, "kept tail text"), sepAt)
	require.NotContains(t, chat, "ctrl+f to search")
}

func TestChatArchiveOlderLineWhenOverBudget(t *testing.T) {
	var archived []string
	for n := range 100 {
		archived = append(archived, fmt.Sprintf("archived row %03d", n))
	}
	path, live := writeArchiveSession(t, archived, "kept")
	i := newArchiveInteractive(t)
	hydrateArchive(i, path, live)
	waitArchive(t, i, 0)

	// Live shows 2 messages, so 78 archived rows fit; 22 are older.
	chat := archiveChat(i)
	require.Contains(t, chat, "22 older messages · ctrl+f to search")
	require.NotContains(t, chat, "archived row 021")
	require.Contains(t, chat, "archived row 022")
	require.Contains(t, chat, "archived row 099")
}

func TestChatArchiveNothingWithoutCompaction(t *testing.T) {
	i := newArchiveInteractive(t)
	hydrateArchive(i, "/nonexistent", []provider.Message{jumpTextMsg(provider.RoleUser, "hello")})
	i.mu.Lock()
	cancel := i.chatArchive.cancel
	rows := len(i.chatArchive.hist.Rows)
	i.mu.Unlock()
	require.Nil(t, cancel, "no load without a compaction summary")
	require.Zero(t, rows)
	require.NotContains(t, archiveChat(i), "ctrl+f to search")
}

func TestChatArchiveClearedForSessionWithoutCompaction(t *testing.T) {
	path, live := writeArchiveSession(t, []string{"old question"}, "kept")
	i := newArchiveInteractive(t)
	hydrateArchive(i, path, live)
	waitArchive(t, i, 0)
	require.Contains(t, archiveChat(i), "old question")

	hydrateArchive(i, filepath.Join(t.TempDir(), "other.jsonl"), []provider.Message{jumpTextMsg(provider.RoleUser, "fresh")})
	require.NotContains(t, archiveChat(i), "old question")
}

func TestChatArchiveStaleLoadDropped(t *testing.T) {
	stalePath, staleLive := writeArchiveSession(t, []string{"stale archived"}, "kept")
	path, live := writeArchiveSession(t, []string{"current archived"}, "kept")
	i := newArchiveInteractive(t)
	hydrateArchive(i, stalePath, staleLive)
	i.mu.Lock()
	gen := i.chatArchive.gen
	i.mu.Unlock()
	// Switching sessions supersedes the first load before it is applied.
	hydrateArchive(i, path, live)
	i.applyChatArchive(gen, stalePath, core.ArchivedHistory{Rows: []core.ArchivedRow{{Message: jumpTextMsg(provider.RoleUser, "from stale load")}}})
	require.NotContains(t, archiveChat(i), "from stale load")
	// A result for another session path is dropped too.
	i.mu.Lock()
	cur := i.chatArchive.gen
	i.mu.Unlock()
	i.applyChatArchive(cur, "/other/session.jsonl", core.ArchivedHistory{Rows: []core.ArchivedRow{{Message: jumpTextMsg(provider.RoleUser, "from other path")}}})
	require.NotContains(t, archiveChat(i), "from other path")
	waitArchive(t, i, 0)
	chat := archiveChat(i)
	require.Contains(t, chat, "current archived")
	require.NotContains(t, chat, "stale archived")
}

func archiveHist(texts ...string) core.ArchivedHistory {
	var h core.ArchivedHistory
	for n, s := range texts {
		h.Rows = append(h.Rows, core.ArchivedRow{MsgSeq: n, Message: jumpTextMsg(provider.RoleUser, s)})
	}
	h.Marks = []core.ArchivedMark{{Index: len(texts), TokensBefore: 1000}}
	return h
}

func archiveState(i *Interactive) (rev, gen uint64, pending bool, budget int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	a := &i.chatArchive
	return a.rev, a.gen, a.pending != nil, a.budget
}

func TestChatArchiveSameSessionResnapshotKeepsArchive(t *testing.T) {
	path, live := writeArchiveSession(t, []string{"old question"}, "kept")
	i := newArchiveInteractive(t)
	hydrateArchive(i, path, live)
	waitArchive(t, i, 0)
	rev, gen, _, _ := archiveState(i)

	// Same session, same compaction (a new turn): no reload, no change.
	hydrateArchive(i, path, append(append([]provider.Message(nil), live...), jumpTextMsg(provider.RoleUser, "next turn")))
	rev2, gen2, _, _ := archiveState(i)
	require.Equal(t, gen, gen2, "no new load for the same session")
	require.Equal(t, rev, rev2)
	i.mu.Lock()
	require.Nil(t, i.chatArchive.cancel)
	i.mu.Unlock()
	require.Contains(t, archiveChat(i), "old question")

	// Live compaction: the archive of the older compaction is dropped so a
	// full repaint never shows it above the new separator. No reload.
	hydrateArchive(i, path, []provider.Message{archiveSummaryMsg("5000"), jumpTextMsg(provider.RoleUser, "newer")})
	_, _, pending, _ := archiveState(i)
	require.False(t, pending)
	i.mu.Lock()
	require.Nil(t, i.chatArchive.cancel, "no reload during the session")
	i.mu.Unlock()
	require.NotContains(t, archiveChat(i), "old question")
}

func TestChatArchiveEqualResultDoesNotRepaint(t *testing.T) {
	path, live := writeArchiveSession(t, []string{"old question"}, "kept")
	i := newArchiveInteractive(t)
	hydrateArchive(i, path, live)
	waitArchive(t, i, 0)
	rev, gen, _, _ := archiveState(i)
	i.mu.Lock()
	hist := i.chatArchive.hist
	i.mu.Unlock()
	i.applyChatArchive(gen, path, hist)
	rev2, _, _, _ := archiveState(i)
	require.Equal(t, rev, rev2)
}

func TestChatArchiveWaitsForIdleTurn(t *testing.T) {
	path, live := writeArchiveSession(t, []string{"old question"}, "kept")
	i := newArchiveInteractive(t)
	hydrateArchive(i, path, live)
	i.mu.Lock()
	i.busy = true // the attached session is running a turn
	i.mu.Unlock()
	_, gen, _, _ := archiveState(i)
	i.applyChatArchive(gen, path, archiveHist("during turn"))
	rev, _, pending, _ := archiveState(i)
	require.True(t, pending)
	require.Zero(t, rev, "nothing painted while a turn is active")
	require.NotContains(t, archiveChat(i), "during turn")

	i.mu.Lock()
	i.applyInfoLocked(session.Info{ID: "s", SessionPath: path, Run: session.RunStateIdle})
	i.mu.Unlock()
	rev, _, pending, _ = archiveState(i)
	require.False(t, pending)
	require.Equal(t, uint64(1), rev)
	require.Contains(t, archiveChat(i), "during turn")
}

func TestChatArchiveBudgetFrozenAtApply(t *testing.T) {
	var archived []string
	for n := range 100 {
		archived = append(archived, fmt.Sprintf("archived row %03d", n))
	}
	path, live := writeArchiveSession(t, archived, "kept")
	i := newArchiveInteractive(t)
	hydrateArchive(i, path, live)
	waitArchive(t, i, 0)
	_, _, _, budget := archiveState(i)
	require.Equal(t, 78, budget)
	before := archiveChat(i)
	require.Contains(t, before, "archived row 022")

	// New live messages do not push archived rows out of the top.
	i.mu.Lock()
	for range 5 {
		i.view.Messages = append(i.view.Messages, jumpTextMsg(provider.RoleUser, "live"))
	}
	i.stableChatCacheValid = false
	i.mu.Unlock()
	require.Contains(t, archiveChat(i), "archived row 022")
	require.Contains(t, archiveChat(i), "22 older messages")
}

func TestChatArchiveThemeChangeRerenders(t *testing.T) {
	path, live := writeArchiveSession(t, []string{"old question"}, "kept")
	i := newArchiveInteractive(t)
	hydrateArchive(i, path, live)
	waitArchive(t, i, 0)
	_ = archiveChat(i)
	i.mu.Lock()
	require.True(t, i.chatArchive.renderedOK)
	i.mu.Unlock()
	i.installResolvedTheme(tui.ThemeResolution{Theme: tui.Light})
	i.mu.Lock()
	require.False(t, i.chatArchive.renderedOK)
	i.mu.Unlock()
}

func TestChatArchiveLeavesModelContextUnchanged(t *testing.T) {
	path, live := writeArchiveSession(t, []string{"old question", "old answer"}, "kept tail text")
	i := newArchiveInteractive(t)
	hydrateArchive(i, path, live)
	i.mu.Lock()
	beforePort := append([]provider.Message(nil), i.portMsgs...)
	beforeView := len(i.view.Messages)
	i.mu.Unlock()
	waitArchive(t, i, 0)
	_ = archiveChat(i)
	i.mu.Lock()
	defer i.mu.Unlock()
	require.Equal(t, beforePort, i.portMsgs)
	require.Equal(t, live, i.portMsgs)
	require.Equal(t, beforeView, len(i.view.Messages))
}

func TestRenderChatArchiveInnerMarksOnly(t *testing.T) {
	row := func(s string) core.ArchivedRow { return core.ArchivedRow{Message: jumpTextMsg(provider.RoleUser, s)} }
	hist := core.ArchivedHistory{
		Rows:  []core.ArchivedRow{row("first"), row("second"), row("third")},
		Marks: []core.ArchivedMark{{Index: 1, TokensBefore: 5000}, {Index: 3, TokensBefore: 9000}},
	}
	chat := stripANSIBytes(strings.Join(renderChatArchive(tui.Dark, hist, 10, 80), "\n"))
	require.Equal(t, 1, strings.Count(chat, "compacted"), chat)
	require.Contains(t, chat, "~5k tokens")
	require.Less(t, strings.Index(chat, "first"), strings.Index(chat, "~5k"))
	require.Greater(t, strings.Index(chat, "second"), strings.Index(chat, "~5k"))
	// The inner mark falls before the shown window: no separator.
	chat = stripANSIBytes(strings.Join(renderChatArchive(tui.Dark, hist, 2, 80), "\n"))
	require.NotContains(t, chat, "compacted")
	require.Contains(t, chat, "1 older message · ctrl+f to search")
}

func newArchiveScreen(t *testing.T) (*Interactive, *alertTestTerminal) {
	t.Helper()
	i := newArchiveInteractive(t)
	term := &alertTestTerminal{}
	i.cfg.Terminal = term
	i.rend = tui.NewRenderer(term)
	i.rend.Resize(100, 40)
	return i, term
}

// screenSince returns the plain text written to term after mark bytes.
func screenSince(term *alertTestTerminal, mark int) string {
	return stripANSIBytes(term.String()[mark:])
}

func TestChatArchivePaintedWithFullRepaint(t *testing.T) {
	path, live := writeArchiveSession(t, []string{"old question"}, "kept tail text")
	i, term := newArchiveScreen(t)
	hydrateArchive(i, path, live)
	i.redraw()
	require.Contains(t, stripANSIBytes(term.String()), "kept tail text")

	mark := len(term.String())
	waitArchive(t, i, 0)
	i.redraw()
	raw := term.String()[mark:]
	require.Contains(t, raw, tui.SeqClearScreenNoHome, "the archive is applied with a full repaint")
	out := screenSince(term, mark)
	require.Equal(t, 1, strings.Count(out, "old question"), out)
	require.Equal(t, 1, strings.Count(out, "kept tail text"), out)
	require.Less(t, strings.Index(out, "old question"), strings.Index(out, "compacted"), out)
	require.Less(t, strings.Index(out, "compacted"), strings.Index(out, "kept tail text"), out)

	// An equal result repaints nothing.
	i.mu.Lock()
	gen, hist := i.chatArchive.gen, i.chatArchive.hist
	i.mu.Unlock()
	mark = len(term.String())
	i.applyChatArchive(gen, path, hist)
	i.redraw()
	require.NotContains(t, term.String()[mark:], tui.SeqClearScreenNoHome)
}

func TestChatArchivePendingDroppedOnSessionSwitch(t *testing.T) {
	path, live := writeArchiveSession(t, []string{"old question"}, "kept")
	i := newArchiveInteractive(t)
	hydrateArchive(i, path, live)
	i.mu.Lock()
	i.busy = true
	gen := i.chatArchive.gen
	i.mu.Unlock()
	i.applyChatArchive(gen, path, archiveHist("from session a"))
	_, _, pending, _ := archiveState(i)
	require.True(t, pending)

	hydrateArchive(i, filepath.Join(t.TempDir(), "b.jsonl"), []provider.Message{jumpTextMsg(provider.RoleUser, "fresh")})
	rev, _, pending, _ := archiveState(i)
	require.False(t, pending)
	require.Zero(t, rev)
	require.NotContains(t, archiveChat(i), "from session a")
}
