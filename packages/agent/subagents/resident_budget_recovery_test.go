package subagents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestResidentBudgetExhaustionHandoffAndRecovery(t *testing.T) {
	for _, mode := range []string{"read-only", "shared", "worktree"} {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/restart=%t", mode, restart), func(t *testing.T) {
				root, workspace := t.TempDir(), t.TempDir()
				if mode == "worktree" {
					workspace = initTestRepo(t)
				}
				spec := ResidentChildSpec{ID: "budget-recovery", SessionID: "session", InitialTurnID: "initial", Provider: "openai", Model: "test", Required: true, BudgetLimit: 100, BudgetSource: "model_context", RepositoryRoot: workspace, WorkspaceMode: WorkspaceShared}
				if mode == "worktree" {
					spec.WorkspaceMode, spec.WorkspaceCapture = WorkspaceWorktree, CapturePatch
				}
				factory := func(spec ResidentChildSpec, journal *ResidentJournal) (ResidentTurnRunner, error) {
					return func(_ context.Context, prompt string) error {
						if prompt == "continue remaining work" {
							if journal.BudgetBaseline() != 100 {
								return fmt.Errorf("recovery baseline = %d", journal.BudgetBaseline())
							}
							messages, err := ReadResidentTranscriptMessages(journal.Dir())
							if err != nil || len(messages) < 3 {
								return fmt.Errorf("missing restored progress: %d messages, %v", len(messages), err)
							}
							if mode != "read-only" {
								data, err := os.ReadFile(filepath.Join(spec.Workspace, "README.md"))
								if err != nil || string(data) != "partial artifact\n" {
									return fmt.Errorf("lost artifact: %q, %v", data, err)
								}
							}
							if err := journal.RecordAgentEvent(core.EvUsage{Cumulative: provider.Usage{InputTokens: 105}}); err != nil {
								return err
							}
							return journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "verified remaining work"}}}})
						}
						tool, output := "read", "inspected README; verification unfinished"
						if mode != "read-only" {
							tool, output = "write", "wrote README.md; verification unfinished"
							if err := os.WriteFile(filepath.Join(spec.Workspace, "README.md"), []byte("partial artifact\n"), 0o600); err != nil {
								return err
							}
						}
						args := json.RawMessage(`{"path":"README.md"}`)
						for _, event := range []core.AgentEvent{
							core.EvUserMessage{Message: provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: prompt}}}},
							core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "working on README"}, provider.ToolCallBlock{ID: "call", Name: tool, Arguments: args}}}},
							core.EvToolCall{ID: "call", Name: tool, Args: args},
							core.EvToolResult{ID: "call", Result: core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: output}}}},
							core.EvUsage{Cumulative: provider.Usage{InputTokens: 100}},
						} {
							if err := journal.RecordAgentEvent(event); err != nil {
								return err
							}
						}
						return ErrBudgetExceeded
					}, nil
				}
				manager := NewResidentManager(root, factory)
				t.Cleanup(func() { _ = manager.Close(context.Background()) })
				completions := make(chan ResidentCompletion, 2)
				manager.SetCompletionObserver(func(c ResidentCompletion) { completions <- c })
				if _, err := manager.Spawn(t.Context(), spec, "inspect and verify README"); err != nil {
					t.Fatal(err)
				}
				completion := awaitBudgetCompletion(t, completions)
				if !errors.Is(completion.Err, ErrBudgetExceeded) || !strings.Contains(completion.Summary, "verification unfinished") {
					t.Fatalf("terminal notification = %#v", completion)
				}
				assertBudgetPartial := func() ResidentResult {
					t.Helper()
					snapshot, ok := manager.SnapshotFor(spec.ID)
					if !ok || snapshot.State != ResidentBudgetExhausted || snapshot.Budget.State != BudgetExceeded || len(manager.UnmetRequired()) != 1 {
						t.Fatalf("partial status = %#v; unmet = %#v", snapshot, manager.UnmetRequired())
					}
					result, err := manager.Result(spec.ID)
					if err != nil || result.ErrorCode != "budget_exhausted" || !strings.Contains(result.Handoff, "README.md") || len(result.Handoff) > residentHandoffBytes {
						t.Fatalf("saved partial result = %#v, %v", result, err)
					}
					return result
				}
				result := assertBudgetPartial()
				if mode == "worktree" {
					if result.PatchRef != PatchRef(spec.ID) || len(result.ChangedFiles) != 1 {
						t.Fatalf("missing partial patch: %#v", result)
					}
					patch, err := os.ReadFile(filepath.Join(root, spec.ID, residentPatchName))
					if err != nil || !strings.Contains(string(patch), "partial artifact") {
						t.Fatalf("partial patch = %q, %v", patch, err)
					}
				}
				if restart {
					if err := manager.Close(t.Context()); err != nil {
						t.Fatal(err)
					}
					manager = NewResidentManager(root, factory)
					manager.SetCompletionObserver(func(c ResidentCompletion) { completions <- c })
					if errs := manager.Reconcile(); len(errs) != 0 {
						t.Fatal(errs)
					}
					assertBudgetPartial()
				}
				if err := manager.Resume(t.Context(), spec.ID, "continue remaining work"); err != nil {
					t.Fatal(err)
				}
				if completion := awaitBudgetCompletion(t, completions); completion.Err != nil {
					t.Fatal(completion.Err)
				}
				if unmet := manager.UnmetRequired(); len(unmet) != 0 {
					t.Fatalf("successful recovery remains unmet: %#v", unmet)
				}
				snapshot, _ := manager.SnapshotFor(spec.ID)
				if snapshot.Budget.Used != 5 || snapshot.Budget.Limit != 100 || snapshot.Usage.InputTokens != 105 {
					t.Fatalf("recovery lost cumulative accounting: %#v", snapshot)
				}
				if err := manager.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				metadata, err := ReconcileResidentJournal(filepath.Join(root, spec.ID))
				if err != nil || metadata.BudgetBaseline != 100 || metadata.Usage.InputTokens != 105 {
					t.Fatalf("recovery baseline did not survive reconciliation: %#v, %v", metadata, err)
				}
			})
		}
	}
}

func awaitBudgetCompletion(t *testing.T, completions <-chan ResidentCompletion) ResidentCompletion {
	t.Helper()
	select {
	case completion := <-completions:
		return completion
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal budget notification")
		return ResidentCompletion{}
	}
}
