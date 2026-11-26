package modes

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bnema/zut/packages/agent/internal/sessionindex"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// archiveReaderFixture hydrates a session with two compactions: archived
// eras "ancient needle" / "older needle", then a live tail.
func archiveReaderFixture(t *testing.T) (*Interactive, string, []provider.Message) {
	t.Helper()
	sess, err := core.NewSession(t.TempDir(), "/work", "test", "model", "test")
	require.NoError(t, err)
	first := jumpTextMsg(provider.RoleUser, "ancient needle question")
	require.NoError(t, sess.AppendMessage(first))
	require.NoError(t, sess.AppendMessage(jumpTextMsg(provider.RoleAssistant, "ancient answer")))
	sum1 := archiveSummaryMsg("50000")
	mid := jumpTextMsg(provider.RoleUser, "older needle question")
	require.NoError(t, sess.AppendCompaction([]provider.Message{sum1}))
	require.NoError(t, sess.AppendMessage(mid))
	live := []provider.Message{archiveSummaryMsg("90000"), jumpTextMsg(provider.RoleUser, "live needle tail")}
	require.NoError(t, sess.AppendMessage(live[1]))
	require.NoError(t, sess.AppendCompaction(live))
	require.NoError(t, sess.Close())
	i := newArchiveInteractive(t)
	hydrateArchive(i, sess.Path, live)
	waitArchive(t, i, 0)
	i.mu.Lock()
	i.cachedChatLocked(80)
	i.mu.Unlock()
	return i, sess.Path, live
}

func waitReader(t *testing.T, i *Interactive) {
	t.Helper()
	driveFinderLoop(t, i, func() bool {
		i.mu.Lock()
		defer i.mu.Unlock()
		return i.searchReader == nil || i.searchReader.ready
	})
}

func readerText(i *Interactive) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return stripANSIBytes(strings.Join(i.searchReader.rows, "\n"))
}

func TestSearchReaderShowsArchivedRowsAndMatches(t *testing.T) {
	i, _, live := archiveReaderFixture(t)
	i.mu.Lock()
	beforeView := len(i.view.Messages)
	i.mu.Unlock()
	i.openSearchReader(searchReaderTarget{terms: []string{"needle"}, focus: 1})
	waitReader(t, i)

	text := readerText(i)
	anc, old, sep, tail := strings.Index(text, "ancient needle"), strings.Index(text, "older needle"), strings.Index(text, "~50k"), strings.Index(text, "live needle tail")
	require.True(t, anc >= 0 && anc < sep && sep < old && old < tail, text)

	i.mu.Lock()
	r := i.searchReader
	require.Len(t, r.archive.Rows, 3)
	require.Equal(t, []int{0, 2, 4}, r.matches)
	require.Equal(t, 2, r.cur, "focus on the live tail")
	i.mu.Unlock()

	i.handleSearchReaderKey(tui.Key{Kind: tui.KeyCtrlN})
	cur, _ := readerCur(i)
	require.Equal(t, 0, cur, "ctrl+n wraps to the archived match")
	i.handleSearchReaderKey(tui.Key{Kind: tui.KeyCtrlN})
	cur, _ = readerCur(i)
	require.Equal(t, 2, cur)

	i.mu.Lock()
	require.Equal(t, beforeView, len(i.view.Messages))
	require.Equal(t, live, i.portMsgs)
	i.mu.Unlock()

	i.handleSearchReaderKey(tui.Key{Kind: tui.KeyEsc})
	i.mu.Lock()
	require.Nil(t, i.searchReader)
	i.mu.Unlock()
}

func TestSearchReaderOpensFinderHitInArchivedEra(t *testing.T) {
	i, _, _ := archiveReaderFixture(t)
	i.mu.Lock()
	path := i.sessionPathLocked()
	i.mu.Unlock()
	hist, err := core.ReadArchivedSessionText(t.Context(), path, core.ArchivedOptions{Skip: isHiddenTranscriptMessage})
	require.NoError(t, err)
	target := hist.Rows[0]
	hit := &sessionindex.HitRef{Fingerprint: core.MessageTextFingerprint(target.Message), MsgSeq: target.MsgSeq, TotalMsgs: 10}
	i.openSearchReader(searchReaderTarget{terms: []string{"needle"}, hit: hit})
	waitReader(t, i)
	cur, ok := readerCur(i)
	require.True(t, ok)
	require.Equal(t, 0, cur)
	require.NotContains(t, i.statusOK, "compacted history")

	// Wrong fingerprint: neither resolves, a note is shown.
	i.mu.Lock()
	i.searchReader = nil
	i.mu.Unlock()
	hit = &sessionindex.HitRef{Fingerprint: 999, MsgSeq: target.MsgSeq, TotalMsgs: 10}
	i.openSearchReader(searchReaderTarget{terms: []string{"needle"}, hit: hit})
	waitReader(t, i)
	require.Contains(t, i.statusOK, "no longer in the session history")
}

func TestSearchReaderNoArchiveWithoutCompaction(t *testing.T) {
	i, _ := readerFixture(t)
	openReader(t, i, []string{"refacto"}, 20)
	i.mu.Lock()
	defer i.mu.Unlock()
	require.Empty(t, i.searchReader.archivePath)
	require.Empty(t, i.searchReader.archive.Rows)
}
