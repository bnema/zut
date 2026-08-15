package subagents

import (
	"encoding/json"
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
	if records[0].Spec == nil || records[0].Spec.SessionID != spec.SessionID || records[0].Prompt != "review this change" {
		t.Fatalf("accepted record = %#v", records[0])
	}

	meta, err := ReadResidentMetadata(filepath.Join(root, "child-1", residentMetadataName))
	if err != nil {
		t.Fatalf("ReadResidentMetadata: %v", err)
	}
	if meta.State != ResidentQueued || meta.SessionID != spec.SessionID {
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
