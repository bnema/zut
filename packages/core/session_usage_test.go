package core

import (
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

func TestSessionUsageDetailDeltasMeasuredCacheUsage(t *testing.T) {
	sess := newUsageTestSession(t)
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "prompt"}}}); err != nil {
		t.Fatal(err)
	}
	first := provider.Usage{
		InputTokens:               60,
		CacheReadTokens:           40,
		CacheMeasuredPromptTokens: 100,
		CacheMeasuredReadTokens:   40,
	}
	if err := sess.AppendUsage(first, first); err != nil {
		t.Fatal(err)
	}
	second := provider.Usage{
		InputTokens:               80,
		CacheReadTokens:           70,
		CacheMeasuredPromptTokens: 150,
		CacheMeasuredReadTokens:   70,
	}
	if err := sess.AppendUsage(provider.Usage{}, second); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	cumulative, last, err := SessionUsageDetail(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	if cumulative.CacheMeasuredPromptTokens != 150 || cumulative.CacheMeasuredReadTokens != 70 {
		t.Fatalf("cumulative measured cache usage = %+v", cumulative)
	}
	if last.CacheMeasuredPromptTokens != 50 || last.CacheMeasuredReadTokens != 30 {
		t.Fatalf("last measured cache usage = %+v", last)
	}
}

func TestSessionAppendUsageSkipsConsecutiveDuplicateCheckpoint(t *testing.T) {
	sess := newUsageTestSession(t)
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "prompt"}}}); err != nil {
		t.Fatal(err)
	}
	usage := provider.Usage{InputTokens: 120, OutputTokens: 8, CostUSD: 0.25}
	if err := sess.AppendUsage(usage, usage); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendUsage(usage, usage); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	snapshot, err := ReadSessionSnapshot(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(snapshot.UsageCheckpoints); got != 1 {
		t.Fatalf("usage checkpoints = %d, want 1 for consecutive duplicate usage", got)
	}
}

func TestSessionAppendUsagePreservesCheckpointAfterMessage(t *testing.T) {
	sess := newUsageTestSession(t)
	usage := provider.Usage{InputTokens: 120, OutputTokens: 8}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "first"}}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendUsage(usage, usage); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "second"}}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendUsage(usage, usage); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	snapshot, err := ReadSessionSnapshot(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(snapshot.UsageCheckpoints); got != 2 {
		t.Fatalf("usage checkpoints = %d, want 2 across transcript changes", got)
	}
	if got := snapshot.UsageCheckpoints[1].MessageCount; got != 2 {
		t.Fatalf("second checkpoint message count = %d, want 2", got)
	}
}

func TestSessionUsageDetailResetsContextAfterCompaction(t *testing.T) {
	t.Run("compaction is latest row", func(t *testing.T) {
		sess := newUsageTestSession(t)
		before := provider.Usage{
			InputTokens:     180,
			OutputTokens:    20,
			CacheReadTokens: 10,
			CostUSD:         1.25,
		}
		if err := sess.AppendUsage(before, before); err != nil {
			t.Fatal(err)
		}
		if err := sess.AppendCompaction([]provider.Message{{
			Role:    provider.RoleUser,
			Content: []provider.Content{provider.TextBlock{Text: "summary"}},
			Time:    time.Now().UTC(),
		}}); err != nil {
			t.Fatal(err)
		}
		if err := sess.Close(); err != nil {
			t.Fatal(err)
		}

		cumulative, lastTurn, err := SessionUsageDetail(sess.Path)
		if err != nil {
			t.Fatal(err)
		}
		if cumulative.InputTokens != before.InputTokens || cumulative.CostUSD != before.CostUSD {
			t.Fatalf("cumulative usage = %+v, want %+v", cumulative, before)
		}
		if lastTurn != (provider.Usage{}) {
			t.Fatalf("last turn after compaction = %+v, want zero context usage", lastTurn)
		}
	})

	t.Run("post-compaction usage wins", func(t *testing.T) {
		sess := newUsageTestSession(t)
		before := provider.Usage{InputTokens: 180, OutputTokens: 20, CacheReadTokens: 10}
		if err := sess.AppendUsage(before, before); err != nil {
			t.Fatal(err)
		}
		if err := sess.AppendCompaction([]provider.Message{{
			Role:    provider.RoleUser,
			Content: []provider.Content{provider.TextBlock{Text: "summary"}},
			Time:    time.Now().UTC(),
		}}); err != nil {
			t.Fatal(err)
		}
		after := provider.Usage{InputTokens: 20, OutputTokens: 5, CacheReadTokens: 30}
		cumulative := provider.Usage{InputTokens: 200, OutputTokens: 25, CacheReadTokens: 30}
		if err := sess.AppendUsage(after, cumulative); err != nil {
			t.Fatal(err)
		}
		if err := sess.Close(); err != nil {
			t.Fatal(err)
		}

		_, lastTurn, err := SessionUsageDetail(sess.Path)
		if err != nil {
			t.Fatal(err)
		}
		if lastTurn.InputTokens != after.InputTokens || lastTurn.OutputTokens != after.OutputTokens {
			t.Fatalf("post-compaction last turn = %+v, want input/output %+v", lastTurn, after)
		}
	})
}

func newUsageTestSession(t *testing.T) *Session {
	t.Helper()
	sess, err := NewSession(t.TempDir(), "/workspace", "anthropic", "test-model", "test")
	if err != nil {
		t.Fatal(err)
	}
	return sess
}
