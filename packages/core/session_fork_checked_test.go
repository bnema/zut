package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/require"
)

func TestBranchSessionCheckedHistoricalSelectionAndIdentity(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	parent, err := NewSession(root, cwd, "test", "model", "test")
	require.NoError(t, err)
	require.NoError(t, parent.AppendMessage(textMessage(provider.RoleUser, "old prompt")))
	require.NoError(t, parent.AppendCompaction([]provider.Message{textMessage(provider.RoleUser, "summary")}))
	require.NoError(t, parent.Close())
	identity, err := os.Stat(parent.Path)
	require.NoError(t, err)
	segment := 0
	branch, err := BranchSessionChecked(context.Background(), parent.Path, root, cwd, "test", 1, true, &segment, identity)
	require.NoError(t, err)
	snapshot, err := ReadSessionSnapshot(branch)
	require.NoError(t, err)
	require.Len(t, snapshot.Messages, 1)
	require.Equal(t, "old prompt", snapshot.Messages[0].Content[0].(provider.TextBlock).Text)
	segment = 999
	_, err = BranchSessionChecked(context.Background(), parent.Path, root, cwd, "test", 1, true, &segment, identity)
	require.ErrorContains(t, err, "historical selection not found")
	segment = 0
	_, err = BranchSessionChecked(context.Background(), parent.Path, root, cwd, "test", 999, true, &segment, identity)
	require.Error(t, err)
	replacement := filepath.Join(t.TempDir(), "replacement.jsonl")
	require.NoError(t, os.WriteFile(replacement, []byte("{}\n"), 0o600))
	require.NoError(t, os.Rename(replacement, parent.Path))
	for _, history := range []*int{nil, &segment} {
		_, err := BranchSessionChecked(context.Background(), parent.Path, root, cwd, "test", 1, true, history, identity)
		require.ErrorContains(t, err, "identity changed")
	}
}
