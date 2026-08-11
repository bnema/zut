package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestFindManagedSessionByIDStrictScopesAllManagedStoresAndReturnsReadErrors(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	session, err := NewSession(root, cwd, "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "managed"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := FindManagedSessionByID(context.Background(), root, session.ID)
	if err != nil {
		t.Fatalf("FindManagedSessionByID: %v", err)
	}
	if got != session.Path {
		t.Fatalf("managed session path = %q, want %q", got, session.Path)
	}

	badPath := filepath.Join(SessionsDir(root, cwd), "bad.jsonl")
	if err := os.WriteFile(badPath, []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := FindManagedSessionByID(context.Background(), root, "missing"); err == nil {
		t.Fatal("malformed managed metadata was reported as not found")
	} else if !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("malformed metadata error = %v, want metadata context", err)
	}
}

func TestFindManagedSessionByIDHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := FindManagedSessionByID(ctx, t.TempDir(), "session-id"); !errors.Is(err, context.Canceled) {
		t.Fatalf("FindManagedSessionByID error = %v, want context.Canceled", err)
	}
}

// TestSessionExportImportRoundTrip writes a few messages to a live
// session, exports it, imports the export under a different cwd,
// and verifies OpenSession on the imported file yields the same
// message payloads.
func TestSessionExportImportRoundTrip(t *testing.T) {
	root := t.TempDir()
	originalCWD := "/path/to/project"
	sess, err := NewSession(root, originalCWD, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "hello from the exporter"}},
	})
	_ = sess.AppendMessage(provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: "hi — reply from the assistant"}},
	})
	handoff := json.RawMessage(`{"version":1,"reason":"status_rescue","rescue_attempts":1}`)
	if err := sess.UpdateCompactHandoff(handoff); err != nil {
		t.Fatal(err)
	}
	goal := &SessionGoal{Objective: "finish the portable task", Status: GoalPaused}
	if err := sess.UpdateGoal(goal); err != nil {
		t.Fatal(err)
	}
	_ = sess.Close()

	// Export to a directory — helper should build a name inside it.
	exportDir := t.TempDir()
	exportPath, err := ExportSession(sess.Path, exportDir)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if !strings.HasSuffix(exportPath, PortableExt) {
		t.Errorf("exported path should end in %s, got %q", PortableExt, exportPath)
	}
	if _, err := os.Stat(exportPath); err != nil {
		t.Fatalf("exported file doesn't exist: %v", err)
	}

	// Import into a different root + cwd.
	root2 := t.TempDir()
	cwd2 := "/some/other/project"
	importedPath, err := ImportSession(exportPath, root2, cwd2, "0.0.0-test")
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	if filepath.Dir(importedPath) != SessionsDir(root2, cwd2) {
		t.Errorf("imported file should land in SessionsDir, got %q", importedPath)
	}

	// Reopen and verify message round-trip.
	imported, msgs, err := OpenSession(importedPath)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer imported.Close()
	if imported.Meta.CWD != cwd2 {
		t.Errorf("meta cwd: want %q, got %q", cwd2, imported.Meta.CWD)
	}
	if imported.Meta.ID == sess.ID {
		t.Errorf("imported session kept the original id %q; must be rotated", sess.ID)
	}
	if imported.Meta.Model != "claude-opus-4-7" {
		t.Errorf("model not preserved: %q", imported.Meta.Model)
	}
	if got := string(imported.Meta.CompactHandoff); got != string(handoff) {
		t.Errorf("compact handoff = %q, want %q", got, handoff)
	}
	if imported.Meta.Goal == nil || imported.Meta.Goal.Objective != goal.Objective || imported.Meta.Goal.Status != goal.Status || imported.Meta.Goal.MissionID == "" {
		t.Errorf("goal = %#v", imported.Meta.Goal)
	}
	if imported.Meta.Mission == nil || imported.Meta.Mission.ID != imported.Meta.Goal.MissionID || imported.Meta.Mission.Objective != goal.Objective {
		t.Errorf("mission = %#v", imported.Meta.Mission)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	// Text should round-trip.
	if extractText(msgs[0]) != "hello from the exporter" {
		t.Errorf("msg 0 mismatch: %q", extractText(msgs[0]))
	}
	if extractText(msgs[1]) != "hi — reply from the assistant" {
		t.Errorf("msg 1 mismatch: %q", extractText(msgs[1]))
	}
}

func TestExportImportUsesLatestProviderModelMetadata(t *testing.T) {
	root := t.TempDir()
	session, err := NewSession(root, "/source", "old-provider", "old-model", "test")
	if err != nil {
		t.Fatal(err)
	}
	_ = session.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "metadata prompt"}},
	})
	if err := session.UpdateModel("new-provider", "new-model"); err != nil {
		t.Fatal(err)
	}
	path := session.Path
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	exportPath, err := ExportSession(path, filepath.Join(t.TempDir(), "latest"+PortableExt))
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	exported, err := ReadSessionSnapshot(exportPath)
	if err != nil {
		t.Fatalf("Read exported snapshot: %v", err)
	}
	if exported.Meta.Provider != "new-provider" || exported.Meta.Model != "new-model" {
		t.Fatalf("export metadata = provider %q model %q, want latest values", exported.Meta.Provider, exported.Meta.Model)
	}

	importedPath, err := ImportSession(exportPath, t.TempDir(), "/destination", "test")
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	imported, _, err := OpenSession(importedPath)
	if err != nil {
		t.Fatalf("Open imported session: %v", err)
	}
	defer imported.Close()
	if imported.Meta.Provider != "new-provider" || imported.Meta.Model != "new-model" {
		t.Fatalf("import metadata = provider %q model %q, want latest values", imported.Meta.Provider, imported.Meta.Model)
	}
}

// TestScanSessionMetaReturnsLatestRowWithoutHydratingMessages verifies that
// metadata lookup follows append-only model updates without reconstructing the
// transcript.
func TestScanSessionMetaReturnsLatestRowWithoutHydratingMessages(t *testing.T) {
	root := t.TempDir()
	session, err := NewSession(root, "/workspace", "old-provider", "old-model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: strings.Repeat("large transcript ", 1000)}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.UpdateModel("new-provider", "new-model"); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendExtensionState("tasked-phases", json.RawMessage(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	path := session.Path
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	meta, err := scanSessionMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Provider != "new-provider" || meta.Model != "new-model" {
		t.Fatalf("scanned metadata = %q/%q, want latest values", meta.Provider, meta.Model)
	}
}

// TestExportToFilePath writes to an explicit file path (no directory
// guessing) and checks the .zutsession extension is appended when missing.
func TestExportToFilePath(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root, "/cwd", "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "x"}},
	})
	_ = sess.Close()

	// No extension — should add .zutsession.
	dst := filepath.Join(t.TempDir(), "mysession")
	out, err := ExportSession(sess.Path, dst)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, PortableExt) {
		t.Errorf("want .zutsession suffix on %q", out)
	}
}

// TestExportStripsCWDFromMeta verifies the exported meta no longer
// carries the source user's cwd (not useful to the recipient).
func TestExportSessionHandlesHugeJSONLRows(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root, "/cwd", "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "first prompt names export"}},
	})
	_ = sess.AppendMessage(provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: strings.Repeat("x", 22*1024*1024)}},
	})
	_ = sess.Close()

	exportDir := t.TempDir()
	out, err := ExportSession(sess.Path, exportDir)
	if err != nil {
		t.Fatalf("ExportSession with huge row: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("exported file missing: %v", err)
	}
	opened, msgs, err := OpenSession(sess.Path)
	if err != nil {
		t.Fatalf("OpenSession with huge row: %v", err)
	}
	_ = opened.Close()
	if len(msgs) != 2 {
		t.Fatalf("OpenSession messages=%d, want 2", len(msgs))
	}
	// Ensure the exported file is still readable JSONL and contains the
	// huge assistant message row, not just the meta/header.
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), strings.Repeat("x", 1024)) {
		t.Fatalf("export does not appear to contain huge assistant row")
	}
	for n, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("exported line %d is invalid json: %v", n+1, err)
		}
	}
}

func TestExportStripsCWDFromMeta(t *testing.T) {
	root := t.TempDir()
	sess, err := NewSession(root, "/original/cwd", "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "x"}},
	})
	_ = sess.Close()

	out, err := ExportSession(sess.Path, filepath.Join(t.TempDir(), "x"+PortableExt))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if strings.Contains(string(b), "/original/cwd") {
		t.Errorf("exported file leaks the source cwd: %s", string(b))
	}
}

// TestBranchSessionCopiesPrefix writes several messages to a
// session, branches at message index 2 (first user + first
// assistant), and verifies the new session has exactly those two
// messages with parent + fork_point meta set.
func TestBranchSessionCopiesPrefix(t *testing.T) {
	root := t.TempDir()
	cwd := "/project"
	parent, err := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = parent.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "first"}},
	})
	_ = parent.AppendMessage(provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: "first reply"}},
	})
	_ = parent.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "second"}},
	})
	_ = parent.Close()

	// Branch at the first user+assistant pair (upToMessageIdx=2).
	branchPath, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 2)
	if err != nil {
		t.Fatalf("BranchSession: %v", err)
	}
	branch, msgs, err := OpenSession(branchPath)
	if err != nil {
		t.Fatal(err)
	}
	defer branch.Close()

	if len(msgs) != 2 {
		t.Errorf("want 2 copied messages, got %d", len(msgs))
	}
	if branch.Meta.Parent != parent.Meta.ID {
		t.Errorf("parent id: want %q, got %q", parent.Meta.ID, branch.Meta.Parent)
	}
	if branch.Meta.ForkPoint != 2 {
		t.Errorf("fork_point: want 2, got %d", branch.Meta.ForkPoint)
	}
	if branch.Meta.ID == parent.Meta.ID {
		t.Errorf("branch kept parent id; must rotate")
	}
	if branch.Meta.Title != "" {
		t.Errorf("meta title should remain append-only, got %q", branch.Meta.Title)
	}
	if summary := describeSession(branchPath); summary.Title != "first" {
		t.Errorf("branch title: want %q, got %q", "first", summary.Title)
	}
}

func TestBranchSessionTitleUsesSelectedUserPrompt(t *testing.T) {
	root := t.TempDir()
	cwd := "/project"
	parent, err := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "first prompt"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "first reply"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "second prompt"}}},
	} {
		_ = parent.AppendMessage(msg)
	}
	_ = parent.Close()

	branchPath, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 3)
	if err != nil {
		t.Fatalf("BranchSession: %v", err)
	}
	if summary := describeSession(branchPath); summary.Title != "second prompt" {
		t.Errorf("branch title: want %q, got %q", "second prompt", summary.Title)
	}
}

func TestBranchSessionTitleUsesFirstPromptAfterForkWhenBranchDiverges(t *testing.T) {
	root := t.TempDir()
	cwd := "/project"
	parent, err := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = parent.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "hello"}}})
	_ = parent.Close()

	branchPath, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 1)
	if err != nil {
		t.Fatalf("BranchSession: %v", err)
	}
	branch, _, err := OpenSession(branchPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = branch.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "how are you?"}}})
	_ = branch.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "fine"}}})
	_ = branch.Close()

	if summary := describeSession(branchPath); summary.Title != "how are you?" {
		t.Errorf("branch title: want %q, got %q", "how are you?", summary.Title)
	}
}

func TestGeneratedBranchTitleWinsWhenWrittenBeforeBranchPrompt(t *testing.T) {
	root := t.TempDir()
	cwd := "/project"
	parent, err := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = parent.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "hello"}}})
	_ = parent.Close()

	branchPath, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 1)
	if err != nil {
		t.Fatalf("BranchSession: %v", err)
	}
	branch, _, err := OpenSession(branchPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := branch.UpdateTitle("AI branch title"); err != nil {
		t.Fatal(err)
	}
	if err := branch.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "how are you?"}}}); err != nil {
		t.Fatal(err)
	}
	if err := branch.Close(); err != nil {
		t.Fatal(err)
	}

	if summary := describeSession(branchPath); summary.Title != "AI branch title" {
		t.Errorf("branch title: want %q, got %q", "AI branch title", summary.Title)
	}
}

func TestRenamedBranchKeepsExplicitTitle(t *testing.T) {
	root := t.TempDir()
	cwd := "/project"
	parent, err := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = parent.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "hello"}}})
	_ = parent.Close()

	branchPath, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 1)
	if err != nil {
		t.Fatalf("BranchSession: %v", err)
	}
	branch, _, err := OpenSession(branchPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = branch.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "how are you?"}}})
	_ = branch.Close()
	if err := RenameSession(branchPath, "friendly branch name"); err != nil {
		t.Fatal(err)
	}

	if summary := describeSession(branchPath); summary.Title != "friendly branch name" {
		t.Errorf("branch title: want %q, got %q", "friendly branch name", summary.Title)
	}
}

func TestBranchSessionUsesEffectiveTranscriptAfterCompaction(t *testing.T) {
	root := t.TempDir()
	cwd := "/project"
	parent, err := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"raw-a", "raw-b", "raw-c", "raw-d"} {
		_ = parent.AppendMessage(provider.Message{
			Role:    provider.RoleUser,
			Content: []provider.Content{provider.TextBlock{Text: text}},
		})
	}
	_ = parent.AppendCompaction([]provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "summary"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "tail-c"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "tail-d"}}},
	})
	_ = parent.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "after-compact"}},
	})
	_ = parent.Close()

	opened, msgs, err := OpenSession(parent.Path)
	if err != nil {
		t.Fatalf("OpenSession parent: %v", err)
	}
	_ = opened.Close()
	assertMessageTexts(t, msgs, []string{"summary", "tail-c", "tail-d", "after-compact"})

	branchPath, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 4)
	if err != nil {
		t.Fatalf("BranchSession: %v", err)
	}
	branch, branchMsgs, err := OpenSession(branchPath)
	if err != nil {
		t.Fatalf("OpenSession branch: %v", err)
	}
	defer branch.Close()

	assertMessageTexts(t, branchMsgs, []string{"summary", "tail-c", "tail-d", "after-compact"})
	if branch.Meta.Parent != parent.Meta.ID {
		t.Errorf("parent id: want %q, got %q", parent.Meta.ID, branch.Meta.Parent)
	}
	if branch.Meta.ForkPoint != 4 {
		t.Errorf("fork_point: want 4, got %d", branch.Meta.ForkPoint)
	}

	shortBranchPath, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 2)
	if err != nil {
		t.Fatalf("BranchSession short fork: %v", err)
	}
	shortBranch, shortBranchMsgs, err := OpenSession(shortBranchPath)
	if err != nil {
		t.Fatalf("OpenSession short branch: %v", err)
	}
	defer shortBranch.Close()

	assertMessageTexts(t, shortBranchMsgs, []string{"summary", "tail-c"})
	if shortBranch.Meta.ForkPoint != 2 {
		t.Errorf("short branch fork_point: want 2, got %d", shortBranch.Meta.ForkPoint)
	}
}

func TestBranchSessionCompactHandoffFollowsCopiedEffectiveTail(t *testing.T) {
	root := t.TempDir()
	cwd := "/project"
	parent, err := NewSession(root, cwd, "anthropic", "claude", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first", "second"} {
		if err := parent.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: text}}}); err != nil {
			t.Fatal(err)
		}
	}
	handoff := json.RawMessage(`{"version":1,"reason":"status_rescue","rescue_attempts":1}`)
	if err := parent.UpdateCompactHandoff(handoff); err != nil {
		t.Fatal(err)
	}
	goal := &SessionGoal{Objective: "finish the branch", Status: GoalActive}
	if err := parent.UpdateGoal(goal); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}

	fullPath, err := BranchSession(parent.Path, root, cwd, "test", 2)
	if err != nil {
		t.Fatal(err)
	}
	full, _, err := OpenSession(fullPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = full.Close() })
	if got := string(full.Meta.CompactHandoff); got != string(handoff) {
		t.Fatalf("full branch compact handoff = %q, want %q", got, handoff)
	}
	if full.Meta.Goal == nil || full.Meta.Goal.Objective != goal.Objective || full.Meta.Goal.Status != goal.Status || full.Meta.Goal.MissionID == "" {
		t.Fatalf("full branch goal = %#v", full.Meta.Goal)
	}
	if full.Meta.Mission == nil || full.Meta.Mission.ID != full.Meta.Goal.MissionID {
		t.Fatalf("full branch mission = %#v", full.Meta.Mission)
	}

	prefixPath, err := BranchSession(parent.Path, root, cwd, "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	prefix, _, err := OpenSession(prefixPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prefix.Close() })
	if len(prefix.Meta.CompactHandoff) != 0 {
		t.Fatalf("prefix branch compact handoff = %q, want empty", prefix.Meta.CompactHandoff)
	}
	if prefix.Meta.Goal != nil {
		t.Fatalf("prefix branch goal = %#v, want nil", prefix.Meta.Goal)
	}
	if prefix.Meta.Mission != nil || len(prefix.Meta.GoalHistory) != 0 {
		t.Fatalf("prefix branch mission/history = %#v / %#v, want nil", prefix.Meta.Mission, prefix.Meta.GoalHistory)
	}
}

func TestBranchSessionHiddenFromHistoryClearsCompactHandoff(t *testing.T) {
	root := t.TempDir()
	cwd := "/project"
	parent, err := NewSession(root, cwd, "anthropic", "claude", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "old"}}}); err != nil {
		t.Fatal(err)
	}
	if err := parent.AppendCompaction([]provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "summary"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := parent.UpdateCompactHandoff(json.RawMessage(`{"version":1,"reason":"forced_length"}`)); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}

	history, err := ReadSessionHistory(parent.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Segments) < 2 {
		t.Fatalf("history segments = %d, want at least 2", len(history.Segments))
	}
	branchPath, err := BranchSessionHiddenFromHistory(parent.Path, root, cwd, "test", history.Segments[0], len(history.Segments[0].Messages))
	if err != nil {
		t.Fatal(err)
	}
	branch, _, err := OpenSession(branchPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = branch.Close() })
	if len(branch.Meta.CompactHandoff) != 0 {
		t.Fatalf("historical branch compact handoff = %q, want empty", branch.Meta.CompactHandoff)
	}
}

func TestBranchSessionCopiesExtensionStateAtFork(t *testing.T) {
	root := t.TempDir()
	cwd := "/project"
	parent, err := NewSession(root, cwd, "anthropic", "claude", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "first"}}}); err != nil {
		t.Fatal(err)
	}
	if err := parent.AppendExtensionState("tasked-phases", json.RawMessage(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := parent.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "second"}}}); err != nil {
		t.Fatal(err)
	}
	if err := parent.AppendExtensionState("tasked-phases", json.RawMessage(`{"version":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}

	branchPath, err := BranchSession(parent.Path, root, cwd, "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	branch, _, err := OpenSession(branchPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = branch.Close() })
	if got := string(branch.ExtensionState["tasked-phases"]); got != `{"version":1}` {
		t.Fatalf("branch extension state = %q, want version 1", got)
	}
}

func TestBranchSessionCarriesStateAcrossCompactionBoundary(t *testing.T) {
	root := t.TempDir()
	cwd := "/project"
	parent, err := NewSession(root, cwd, "anthropic", "claude", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"before-a", "before-b"} {
		if err := parent.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: text}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := parent.AppendExtensionState("tasked-phases", json.RawMessage(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := parent.AppendCompaction([]provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "summary"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "tail"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}

	branchPath, err := BranchSession(parent.Path, root, cwd, "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	branch, _, err := OpenSession(branchPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = branch.Close() })
	if got := string(branch.ExtensionState["tasked-phases"]); got != `{"version":1}` {
		t.Fatalf("compacted branch extension state = %q, want version 1", got)
	}
}

// TestBuildSessionTree verifies parent/child edges are rebuilt
// from meta + sibling-scan.
func TestBuildSessionTree(t *testing.T) {
	root := t.TempDir()
	cwd := "/project"
	parent, _ := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	_ = parent.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "x"}},
	})
	_ = parent.Close()

	childA, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 1)
	if err != nil {
		t.Fatal(err)
	}
	childB, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 1)
	if err != nil {
		t.Fatal(err)
	}
	_ = childA
	_ = childB

	tree := BuildSessionTree(root, cwd)
	if len(tree) != 1 {
		t.Fatalf("want 1 root, got %d", len(tree))
	}
	rootNode := tree[0]
	if rootNode.Meta.ID != parent.Meta.ID {
		t.Errorf("root should be parent %q, got %q", parent.Meta.ID, rootNode.Meta.ID)
	}
	if len(rootNode.Children) != 2 {
		t.Errorf("want 2 children, got %d", len(rootNode.Children))
	}
}

func assertMessageTexts(t *testing.T, msgs []provider.Message, want []string) {
	t.Helper()
	if len(msgs) != len(want) {
		t.Fatalf("message count: want %d, got %d", len(want), len(msgs))
	}
	for i, msg := range msgs {
		if got := extractText(msg); got != want[i] {
			t.Errorf("message %d: want %q, got %q", i, want[i], got)
		}
	}
}
