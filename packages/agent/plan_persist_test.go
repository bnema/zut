package agent

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func newPlanPersistSession(t *testing.T) *core.Session {
	t.Helper()
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "provider", "model", "version")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// planMutationResult builds the result shape core produces for a validated
// plan mutation: materialized details plus the pending operation.
func planMutationResult(plan []core.PlanStep) core.ToolResult {
	op := core.PlanOperation{Action: "set"}
	return core.ToolResult{
		Content:       []provider.Content{provider.TextBlock{Text: "Plan updated"}},
		Details:       core.PlanUpdate{Plan: plan},
		PlanOperation: &op,
	}
}

func TestPersistPlanToolResultWritesMetaRow(t *testing.T) {
	sess := newPlanPersistSession(t)
	want := []core.PlanStep{
		{Step: "one", Status: core.PlanPending},
		{Step: "two", Status: core.PlanInProgress},
	}
	if err := persistToolResultState(nil, sess, planMutationResult(want)); err != nil {
		t.Fatal(err)
	}
	if sess.Meta.Plan == nil || !slices.Equal(sess.Meta.Plan.Steps, want) {
		t.Fatalf("plan = %#v, want %#v", sess.Meta.Plan, want)
	}
}

// TestPersistPlanToolResultSkipsShowAndErrors covers the two results that must
// not append a meta row: show (no materialized state) and any error.
func TestPersistPlanToolResultSkipsShowAndErrors(t *testing.T) {
	errorResult := planMutationResult([]core.PlanStep{{Step: "one", Status: core.PlanPending}})
	errorResult.IsError = true
	tests := []struct {
		name   string
		result core.ToolResult
	}{
		{name: "show", result: core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "Plan is empty"}}}},
		{name: "error", result: errorResult},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sess := newPlanPersistSession(t)
			if err := persistToolResultState(nil, sess, test.result); err != nil {
				t.Fatal(err)
			}
			if sess.Meta.Plan != nil {
				t.Fatalf("plan = %#v, want nil", sess.Meta.Plan)
			}
			raw, err := os.ReadFile(sess.Path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), `"plan"`) {
				t.Fatalf("session file gained a plan meta row: %s", raw)
			}
		})
	}
}

func TestPersistPlanToolResultWriteFailureSurfacesSafeError(t *testing.T) {
	sess := newPlanPersistSession(t)
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	err := persistToolResultState(nil, sess, planMutationResult([]core.PlanStep{{Step: "one", Status: core.PlanPending}}))
	var safe *core.ToolResultCommitError
	if !errors.As(err, &safe) {
		t.Fatalf("persist error = %T %v, want safe tool result commit error", err, err)
	}
	if safe.Message != "plan state could not be saved" {
		t.Fatalf("safe message = %q", safe.Message)
	}
}

func TestPersistPlanToolResultWithoutSessionSurfacesSafeError(t *testing.T) {
	err := persistToolResultState(nil, nil, planMutationResult([]core.PlanStep{{Step: "one", Status: core.PlanPending}}))
	var safe *core.ToolResultCommitError
	if !errors.As(err, &safe) {
		t.Fatalf("persist error = %T %v, want safe tool result commit error", err, err)
	}
	if safe.Message != "plan state unavailable: session persistence is disabled" {
		t.Fatalf("safe message = %q", safe.Message)
	}
}
