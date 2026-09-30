package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// A waited turn still reaches the host, but its report must not also be
// repeated in the tool result. Without a host, the tool remains self-contained.
func TestResidentWaitResultDelivery(t *testing.T) {
	for _, action := range []string{"spawn", "resume"} {
		for _, host := range []bool{false, true} {
			for _, turnErr := range []error{nil, errors.New("synthetic failure"), context.Canceled} {
				t.Run(fmt.Sprintf("%s/host=%t/%s", action, host, completionStatus(turnErr)), func(t *testing.T) {
					const summary = "synthetic worker report"
					manager := subagents.NewResidentManager(t.TempDir(), func(_ subagents.ResidentChildSpec, journal *subagents.ResidentJournal) (subagents.ResidentRuntime, error) {
						return subagents.ResidentTurnRunner(func(_ context.Context, prompt string) error {
							if prompt == "initial" {
								return nil
							}
							if err := journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: summary}}}}); err != nil {
								return err
							}
							return turnErr
						}), nil
					})
					t.Cleanup(func() { _ = manager.Close(context.Background()) })
					spec := subagents.ResidentChildSpec{ID: "delivery-child", InitialTurnID: "initial", SessionID: "child-session", Provider: "openai", Model: "test"}
					if action == "resume" {
						done, cancel := manager.WatchCompletion(spec.ID, spec.InitialTurnID)
						defer cancel()
						if _, err := manager.Spawn(t.Context(), spec, "initial"); err != nil {
							t.Fatal(err)
						}
						select {
						case <-done:
						case <-time.After(5 * time.Second):
							t.Fatal("initial turn did not finish")
						}
					}
					updates := make(chan subagents.ResidentCompletion, 2)
					if host {
						manager.SetCompletionObserver(func(c subagents.ResidentCompletion) { updates <- c })
					}
					facade := &SubagentTool{
						Spawn:  &SubagentSpawnTool{ResidentManager: manager, Enabled: func() bool { return true }, BuildResidentSpec: func(context.Context, ResidentSpawnRequest) (subagents.ResidentChildSpec, error) { return spec, nil }},
						Resume: &SubagentResumeTool{ResidentManager: manager, Enabled: func() bool { return true }},
					}
					args := json.RawMessage(`{"action":"spawn","task":"report","wait":5}`)
					if action == "resume" {
						args = json.RawMessage(`{"action":"resume","agent_id":"delivery-child","prompt":"report","mode":"queue","wait":5}`)
					}
					result, err := facade.Execute(t.Context(), args, nil)
					if err != nil || result.IsError {
						t.Fatalf("Execute = (%#v, %v)", result, err)
					}
					text := toolResultText(t, result)
					if strings.Contains(text, summary) == host {
						t.Fatalf("host=%t, tool report = %q", host, text)
					}
					if !strings.Contains(text, completionStatus(turnErr)) {
						t.Fatalf("missing terminal status in %q", text)
					}
					if host {
						if !strings.Contains(text, "host_update") {
							t.Fatalf("missing delivery indication in %q", text)
						}
						select {
						case c := <-updates:
							if c.Summary != summary || !errors.Is(c.Err, turnErr) {
								t.Fatalf("host report = %#v", c)
							}
						case <-time.After(5 * time.Second):
							t.Fatal("host lost the report")
						}
						select {
						case c := <-updates:
							t.Fatalf("duplicate host report: %#v", c)
						default:
						}
					}
				})
			}
		}
	}
}

func completionStatus(err error) string {
	if errors.Is(err, context.Canceled) {
		return "interrupted"
	}
	if err != nil {
		return "failed"
	}
	return "completed"
}

// Abandoning a bounded wait never removes the host's completion route.
func TestResidentAbandonedWaitPreservesHostReport(t *testing.T) {
	for _, action := range []string{"spawn", "resume"} {
		for _, cancelWait := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancel=%t", action, cancelWait), func(t *testing.T) {
				started := make(chan struct{}, 1)
				release := make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				manager := subagents.NewResidentManager(t.TempDir(), func(_ subagents.ResidentChildSpec, journal *subagents.ResidentJournal) (subagents.ResidentRuntime, error) {
					return subagents.ResidentTurnRunner(func(ctx context.Context, prompt string) error {
						if prompt == "initial" {
							return nil
						}
						started <- struct{}{}
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}
						return journal.RecordAgentEvent(core.EvAssistantMessage{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "late report"}}}})
					}), nil
				})
				t.Cleanup(func() { unblock(); _ = manager.Close(context.Background()) })
				spec := subagents.ResidentChildSpec{ID: "late-child", InitialTurnID: "initial", SessionID: "session", Provider: "openai", Model: "test"}
				if action == "resume" {
					done, cancel := manager.WatchCompletion(spec.ID, spec.InitialTurnID)
					defer cancel()
					if _, err := manager.Spawn(t.Context(), spec, "initial"); err != nil {
						t.Fatal(err)
					}
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Fatal("initial turn did not finish")
					}
				}
				updates := make(chan subagents.ResidentCompletion, 2)
				manager.SetCompletionObserver(func(c subagents.ResidentCompletion) { updates <- c })
				facade := &SubagentTool{
					Spawn:  &SubagentSpawnTool{ResidentManager: manager, Enabled: func() bool { return true }, BuildResidentSpec: func(context.Context, ResidentSpawnRequest) (subagents.ResidentChildSpec, error) { return spec, nil }},
					Resume: &SubagentResumeTool{ResidentManager: manager, Enabled: func() bool { return true }},
				}
				seconds := 1
				if cancelWait {
					seconds = 300
				}
				args := fmt.Sprintf(`{"action":"spawn","task":"report","wait":%d}`, seconds)
				if action == "resume" {
					args = fmt.Sprintf(`{"action":"resume","agent_id":"late-child","prompt":"report","mode":"queue","wait":%d}`, seconds)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				type toolOutcome struct {
					result core.ToolResult
					err    error
				}
				done := make(chan toolOutcome, 1)
				go func() {
					result, err := facade.Execute(ctx, json.RawMessage(args), nil)
					done <- toolOutcome{result, err}
				}()
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("turn did not start")
				}
				if cancelWait {
					cancel()
				}
				select {
				case outcome := <-done:
					if cancelWait {
						if !errors.Is(outcome.err, context.Canceled) {
							t.Fatalf("wait error = %v", outcome.err)
						}
					} else {
						if outcome.err != nil || outcome.result.IsError {
							t.Fatalf("wait outcome = %#v", outcome)
						}
						if action == "resume" {
							response, ok := outcome.result.Details.(subagentResumeResponse)
							if !ok || response.Wait == nil || !response.Wait.TimedOut {
								t.Fatalf("wait outcome = %#v", outcome)
							}
						} else if !strings.Contains(toolResultText(t, outcome.result), "timed out") {
							t.Fatalf("wait outcome = %#v", outcome)
						}
					}
				case <-time.After(5 * time.Second):
					t.Fatal("wait did not return")
				}
				unblock()
				select {
				case c := <-updates:
					if c.Err != nil || c.Summary != "late report" {
						t.Fatalf("late completion = %#v", c)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("late host report was lost")
				}
			})
		}
	}
}

func TestSubagentGuidanceRejectsResumeAsWait(t *testing.T) {
	facade := &SubagentTool{}
	for _, text := range []string{facade.Description(), string(facade.Schema())} {
		if !strings.Contains(text, "Do not use resume solely to wait") {
			t.Fatalf("missing resume-as-wait warning: %s", text)
		}
	}
}
