package subagents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestResidentJournalAcceptCommitsAuthorityBeforeMetadataProjection(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "child-1")
	if err != nil {
		t.Fatalf("OpenResidentJournal: %v", err)
	}
	defer journal.Close()

	spec := ResidentChildSpec{
		ID:              "child-1",
		SessionID:       "child-session-1",
		RootCacheID:     "root-cache-1",
		ParentSessionID: "parent-session-1",
		Provider:        "openai-codex",
		Model:           "gpt-5.6-terra",
		Profile:         "reviewer",
		Tools:           []string{"read", "bash"},
	}
	if err := journal.Accept(spec, "review this change"); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	records, err := ReadResidentJournal(filepath.Join(root, "child-1", residentTranscriptName))
	if err != nil {
		t.Fatalf("ReadResidentJournal: %v", err)
	}
	if len(records) != 1 || records[0].Type != residentRecordAccepted {
		t.Fatalf("records = %#v, want one accepted record", records)
	}
	if records[0].Spec == nil || records[0].Spec.SessionID != spec.SessionID || records[0].Spec.RootCacheID != spec.RootCacheID || records[0].Prompt != "review this change" {
		t.Fatalf("accepted record = %#v", records[0])
	}

	meta, err := ReadResidentMetadata(filepath.Join(root, "child-1", residentMetadataName))
	if err != nil {
		t.Fatalf("ReadResidentMetadata: %v", err)
	}
	if meta.State != ResidentQueued || meta.SessionID != spec.SessionID || meta.RootCacheID != spec.RootCacheID {
		t.Fatalf("metadata = %#v", meta)
	}
	info, err := os.Stat(filepath.Join(root, "child-1", residentTranscriptName))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("transcript permissions = %o, want no group/other access", info.Mode().Perm())
	}
}

func TestResidentJournalReconcilesUsageMetadata(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "usage-child")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "usage-child", SessionID: "usage-session", Provider: "openai-codex", Model: "gpt-test"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	journal.ConfigureUsage(272_000, true)
	turn := provider.Usage{InputTokens: 84_000, OutputTokens: 1_500, CacheReadTokens: 123_000, CostUSD: 0.525}
	if err := journal.RecordAgentEvent(core.EvUsage{Usage: turn, Cumulative: turn}); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvUsage{Usage: provider.Usage{}, Cumulative: turn}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	metadata, err := ReconcileResidentJournal(filepath.Join(root, spec.ID))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Usage != turn || metadata.ContextUsed != 207_000 || metadata.ContextMax != 272_000 || !metadata.Subscription {
		t.Fatalf("metadata = %#v, want durable usage projection", metadata)
	}
}

func TestResidentJournalSkipsDuplicateUsageRecord(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "duplicate-usage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	spec := ResidentChildSpec{ID: "duplicate-usage", SessionID: "usage-session", Provider: "openai", Model: "gpt-test"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	journal.ConfigureUsage(128_000, false)
	usage := provider.Usage{InputTokens: 10_000, OutputTokens: 500}
	event := core.EvUsage{Usage: usage, Cumulative: usage}
	if err := journal.RecordAgentEvent(event); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(event); err != nil {
		t.Fatal(err)
	}

	records, err := ReadResidentJournal(filepath.Join(journal.Dir(), residentTranscriptName))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(records); got != 2 {
		t.Fatalf("journal records = %d, want accepted record plus one usage record", got)
	}
}

func TestResidentJournalDuplicateUsageRetriesFailedMetadataProjection(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "retry-usage-projection")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	spec := ResidentChildSpec{ID: "retry-usage-projection", SessionID: "usage-session", Provider: "openai", Model: "gpt-test"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	journal.ConfigureUsage(128_000, false)
	metadataPath := filepath.Join(journal.Dir(), residentMetadataName)
	originalMetadata, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	usage := provider.Usage{InputTokens: 10_000, OutputTokens: 500}
	event := core.EvUsage{Usage: usage, Cumulative: usage}
	if err := journal.RecordAgentEvent(event); err == nil {
		t.Fatal("first usage projection succeeded with malformed metadata")
	}
	if err := os.WriteFile(metadataPath, originalMetadata, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(event); err != nil {
		t.Fatalf("retry identical usage: %v", err)
	}

	records, err := ReadResidentJournal(filepath.Join(journal.Dir(), residentTranscriptName))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(records); got != 2 {
		t.Fatalf("journal records = %d, want accepted record plus one usage record", got)
	}
	metadata, err := ReadResidentMetadata(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Usage != usage || metadata.ContextUsed != usage.PromptTokens() {
		t.Fatalf("metadata usage = %+v, context = %d; want %+v, %d", metadata.Usage, metadata.ContextUsed, usage, usage.PromptTokens())
	}
}

func TestReconcileResidentJournalRetainsRootCacheID(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "cache-child")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "cache-child", SessionID: "child-session", RootCacheID: "root-cache", Provider: "openai", Model: "gpt-test"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	metadata, err := ReconcileResidentJournal(filepath.Join(root, spec.ID))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.RootCacheID != spec.RootCacheID {
		t.Fatalf("RootCacheID = %q, want %q", metadata.RootCacheID, spec.RootCacheID)
	}
}

func TestResidentJournalProjectsUsageEmittedAfterInterruption(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "interrupted-usage")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	spec := ResidentChildSpec{ID: "interrupted-usage", SessionID: "usage-session", Provider: "openai", Model: "gpt-test"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	journal.ConfigureUsage(272_000, false)
	if err := journal.RecordTurnStarted(spec, "turn-1"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnInterrupted(spec, "turn-1"); err != nil {
		t.Fatal(err)
	}
	usage := provider.Usage{InputTokens: 84_000, CacheReadTokens: 123_000}
	if err := journal.RecordAgentEvent(core.EvUsage{Usage: usage, Cumulative: usage}); err != nil {
		t.Fatal(err)
	}
	metadata, err := ReadResidentMetadata(filepath.Join(journal.Dir(), residentMetadataName))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.State != ResidentInterrupted || metadata.Usage != usage || metadata.ContextUsed != 207_000 {
		t.Fatalf("metadata = %#v, want interrupted state with latest usage", metadata)
	}
}

func TestResidentProjectionSkipsIdenticalRewrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no-follow projection reads are unavailable on Windows")
	}
	dir := t.TempDir()
	metadata := ResidentMetadata{
		Version:   residentJournalVersion,
		ID:        "projection-child",
		SessionID: "projection-session",
		State:     ResidentIdle,
		UpdatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	if err := writeResidentMetadata(dir, metadata); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, residentMetadataName)
	oldTime := time.Unix(1_600_000_000, 0)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	if err := writeResidentMetadata(dir, metadata); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(oldTime) {
		t.Fatalf("identical projection rewrite changed mtime to %v", info.ModTime())
	}

	metadata.State = ResidentStopped
	if err := writeResidentMetadata(dir, metadata); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Equal(oldTime) {
		t.Fatal("changed projection was not rewritten")
	}
}

func TestResidentProjectionReplacesSymlinkInsteadOfFollowingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated privileges on Windows")
	}
	dir := t.TempDir()
	metadata := ResidentMetadata{Version: residentJournalVersion, ID: "projection-child", SessionID: "projection-session", State: ResidentIdle}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, residentMetadataName)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	if err := writeResidentMetadata(dir, metadata); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("projection mode = %v, want regular file", info.Mode())
	}
}

func TestResidentProjectionRepairsPermissionsForIdenticalData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	dir := t.TempDir()
	metadata := ResidentMetadata{Version: residentJournalVersion, ID: "projection-child", SessionID: "projection-session", State: ResidentIdle}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, residentMetadataName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeResidentMetadata(dir, metadata); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("projection permissions = %o, want 600", got)
	}
}

func TestOpenResidentJournalRejectsDotChildIDs(t *testing.T) {
	for _, childID := range []string{".", "..", "nested/child"} {
		t.Run(childID, func(t *testing.T) {
			if _, err := OpenResidentJournal(t.TempDir(), childID); err == nil {
				t.Fatalf("OpenResidentJournal(%q) succeeded", childID)
			}
		})
	}
}

func TestResidentJournalPersistsFinalizedAgentEvents(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "child-events")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "child-events", SessionID: "child-session", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	message := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done"}}}
	if err := journal.RecordAgentEvent(core.EvAssistantMessage{Message: message}); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvToolCall{ID: "call-1", Name: "bash", Args: json.RawMessage(`{"command":"pwd"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	records, err := ReadResidentJournal(filepath.Join(root, spec.ID, residentTranscriptName))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || records[1].Type != residentRecordAssistant || len(records[1].Message) == 0 || records[2].Type != residentRecordToolCall || records[2].ToolName != "bash" {
		t.Fatalf("records = %#v", records)
	}
}

func TestReconcileResidentJournalInterruptsStartedTurnWithoutReplay(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "child-2")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "child-2", SessionID: "child-session-2", InitialTurnID: "initial-turn", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "do work"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnStarted(spec, spec.InitialTurnID); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	metadata, err := ReconcileResidentJournal(filepath.Join(root, "child-2"))
	if err != nil {
		t.Fatalf("ReconcileResidentJournal: %v", err)
	}
	if metadata.State != ResidentInterrupted {
		t.Fatalf("state = %q, want interrupted", metadata.State)
	}
	records, err := ReadResidentJournal(filepath.Join(root, "child-2", residentTranscriptName))
	if err != nil {
		t.Fatal(err)
	}
	if got := records[len(records)-1].Type; got != residentRecordInterrupted {
		t.Fatalf("last record = %q, want interruption", got)
	}
	if _, err := ReconcileResidentJournal(filepath.Join(root, "child-2")); err != nil {
		t.Fatalf("repeat reconciliation: %v", err)
	}
	repeated, err := ReadResidentJournal(filepath.Join(root, "child-2", residentTranscriptName))
	if err != nil {
		t.Fatal(err)
	}
	if len(repeated) != len(records) {
		t.Fatalf("repeat reconciliation appended records: %d -> %d", len(records), len(repeated))
	}
}

func TestReconcileResidentJournalInterruptsQueuedInitialTurnOnce(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "queued-child")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "queued-child", SessionID: "queued-session", InitialTurnID: "initial-turn", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "do work"); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		metadata, err := ReconcileResidentJournal(filepath.Join(root, spec.ID))
		if err != nil || metadata.State != ResidentInterrupted {
			t.Fatalf("reconcile %d = (%#v, %v)", attempt, metadata, err)
		}
	}
	records, err := ReadResidentJournal(filepath.Join(root, spec.ID, residentTranscriptName))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[1].Type != residentRecordInterrupted || records[1].TurnID != spec.InitialTurnID {
		t.Fatalf("queued initial records = %#v", records)
	}
}

func TestReconcileResidentJournalRepairsOnlyDanglingToolCalls(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "tool-pair")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "tool-pair", SessionID: "child-session", InitialTurnID: "turn-1", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnStarted(spec, "turn-1"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvToolCall{ID: "call-1", Name: "bash", Args: json.RawMessage(`{"command":"pwd"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileResidentJournal(filepath.Join(root, spec.ID)); err != nil {
		t.Fatal(err)
	}
	records, err := ReadResidentJournal(filepath.Join(root, spec.ID, residentTranscriptName))
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Type == residentRecordToolResult && record.ToolID == "call-1" {
			return
		}
	}
	t.Fatalf("no repaired tool result in %#v", records)
}

func TestReconcileResidentJournalRepairsTerminalDanglingCallWithoutInterruptingChild(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "terminal-tool-pair")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "terminal-tool-pair", SessionID: "child-session", InitialTurnID: "turn-1", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnStarted(spec, "turn-1"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvToolCall{ID: "call-1", Name: "bash", Args: json.RawMessage(`{"command":"pwd"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnFinished(spec, "turn-1", nil); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	metadata, err := ReconcileResidentJournal(filepath.Join(root, spec.ID))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.State != ResidentIdle {
		t.Fatalf("state = %q, want idle", metadata.State)
	}
	records, err := ReadResidentJournal(filepath.Join(root, spec.ID, residentTranscriptName))
	if err != nil {
		t.Fatal(err)
	}
	if got := records[len(records)-1]; got.Type != residentRecordToolResult || got.ToolID != "call-1" {
		t.Fatalf("last record = %#v, want repaired tool result", got)
	}
}

func TestReconcileResidentJournalRebuildsMissingTerminalResult(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "terminal-result")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "terminal-result", SessionID: "child-session", InitialTurnID: "turn-1", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnStarted(spec, "turn-1"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "recovered final answer"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnFinished(spec, "turn-1", nil); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(root, spec.ID, residentResultName)
	if err := os.WriteFile(resultPath, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileResidentJournal(filepath.Join(root, spec.ID)); err != nil {
		t.Fatal(err)
	}
	result, err := ReadResidentResult(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.TurnID != "turn-1" || result.State != ResidentIdle || result.Summary != "recovered final answer" {
		t.Fatalf("result = %#v", result)
	}
}

func TestReconcileResidentJournalPreservesTerminalLifecycleTime(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "terminal-time")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "terminal-time", SessionID: "child-session", InitialTurnID: "turn-1", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnStarted(spec, "turn-1"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnFinished(spec, "turn-1", nil); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	records, err := ReadResidentJournal(filepath.Join(root, spec.ID, residentTranscriptName))
	if err != nil {
		t.Fatal(err)
	}
	want := records[len(records)-1].Time
	metadata, err := ReconcileResidentJournal(filepath.Join(root, spec.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !metadata.UpdatedAt.Equal(want) {
		t.Fatalf("updated_at = %s, want terminal record time %s", metadata.UpdatedAt, want)
	}
}

func TestResidentJournalStoresBoundedFinalAssistantSummary(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "final-summary")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	spec := ResidentChildSpec{ID: "final-summary", SessionID: "child-session", InitialTurnID: "turn-1", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnStarted(spec, "turn-1"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "the child final answer"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnFinished(spec, "turn-1", nil); err != nil {
		t.Fatal(err)
	}
	result, err := ReadResidentResult(filepath.Join(root, spec.ID, residentResultName))
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != "the child final answer" {
		t.Fatalf("result summary = %q", result.Summary)
	}
}

func TestTruncateResidentResultSummaryPreservesUTF8Boundary(t *testing.T) {
	if residentResultSummaryBytes != 256<<10 {
		t.Fatalf("summary limit = %d, want %d", residentResultSummaryBytes, 256<<10)
	}
	text := strings.Repeat("é", residentResultSummaryBytes)
	summary := truncateResidentResultSummary(text)
	if !utf8.ValidString(summary) || !strings.HasSuffix(summary, "…") || len(summary) > residentResultSummaryBytes {
		t.Fatalf("summary = %q", summary[:min(len(summary), 32)])
	}
}

func TestReconcileResidentJournalRepairsLegacyFalseRecovery(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "legacy-race")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "legacy-race", SessionID: "child-session", InitialTurnID: "turn-1", Provider: "openai", Model: "gpt-5"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnStarted(spec, spec.InitialTurnID); err != nil {
		t.Fatal(err)
	}
	if err := journal.appendSync(residentRecord{Version: residentJournalVersion, Type: residentRecordToolCall, Time: time.Now().UTC(), ToolID: "call-1", ToolName: "bash", ToolArgs: json.RawMessage(`{"command":"pwd"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := journal.appendSync(residentRecord{Version: residentJournalVersion, Type: residentRecordToolResult, Time: time.Now().UTC(), ToolID: "call-1", ToolResult: json.RawMessage(`{"Content":[{"text":"tool interrupted by resident host restart"}],"IsError":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnInterrupted(spec, spec.InitialTurnID); err != nil {
		t.Fatal(err)
	}
	if err := journal.appendSync(residentRecord{Version: residentJournalVersion, Type: residentRecordToolResult, Time: time.Now().UTC(), ToolID: "call-1", ToolResult: json.RawMessage(`{"Content":[{"text":"ok"}],"IsError":false}`)}); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnFinished(spec, spec.InitialTurnID, nil); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	metadata, err := ReconcileResidentJournal(filepath.Join(root, spec.ID))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.State != ResidentIdle {
		t.Fatalf("state = %q, want idle", metadata.State)
	}
	records, err := ReadResidentJournal(filepath.Join(root, spec.ID, residentTranscriptName))
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Type == residentRecordInterrupted || isSyntheticInterruption(record.ToolResult) {
			t.Fatalf("repaired records retained false recovery: %#v", record)
		}
	}
	backups, err := filepath.Glob(filepath.Join(root, spec.ID, ".transcript-backup-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want one", backups)
	}
}

func TestReconcileResidentJournalTranslatesV2BudgetHistory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome string
		want    ResidentState
	}{
		{name: "exhausted stop is failure", outcome: residentV2OutcomeBudgetExhausted, want: ResidentFailed},
		{name: "completed before check is success", outcome: residentV2OutcomeCompletedBudgetExhausted, want: ResidentCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			journal, err := OpenResidentJournal(root, "v2-child")
			if err != nil {
				t.Fatal(err)
			}
			spec := ResidentChildSpec{ID: "v2-child", SessionID: "child-session", InitialTurnID: "turn-1", Provider: "openai", Model: "test", Required: true}
			if err := journal.Accept(spec, "task"); err != nil {
				t.Fatal(err)
			}
			// A historical recovery follow-up: the v2 host accepted it
			// with a budget baseline on the turn.accepted record, then
			// ran and finished it with the historical outcome.
			if err := journal.AcceptFollowUp(spec, "turn-2", "continue"); err != nil {
				t.Fatal(err)
			}
			if err := journal.RecordTurnStarted(spec, "turn-2"); err != nil {
				t.Fatal(err)
			}
			if err := journal.appendSync(residentRecord{Version: residentJournalVersion, Type: residentRecordTurnFinished, Time: time.Now().UTC(), TurnID: "turn-2", Outcome: tc.outcome}); err != nil {
				t.Fatal(err)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			downgradeJournalVersion(t, filepath.Join(root, spec.ID, residentTranscriptName))
			transcript, err := os.ReadFile(filepath.Join(root, spec.ID, residentTranscriptName))
			if err != nil {
				t.Fatal(err)
			}
			factory := func(ResidentChildSpec, *ResidentJournal) (ResidentTurnRunner, error) {
				return func(context.Context, string) error { return nil }, nil
			}
			manager := NewResidentManager(root, factory)
			t.Cleanup(func() { _ = manager.Close(context.Background()) })
			completions := make(chan ResidentCompletion, 1)
			manager.SetCompletionObserver(func(c ResidentCompletion) { completions <- c })
			if errs := manager.Reconcile(); len(errs) != 0 {
				t.Fatal(errs)
			}
			snapshot, ok := manager.SnapshotFor(spec.ID)
			if !ok || snapshot.State != tc.want {
				t.Fatalf("reconciled snapshot = %#v, want state %q", snapshot, tc.want)
			}
			wantUnmet := 0
			if tc.want == ResidentFailed {
				wantUnmet = 1
			}
			if unmet := manager.UnmetRequired(); len(unmet) != wantUnmet {
				t.Fatalf("unmet required = %#v, want %d", unmet, wantUnmet)
			}
			// Translation rebuilds projections but never rewrites the
			// authoritative transcript.
			after, err := os.ReadFile(filepath.Join(root, spec.ID, residentTranscriptName))
			if err != nil || string(after) != string(transcript) {
				t.Fatalf("reconciliation modified transcript: %v", err)
			}
			// An explicit follow-up resolves the history: success
			// satisfies required work for the exhausted case too.
			if err := manager.Resume(t.Context(), spec.ID, "finish it"); err != nil {
				t.Fatal(err)
			}
			select {
			case completion := <-completions:
				if completion.Err != nil {
					t.Fatal(completion.Err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no terminal notification")
			}
			if unmet := manager.UnmetRequired(); len(unmet) != 0 {
				t.Fatalf("successful resume remains unmet: %#v", unmet)
			}
			// A second reconciliation replays nothing.
			if errs := manager.Reconcile(); len(errs) != 0 {
				t.Fatal(errs)
			}
			snapshot, ok = manager.SnapshotFor(spec.ID)
			if !ok || snapshot.State != ResidentIdle {
				t.Fatalf("post-resume snapshot = %#v, want idle", snapshot)
			}
		})
	}
}

func TestReconcileResidentJournalRejectsUnknownTurnOutcome(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "bogus-child")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "bogus-child", SessionID: "child-session", InitialTurnID: "turn-1", Provider: "openai", Model: "test"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnStarted(spec, spec.InitialTurnID); err != nil {
		t.Fatal(err)
	}
	if err := journal.appendSync(residentRecord{Version: residentJournalVersion, Type: residentRecordTurnFinished, Time: time.Now().UTC(), TurnID: spec.InitialTurnID, Outcome: "bogus"}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileResidentJournal(filepath.Join(root, spec.ID)); err == nil {
		t.Fatal("unknown turn outcome reconciled without error")
	}
}

func TestReadResidentResultTranslatesArchivedV2BudgetExhaustion(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "v2-result")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "v2-result", SessionID: "child-session", InitialTurnID: "turn-1", Provider: "openai", Model: "test"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnStarted(spec, spec.InitialTurnID); err != nil {
		t.Fatal(err)
	}
	if err := journal.appendSync(residentRecord{Version: residentJournalVersion, Type: residentRecordTurnFinished, Time: time.Now().UTC(), TurnID: spec.InitialTurnID, Outcome: residentV2OutcomeBudgetExhausted}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	downgradeJournalVersion(t, filepath.Join(root, spec.ID, residentTranscriptName))
	// An archived v2 result.json carries the historical state, handoff,
	// and error code. The reader translates the state to failure while
	// keeping the archived bytes readable as provenance.
	archived, err := json.Marshal(ResidentResult{Version: residentJournalVersionV2, ID: spec.ID, TurnID: spec.InitialTurnID, State: ResidentState(residentV2OutcomeBudgetExhausted), Summary: "partial", Handoff: "archived handoff", ErrorCode: residentV2ErrorBudgetExhausted})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, spec.ID, residentResultName), archived, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ReadResidentResult(filepath.Join(root, spec.ID, residentResultName))
	if err != nil {
		t.Fatal(err)
	}
	if result.State != ResidentFailed || result.Handoff != "archived handoff" || result.ErrorCode != residentV2ErrorBudgetExhausted {
		t.Fatalf("translated result = %#v", result)
	}
}

func TestReconcileResidentJournalRejectsUnknownVersion(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "future-child")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "future-child", SessionID: "child-session", Provider: "openai", Model: "test"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(root, spec.ID, residentTranscriptName)
	data, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	future := strings.Replace(string(data), `"version":3`, `"version":99`, 1)
	if future == string(data) {
		t.Fatal("accepted record has no journal version")
	}
	if err := os.WriteFile(transcript, []byte(future), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileResidentJournal(filepath.Join(root, spec.ID)); err == nil {
		t.Fatal("unknown journal version reconciled without error")
	}
}

func TestReconcileResidentJournalRejectsUnknownLaterRecordVersion(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "tampered-child")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "tampered-child", SessionID: "child-session", InitialTurnID: "turn-1", Provider: "openai", Model: "test"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnStarted(spec, spec.InitialTurnID); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnFinished(spec, spec.InitialTurnID, nil); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	// Tamper only a later record: the accepted prefix stays v3, so only
	// per-record validation can catch this corruption.
	transcript := filepath.Join(root, spec.ID, residentTranscriptName)
	data, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("transcript lines = %d, want 3", len(lines))
	}
	lines[2] = strings.Replace(lines[2], `"version":3`, `"version":99`, 1)
	if !strings.Contains(lines[2], `"version":99`) {
		t.Fatal("finished record has no journal version")
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileResidentJournal(filepath.Join(root, spec.ID)); !errors.Is(err, ErrIncompatibleResidentJournal) {
		t.Fatalf("tampered record reconciled: %v, want incompatible version", err)
	}
}

func TestReconcileResidentJournalAcceptsMixedV2V3ResumedJournal(t *testing.T) {
	root := t.TempDir()
	journal, err := OpenResidentJournal(root, "mixed-child")
	if err != nil {
		t.Fatal(err)
	}
	spec := ResidentChildSpec{ID: "mixed-child", SessionID: "child-session", InitialTurnID: "turn-1", Provider: "openai", Model: "test"}
	if err := journal.Accept(spec, "task"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnStarted(spec, spec.InitialTurnID); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordTurnFinished(spec, spec.InitialTurnID, nil); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	// A resumed v2 journal keeps its v2 prefix on disk while the new host
	// appends v3 records. Downgrade only the accepted record to model it.
	transcript := filepath.Join(root, spec.ID, residentTranscriptName)
	data, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	mixed := strings.Replace(string(data), `"version":3`, `"version":2`, 1)
	if mixed == string(data) {
		t.Fatal("accepted record has no journal version")
	}
	if err := os.WriteFile(transcript, []byte(mixed), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, err := ReconcileResidentJournal(filepath.Join(root, spec.ID))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.State != ResidentIdle {
		t.Fatalf("mixed-version state = %q, want idle", metadata.State)
	}
}

// downgradeJournalVersion rewrites a freshly written v3 transcript into a
// faithful v2 archive by lowering every record version and restoring the
// historical budget bytes (baseline and spec fields) that v3 no longer
// writes. The v3 reader must ignore those bytes structurally.
func downgradeJournalVersion(t *testing.T, transcript string) {
	t.Helper()
	data, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	downgraded := strings.ReplaceAll(string(data), `"version":3`, `"version":2`)
	if downgraded == string(data) {
		t.Fatal("transcript has no journal version")
	}
	// Historical v2 bytes: a budget baseline on the follow-up turn.accepted
	// record (written by the v2 recovery acceptance) and budget fields on
	// the accepted spec. Neither exists in the v3 structs, so the reader
	// must drop them without error.
	downgraded = strings.Replace(downgraded, `"type":"turn.accepted"`, `"type":"turn.accepted","budget_baseline":100`, 1)
	downgraded = strings.Replace(downgraded, `"provider":"openai"`, `"provider":"openai","budget_limit":100,"budget_source":"model_context"`, 1)
	if err := os.WriteFile(transcript, []byte(downgraded), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRepairLegacyFalseRecoveryRefusesAmbiguousBlock(t *testing.T) {
	synthetic := json.RawMessage(`{"Content":[{"text":"tool interrupted by resident host restart"}],"IsError":true}`)
	real := json.RawMessage(`{"Content":[{"text":"ok"}],"IsError":false}`)
	records := []residentRecord{
		{Type: residentRecordTurnStarted, TurnID: "turn-1"},
		{Type: residentRecordToolResult, ToolID: "call-1", ToolResult: synthetic},
		{Type: residentRecordToolResult, ToolID: "call-2", ToolResult: synthetic},
		{Type: residentRecordInterrupted, TurnID: "turn-1"},
		{Type: residentRecordToolResult, ToolID: "call-1", ToolResult: real},
	}
	if repaired, ok := repairLegacyFalseRecovery(records); ok || repaired != nil {
		t.Fatalf("repairLegacyFalseRecovery = %#v, %v; want no repair", repaired, ok)
	}
}

func TestRepairLegacyFalseRecoveryPreservesOtherTurnInterruption(t *testing.T) {
	synthetic := json.RawMessage(`{"Content":[{"text":"tool interrupted by resident host restart"}],"IsError":true}`)
	real := json.RawMessage(`{"Content":[{"text":"ok"}],"IsError":false}`)
	records := []residentRecord{
		{Type: residentRecordTurnStarted, TurnID: "turn-1"},
		{Type: residentRecordToolResult, ToolID: "call-1", ToolResult: synthetic},
		{Type: residentRecordInterrupted, TurnID: "turn-1"},
		{Type: residentRecordInterrupted, TurnID: "turn-2"},
		{Type: residentRecordToolResult, ToolID: "call-1", ToolResult: real},
	}
	if repaired, ok := repairLegacyFalseRecovery(records); ok || repaired != nil {
		t.Fatalf("repairLegacyFalseRecovery = %#v, %v; want no repair", repaired, ok)
	}
}

func TestReconcileResidentJournalRejectsConflictingToolPairs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		records []residentRecord
		want    string
	}{
		{
			name:    "orphan-result",
			records: []residentRecord{{Version: residentJournalVersion, Type: residentRecordToolResult, ToolID: "missing", ToolResult: json.RawMessage(`{"is_error":true}`)}},
			want:    "orphan tool result",
		},
		{
			name: "duplicate-result",
			records: []residentRecord{
				{Version: residentJournalVersion, Type: residentRecordToolCall, ToolID: "call-1", ToolName: "bash", ToolArgs: json.RawMessage(`{"command":"pwd"}`)},
				{Version: residentJournalVersion, Type: residentRecordToolResult, ToolID: "call-1", ToolResult: json.RawMessage(`{"is_error":false}`)},
				{Version: residentJournalVersion, Type: residentRecordToolResult, ToolID: "call-1", ToolResult: json.RawMessage(`{"is_error":false}`)},
			},
			want: "duplicate tool result",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			journal, err := OpenResidentJournal(root, "corrupt")
			if err != nil {
				t.Fatal(err)
			}
			spec := ResidentChildSpec{ID: "corrupt", SessionID: "child-session", Provider: "openai", Model: "gpt-5"}
			if err := journal.Accept(spec, "task"); err != nil {
				t.Fatal(err)
			}
			for _, record := range tc.records {
				record.Time = time.Now().UTC()
				if err := journal.appendSync(record); err != nil {
					t.Fatal(err)
				}
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := ReconcileResidentJournal(filepath.Join(root, spec.ID)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ReconcileResidentJournal error = %v, want %q", err, tc.want)
			}
		})
	}
}
