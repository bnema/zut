package modes

import (
	"fmt"
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

func TestChatArchiveClearedAfterReplacementWithoutCompaction(t *testing.T) {
	path, live := writeArchiveSession(t, []string{"old question"}, "kept")
	i := newArchiveInteractive(t)
	hydrateArchive(i, path, live)
	waitArchive(t, i, 0)
	require.Contains(t, archiveChat(i), "old question")

	hydrateArchive(i, path, []provider.Message{jumpTextMsg(provider.RoleUser, "fresh")})
	require.NotContains(t, archiveChat(i), "old question")
}

func TestChatArchiveStaleLoadDropped(t *testing.T) {
	path, live := writeArchiveSession(t, []string{"stale archived"}, "kept")
	i := newArchiveInteractive(t)
	hydrateArchive(i, path, live)
	i.mu.Lock()
	gen := i.chatArchive.gen
	i.mu.Unlock()
	// A newer load supersedes the first one before it is applied.
	hydrateArchive(i, path, live)
	i.applyChatArchive(gen, path, core.ArchivedHistory{Rows: []core.ArchivedRow{{Message: jumpTextMsg(provider.RoleUser, "from stale load")}}})
	require.NotContains(t, archiveChat(i), "from stale load")
	// A result for another session path is dropped too.
	i.mu.Lock()
	cur := i.chatArchive.gen
	i.mu.Unlock()
	i.applyChatArchive(cur, "/other/session.jsonl", core.ArchivedHistory{Rows: []core.ArchivedRow{{Message: jumpTextMsg(provider.RoleUser, "from other path")}}})
	require.NotContains(t, archiveChat(i), "from other path")
	waitArchive(t, i, 0)
	require.Contains(t, archiveChat(i), "stale archived")
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
