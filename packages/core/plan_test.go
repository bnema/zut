package core

import (
	"slices"
	"sync"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func planStep(text string, status PlanStepStatus) PlanStep {
	return PlanStep{Step: text, Status: status}
}

func TestAgentPlanCopySemantics(t *testing.T) {
	agent := &Agent{}
	original := []PlanStep{
		planStep("first", PlanPending),
		planStep("second", PlanInProgress),
	}
	agent.SetPlan(original)

	// Mutating the caller's slice after SetPlan must not affect state.
	original[0].Step = "mutated"
	if got := agent.CurrentPlan(); got[0].Step != "first" {
		t.Fatalf("SetPlan aliased caller slice: %#v", got)
	}

	// Mutating the slice returned by CurrentPlan must not affect state.
	snapshot := agent.CurrentPlan()
	snapshot[0].Step = "mutated again"
	snapshot[1].Status = PlanCompleted
	if got := agent.CurrentPlan(); got[0].Step != "first" || got[1].Status != PlanInProgress {
		t.Fatalf("CurrentPlan returned shared state: %#v", got)
	}

	agent.SetPlan(nil)
	if got := agent.CurrentPlan(); got != nil {
		t.Fatalf("SetPlan(nil) = %#v, want nil", got)
	}
}

func TestPreviewPlanOperationDoesNotMutate(t *testing.T) {
	agent := &Agent{}
	agent.SetPlan([]PlanStep{
		planStep("first", PlanInProgress),
		planStep("second", PlanPending),
	})
	before := agent.CurrentPlan()

	text := "renamed"
	operations := []PlanOperation{
		{Action: "set", Steps: []PlanStep{planStep("only", PlanPending)}},
		{Action: "add", Steps: []PlanStep{planStep("third", PlanPending)}},
		{Action: "update", Index: 2, Status: PlanCompleted},
		{Action: "update", Index: 1, Text: &text},
		{Action: "remove", Index: 1},
		{Action: "clear"},
		{Action: "show"},
	}
	for _, op := range operations {
		if _, err := agent.previewPlanOperation(op); err != nil {
			t.Fatalf("preview %q: %v", op.Action, err)
		}
		if got := agent.CurrentPlan(); !slices.Equal(got, before) {
			t.Fatalf("preview %q mutated state: %#v, want %#v", op.Action, got, before)
		}
	}
}

func TestPreviewPlanOperationErrors(t *testing.T) {
	base := []PlanStep{
		planStep("first", PlanInProgress),
		planStep("second", PlanPending),
		planStep("third", PlanPending),
	}
	blank := ""
	tests := []struct {
		name string
		op   PlanOperation
		want string
	}{
		{
			name: "missing action",
			op:   PlanOperation{},
			want: "plan: action is required",
		},
		{
			name: "unknown action",
			op:   PlanOperation{Action: "foo"},
			want: `plan: unknown action "foo"`,
		},
		{
			name: "update index out of range",
			op:   PlanOperation{Action: "update", Index: 4, Status: PlanCompleted},
			want: "plan: index 4 out of range (plan has 3 steps)",
		},
		{
			name: "update index zero",
			op:   PlanOperation{Action: "update", Index: 0, Status: PlanCompleted},
			want: "plan: index 0 out of range (plan has 3 steps)",
		},
		{
			name: "update without status or text",
			op:   PlanOperation{Action: "update", Index: 2},
			want: "plan: update requires status or step",
		},
		{
			name: "update with blank text",
			op:   PlanOperation{Action: "update", Index: 2, Text: &blank},
			want: "plan: step text is required",
		},
		{
			name: "remove index out of range",
			op:   PlanOperation{Action: "remove", Index: 9},
			want: "plan: index 9 out of range (plan has 3 steps)",
		},
		{
			name: "remove index negative",
			op:   PlanOperation{Action: "remove", Index: -1},
			want: "plan: index -1 out of range (plan has 3 steps)",
		},
		{
			name: "add without steps",
			op:   PlanOperation{Action: "add"},
			want: "plan: steps are required for add",
		},
		{
			name: "set blank step",
			op:   PlanOperation{Action: "set", Steps: []PlanStep{planStep("ok", PlanPending), planStep("  ", PlanPending)}},
			want: "plan: step text is required",
		},
		{
			name: "set without steps",
			op:   PlanOperation{Action: "set"},
			want: "plan: steps are required for set (use clear to remove every step)",
		},
		{
			name: "set two in progress",
			op:   PlanOperation{Action: "set", Steps: []PlanStep{planStep("a", PlanInProgress), planStep("b", PlanInProgress)}},
			want: "plan: at most one step can be in_progress",
		},
		{
			name: "add second in progress",
			op:   PlanOperation{Action: "add", Steps: []PlanStep{planStep("fourth", PlanInProgress)}},
			want: "plan: at most one step can be in_progress",
		},
		{
			name: "update second in progress",
			op:   PlanOperation{Action: "update", Index: 2, Status: PlanInProgress},
			want: "plan: at most one step can be in_progress",
		},
		{
			name: "set unknown status",
			op:   PlanOperation{Action: "set", Steps: []PlanStep{planStep("a", PlanStepStatus("done"))}},
			want: `plan: unknown status "done"`,
		},
		{
			name: "update unknown status",
			op:   PlanOperation{Action: "update", Index: 2, Status: PlanStepStatus("done")},
			want: `plan: unknown status "done"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &Agent{}
			agent.SetPlan(base)
			_, err := agent.previewPlanOperation(tt.op)
			if err == nil {
				t.Fatalf("preview succeeded, want error %q", tt.want)
			}
			if err.Error() != tt.want {
				t.Fatalf("error = %q, want %q", err.Error(), tt.want)
			}
			if got := agent.CurrentPlan(); !slices.Equal(got, base) {
				t.Fatalf("failed preview mutated state: %#v", got)
			}
		})
	}
}

// TestPreviewPlanOperationDefaultsMissingStatus covers the schema's optional step
// status. A step that omits it must materialize as pending, otherwise agent state
// would hold a status the session normalizer later drops on reload and the live
// plan would silently lose a step across a restart.
func TestPreviewPlanOperationDefaultsMissingStatus(t *testing.T) {
	tests := []struct {
		name string
		op   PlanOperation
		want []PlanStep
	}{
		{
			name: "set without status",
			op:   PlanOperation{Action: "set", Steps: []PlanStep{{Step: "a"}}},
			want: []PlanStep{planStep("a", PlanPending)},
		},
		{
			name: "add without status",
			op:   PlanOperation{Action: "add", Steps: []PlanStep{{Step: "first"}, {Step: "second"}}},
			want: []PlanStep{planStep("first", PlanPending), planStep("second", PlanPending)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &Agent{}
			update, err := agent.previewPlanOperation(tt.op)
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			if !slices.Equal(update.Plan, tt.want) {
				t.Fatalf("plan = %#v, want %#v", update.Plan, tt.want)
			}
			// The same steps must survive a persist/reload cycle unchanged.
			reloaded := normalizeSessionPlan(&SessionPlan{Steps: update.Plan})
			if reloaded == nil || !slices.Equal(reloaded.Steps, tt.want) {
				t.Fatalf("reloaded plan = %#v, want %#v", reloaded, tt.want)
			}
		})
	}
}

func TestPreviewThenCommitAppliesActions(t *testing.T) {
	text := "renamed"
	tests := []struct {
		name  string
		start []PlanStep
		op    PlanOperation
		want  []PlanStep
	}{
		{
			name:  "set replaces",
			start: []PlanStep{planStep("first", PlanPending)},
			op:    PlanOperation{Action: "set", Steps: []PlanStep{planStep("a", PlanPending), planStep("b", PlanCompleted)}},
			want:  []PlanStep{planStep("a", PlanPending), planStep("b", PlanCompleted)},
		},
		{
			name:  "clear removes every step",
			start: []PlanStep{planStep("first", PlanPending), planStep("second", PlanCompleted)},
			op:    PlanOperation{Action: "clear"},
			want:  nil,
		},
		{
			name:  "add appends",
			start: []PlanStep{planStep("first", PlanPending)},
			op:    PlanOperation{Action: "add", Steps: []PlanStep{planStep("second", PlanPending)}},
			want:  []PlanStep{planStep("first", PlanPending), planStep("second", PlanPending)},
		},
		{
			name:  "update status",
			start: []PlanStep{planStep("first", PlanPending), planStep("second", PlanPending)},
			op:    PlanOperation{Action: "update", Index: 2, Status: PlanCompleted},
			want:  []PlanStep{planStep("first", PlanPending), planStep("second", PlanCompleted)},
		},
		{
			name:  "update text",
			start: []PlanStep{planStep("first", PlanPending)},
			op:    PlanOperation{Action: "update", Index: 1, Text: &text},
			want:  []PlanStep{planStep("renamed", PlanPending)},
		},
		{
			name:  "remove",
			start: []PlanStep{planStep("first", PlanPending), planStep("second", PlanPending)},
			op:    PlanOperation{Action: "remove", Index: 1},
			want:  []PlanStep{planStep("second", PlanPending)},
		},
		{
			name:  "remove last",
			start: []PlanStep{planStep("first", PlanPending)},
			op:    PlanOperation{Action: "remove", Index: 1},
			want:  nil,
		},
		{
			name:  "clear",
			start: []PlanStep{planStep("first", PlanPending)},
			op:    PlanOperation{Action: "clear"},
			want:  nil,
		},
		{
			name:  "show is a no-op",
			start: []PlanStep{planStep("first", PlanPending)},
			op:    PlanOperation{Action: "show"},
			want:  []PlanStep{planStep("first", PlanPending)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &Agent{}
			agent.SetPlan(tt.start)
			update, err := agent.previewPlanOperation(tt.op)
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			// A failed preview means nothing is persisted and nothing is committed,
			// so state must still be the pre-operation plan.
			if got := agent.CurrentPlan(); !slices.Equal(got, tt.start) {
				t.Fatalf("preview mutated state: %#v", got)
			}
			agent.commitPlanUpdate(update)
			if got := agent.CurrentPlan(); !slices.Equal(got, tt.want) {
				t.Fatalf("plan = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestCommitPlanUpdateStoresPersistedValue covers the documented interleaving: a
// SetPlan between preview and commit must not win, because the persisted list is
// what the session file now records.
func TestCommitPlanUpdateStoresPersistedValue(t *testing.T) {
	agent := &Agent{}
	agent.SetPlan([]PlanStep{planStep("first", PlanPending)})
	update, err := agent.previewPlanOperation(PlanOperation{
		Action: "add",
		Steps:  []PlanStep{planStep("second", PlanPending)},
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}

	agent.SetPlan([]PlanStep{planStep("cleared by /clear", PlanPending)})
	agent.commitPlanUpdate(update)

	want := []PlanStep{planStep("first", PlanPending), planStep("second", PlanPending)}
	if got := agent.CurrentPlan(); !slices.Equal(got, want) {
		t.Fatalf("plan = %#v, want the persisted %#v", got, want)
	}
}

// TestAgentPlanStateIsConcurrencySafe exercises the plan accessors under -race.
func TestAgentPlanStateIsConcurrencySafe(t *testing.T) {
	agent := &Agent{}
	agent.SetPlan([]PlanStep{planStep("first", PlanPending)})

	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iter := 0; iter < 50; iter++ {
				agent.SetPlan([]PlanStep{planStep("first", PlanPending)})
				_ = agent.CurrentPlan()
				if update, err := agent.previewPlanOperation(PlanOperation{
					Action: "update",
					Index:  1,
					Status: PlanCompleted,
				}); err == nil {
					agent.commitPlanUpdate(update)
				}
			}
		}()
	}
	wg.Wait()
}

func TestSessionPlanMetaRoundTrip(t *testing.T) {
	root := t.TempDir()
	cwd := "/project/plan"
	sess, err := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	steps := []PlanStep{
		planStep("first", PlanCompleted),
		planStep("second", PlanInProgress),
	}
	if err := sess.UpdatePlan(&SessionPlan{Steps: steps}); err != nil {
		t.Fatalf("UpdatePlan: %v", err)
	}
	// Mutating the caller's slice after UpdatePlan must not affect session state.
	steps[0].Step = "mutated"
	if sess.Meta.Plan == nil || sess.Meta.Plan.Steps[0].Step != "first" {
		t.Fatalf("UpdatePlan aliased caller slice: %#v", sess.Meta.Plan)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	// A plan-only session must survive the fresh-file deletion guard.
	reopened, messages, err := OpenSession(sess.Path)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer reopened.Close()
	if len(messages) != 0 {
		t.Fatalf("want no messages, got %d", len(messages))
	}
	if reopened.Meta.Plan == nil {
		t.Fatal("plan not restored")
	}
	want := []PlanStep{planStep("first", PlanCompleted), planStep("second", PlanInProgress)}
	if !slices.Equal(reopened.Meta.Plan.Steps, want) {
		t.Fatalf("plan = %#v, want %#v", reopened.Meta.Plan.Steps, want)
	}
}

func TestSessionUpdatePlanNilClears(t *testing.T) {
	root := t.TempDir()
	cwd := "/project/plan"
	sess, err := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	// Keep the session file after Close: a fresh plan-only session whose plan is
	// cleared is deleted by the fresh-file guard, which this test is not about.
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "clear my plan"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sess.UpdatePlan(&SessionPlan{Steps: []PlanStep{planStep("first", PlanPending)}}); err != nil {
		t.Fatal(err)
	}
	if err := sess.UpdatePlan(nil); err != nil {
		t.Fatal(err)
	}
	if sess.Meta.Plan != nil {
		t.Fatalf("in-memory plan = %#v, want nil", sess.Meta.Plan)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := OpenSession(sess.Path)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer reopened.Close()
	if reopened.Meta.Plan != nil {
		t.Fatalf("persisted plan = %#v, want nil", reopened.Meta.Plan)
	}
}

func TestSessionPlanNormalizesOnLoad(t *testing.T) {
	root := t.TempDir()
	cwd := "/project/plan"
	sess, err := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "normalize me"}},
	}); err != nil {
		t.Fatal(err)
	}
	// UpdatePlan writes verbatim; the load path is responsible for normalization.
	raw := &SessionPlan{Steps: []PlanStep{
		planStep("", PlanPending),
		planStep("unknown", "sleeping"),
		planStep("first", PlanInProgress),
		planStep("second", PlanInProgress),
		planStep("third", PlanCompleted),
	}}
	if err := sess.UpdatePlan(raw); err != nil {
		t.Fatal(err)
	}
	_ = sess.Close()

	reopened, _, err := OpenSession(sess.Path)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer reopened.Close()
	want := []PlanStep{
		planStep("first", PlanInProgress),
		planStep("third", PlanCompleted),
	}
	if reopened.Meta.Plan == nil || !slices.Equal(reopened.Meta.Plan.Steps, want) {
		t.Fatalf("normalized plan = %#v, want %#v", reopened.Meta.Plan, want)
	}
}

func TestImportSessionPreservesPlan(t *testing.T) {
	root := t.TempDir()
	cwd := "/project/plan"
	sess, err := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "carry my plan"}},
	})
	want := []PlanStep{planStep("first", PlanCompleted), planStep("second", PlanInProgress)}
	if err := sess.UpdatePlan(&SessionPlan{Steps: want}); err != nil {
		t.Fatal(err)
	}
	_ = sess.Close()

	exportDir := t.TempDir()
	exportPath, err := ExportSession(sess.Path, exportDir)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	root2 := t.TempDir()
	importedPath, err := ImportSession(exportPath, root2, "/other/project", "0.0.0-test")
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	imported, _, err := OpenSession(importedPath)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer imported.Close()
	if imported.Meta.Plan == nil {
		t.Fatal("imported session dropped the plan")
	}
	if !slices.Equal(imported.Meta.Plan.Steps, want) {
		t.Fatalf("imported plan = %#v, want %#v", imported.Meta.Plan.Steps, want)
	}
}

func TestBranchSessionReconstructsPlanAtForkPoint(t *testing.T) {
	root := t.TempDir()
	cwd := "/project/plan"
	parent, err := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	appendPlanTranscript(t, parent)

	// The persisted plan deliberately differs from what the fork-point replay
	// should reconstruct, proving replay wins over the snapshot plan.
	if err := parent.UpdatePlan(&SessionPlan{Steps: []PlanStep{planStep("unrelated", PlanPending)}}); err != nil {
		t.Fatal(err)
	}
	_ = parent.Close()

	branchPath, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 5)
	if err != nil {
		t.Fatalf("BranchSession: %v", err)
	}
	branch, _, err := OpenSession(branchPath)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer branch.Close()
	want := []PlanStep{
		planStep("one", PlanInProgress),
		planStep("two", PlanPending),
		planStep("three", PlanPending),
	}
	if branch.Meta.Plan == nil {
		t.Fatal("branch has no plan")
	}
	if !slices.Equal(branch.Meta.Plan.Steps, want) {
		t.Fatalf("fork-point plan = %#v, want %#v", branch.Meta.Plan.Steps, want)
	}

	// Forking earlier reconstructs the plan as it stood then: only the legacy
	// update_plan call has been replayed.
	earlyPath, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 2)
	if err != nil {
		t.Fatalf("BranchSession(early): %v", err)
	}
	early, _, err := OpenSession(earlyPath)
	if err != nil {
		t.Fatalf("OpenSession(early): %v", err)
	}
	defer early.Close()
	earlyWant := []PlanStep{planStep("one", PlanInProgress), planStep("two", PlanPending)}
	if early.Meta.Plan == nil || !slices.Equal(early.Meta.Plan.Steps, earlyWant) {
		t.Fatalf("early plan = %#v, want %#v", early.Meta.Plan, earlyWant)
	}
}

// TestBranchSessionFullForkKeepsPersistedPlan covers the end-of-transcript fork
// rule: the parent's persisted plan is authoritative, including for a compacted
// parent whose plan calls are no longer in the transcript.
func TestBranchSessionFullForkKeepsPersistedPlan(t *testing.T) {
	root := t.TempDir()
	cwd := "/project/plan"
	parent, err := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	_ = parent.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "no plan calls here"}},
	})
	_ = parent.AppendMessage(provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: "a compacted parent"}},
	})
	want := []PlanStep{planStep("inherited", PlanInProgress)}
	if err := parent.UpdatePlan(&SessionPlan{Steps: want}); err != nil {
		t.Fatal(err)
	}
	_ = parent.Close()

	branchPath, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 2)
	if err != nil {
		t.Fatalf("BranchSession: %v", err)
	}
	branch, _, err := OpenSession(branchPath)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer branch.Close()
	if branch.Meta.Plan == nil || !slices.Equal(branch.Meta.Plan.Steps, want) {
		t.Fatalf("fallback plan = %#v, want %#v", branch.Meta.Plan, want)
	}
}

// TestBranchSessionNormalizesReplayedPlan guards the fork replay against a
// transcript that carries a step the live agent could never hold, such as a
// hand-edited or pre-validation session file.
func TestBranchSessionNormalizesReplayedPlan(t *testing.T) {
	root := t.TempDir()
	cwd := "/project/plan"
	parent, err := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	messages := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "start"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{
			ID:   "legacy-1",
			Name: legacyPlanToolName,
			Arguments: []byte(`{"plan":[
				{"step":"keep me","status":"pending"},
				{"step":"bogus status","status":"done"},
				{"step":"  ","status":"pending"}
			]}`),
		}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "legacy-1",
			Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}},
		}}},
		// Keeps the fork point short of the transcript end so the replay path runs
		// instead of the end-of-transcript rule.
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "more"}}},
	}
	for _, msg := range messages {
		if err := parent.AppendMessage(msg); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	_ = parent.Close()

	branchPath, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 2)
	if err != nil {
		t.Fatalf("BranchSession: %v", err)
	}
	branch, _, err := OpenSession(branchPath)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer branch.Close()

	want := []PlanStep{planStep("keep me", PlanPending)}
	if branch.Meta.Plan == nil || !slices.Equal(branch.Meta.Plan.Steps, want) {
		t.Fatalf("branch plan = %#v, want %#v", branch.Meta.Plan, want)
	}
}

// TestBranchSessionSkipsFailedPlanCalls guards the replay against calls that left
// a transcript row but changed nothing: a rejected, denied, or abandoned plan
// call must not shape the branch's plan.
func TestBranchSessionSkipsFailedPlanCalls(t *testing.T) {
	root := t.TempDir()
	cwd := "/project/plan"
	parent, err := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	messages := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "start"}}},
		// Applied: one in_progress step and one pending step.
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{
			ID:        "applied-1",
			Name:      legacyPlanToolName,
			Arguments: []byte(`{"plan":[{"step":"one","status":"in_progress"},{"step":"two","status":"pending"}]}`),
		}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "applied-1",
			Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}},
		}}},
		// Rejected by the tool: the result is an error, so nothing was applied.
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{
			ID:        "rejected-1",
			Name:      legacyPlanToolName,
			Arguments: []byte(`{"plan":[{"step":"never applied","status":"pending"}]}`),
		}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "rejected-1",
			IsError: true,
			Content: []provider.Content{provider.TextBlock{Text: "invalid arguments"}},
		}}},
		// Denied clear: also an error result.
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{
			ID:        "denied-1",
			Name:      planToolName,
			Arguments: []byte(`{"action":"clear"}`),
		}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "denied-1",
			IsError: true,
			Content: []provider.Content{provider.TextBlock{Text: "tool call refused by extension guard"}},
		}}},
		// Keeps the fork point short of the transcript end so the replay path runs.
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "more"}}},
	}
	for _, msg := range messages {
		if err := parent.AppendMessage(msg); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	_ = parent.Close()

	// Fork after the rejected call so the replay covers every call above.
	branchPath, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 7)
	if err != nil {
		t.Fatalf("BranchSession: %v", err)
	}
	branch, _, err := OpenSession(branchPath)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer branch.Close()

	want := []PlanStep{planStep("one", PlanInProgress), planStep("two", PlanPending)}
	if branch.Meta.Plan == nil || !slices.Equal(branch.Meta.Plan.Steps, want) {
		t.Fatalf("branch plan = %#v, want only the applied call %#v", branch.Meta.Plan, want)
	}
}

// TestBranchSessionPartialForkWithoutAppliedCallHasNoPlan covers the other half of
// the rule: a short fork whose only plan call failed inherits nothing, rather than
// reaching forward to the parent's later plan the way goals refuse to do.
func TestBranchSessionPartialForkWithoutAppliedCallHasNoPlan(t *testing.T) {
	root := t.TempDir()
	cwd := "/project/plan"
	parent, err := NewSession(root, cwd, "anthropic", "claude-opus-4-7", "0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	messages := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "start"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{
			ID:        "rejected-1",
			Name:      legacyPlanToolName,
			Arguments: []byte(`{"plan":[{"step":"never applied","status":"pending"}]}`),
		}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "rejected-1",
			IsError: true,
			Content: []provider.Content{provider.TextBlock{Text: "invalid arguments"}},
		}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "later work"}}},
	}
	for _, msg := range messages {
		if err := parent.AppendMessage(msg); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
	// The parent's own plan describes work after the fork point.
	if err := parent.UpdatePlan(&SessionPlan{Steps: []PlanStep{planStep("later", PlanPending)}}); err != nil {
		t.Fatal(err)
	}
	_ = parent.Close()

	branchPath, err := BranchSession(parent.Path, root, cwd, "0.0.0-test", 3)
	if err != nil {
		t.Fatalf("BranchSession: %v", err)
	}
	branch, _, err := OpenSession(branchPath)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer branch.Close()

	if branch.Meta.Plan != nil {
		t.Fatalf("branch plan = %#v, want none", branch.Meta.Plan)
	}
}

func TestNormalizeSessionPlan(t *testing.T) {
	tests := []struct {
		name string
		in   *SessionPlan
		want []PlanStep
	}{
		{
			name: "nil stays nil",
			in:   nil,
			want: nil,
		},
		{
			name: "nothing remains",
			in:   &SessionPlan{Steps: []PlanStep{planStep("", PlanPending), planStep("kept", "unknown")}},
			want: nil,
		},
		{
			name: "drops empty text and unknown status",
			in: &SessionPlan{Steps: []PlanStep{
				planStep("", PlanPending),
				planStep("unknown", "sleeping"),
				planStep("kept", PlanCompleted),
			}},
			want: []PlanStep{planStep("kept", PlanCompleted)},
		},
		{
			name: "keeps only first in progress",
			in: &SessionPlan{Steps: []PlanStep{
				planStep("first", PlanInProgress),
				planStep("second", PlanInProgress),
				planStep("third", PlanPending),
			}},
			want: []PlanStep{planStep("first", PlanInProgress), planStep("third", PlanPending)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeSessionPlan(tt.in)
			var gotSteps []PlanStep
			if got != nil {
				gotSteps = got.Steps
			}
			if !slices.Equal(gotSteps, tt.want) {
				t.Fatalf("plan = %#v, want %#v", gotSteps, tt.want)
			}
		})
	}

	long := &SessionPlan{}
	for i := 0; i < 50; i++ {
		long.Steps = append(long.Steps, planStep("step", PlanPending))
	}
	if got := normalizeSessionPlan(long); got == nil || len(got.Steps) != 50 {
		t.Fatalf("plan was capped: %d steps", len(got.Steps))
	}
}

func appendPlanTranscript(t *testing.T, sess *Session) {
	t.Helper()
	messages := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "start"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{
			ID:   "legacy-1",
			Name: legacyPlanToolName,
			Arguments: []byte(`{"explanation":"initial","plan":[
				{"step":"one","status":"in_progress"},
				{"step":"two","status":"pending"}
			]}`),
		}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "legacy-1",
			Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}},
		}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{
			ID:        "plan-2",
			Name:      planToolName,
			Arguments: []byte(`{"action":"add","steps":[{"step":"three","status":"pending"}]}`),
		}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "plan-2",
			Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}},
		}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "more"}}},
	}
	for _, msg := range messages {
		if err := sess.AppendMessage(msg); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}
}

// TestPlanUpdateSnapshotsRebuildsPerCallChecklists guards the load-time replay
// the host renders from: a transcript mixing the legacy update_plan full-list
// shape and the plan delta shape must yield the materialized checklist after
// each applied call, keyed by call id, while failed and read-only calls produce
// no snapshot.
func TestPlanUpdateSnapshotsRebuildsPerCallChecklists(t *testing.T) {
	explanation := "replanned"
	messages := []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{
			ID:        "legacy-1",
			Name:      legacyPlanToolName,
			Arguments: []byte(`{"plan":[{"step":"one","status":"completed"},{"step":"two","status":"in_progress"}]}`),
		}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "legacy-1",
			Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}},
		}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{
			ID:        "delta-1",
			Name:      planToolName,
			Arguments: []byte(`{"action":"add","explanation":"replanned","steps":[{"step":"three"}]}`),
		}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "delta-1",
			Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}},
		}}},
		// A read never changes state and must not produce a snapshot.
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{
			ID:        "show-1",
			Name:      planToolName,
			Arguments: []byte(`{"action":"show"}`),
		}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "show-1",
			Content: []provider.Content{provider.TextBlock{Text: "1. [x] one"}},
		}}},
		// A rejected call left a transcript row but changed nothing.
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{
			ID:        "rejected-1",
			Name:      planToolName,
			Arguments: []byte(`{"action":"set","steps":[{"step":"never","status":"pending"}]}`),
		}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "rejected-1",
			IsError: true,
			Content: []provider.Content{provider.TextBlock{Text: "plan: step text is required"}},
		}}},
	}

	snapshots := PlanUpdateSnapshots(messages)

	legacy, ok := snapshots["legacy-1"]
	if !ok {
		t.Fatalf("legacy call produced no snapshot: %#v", snapshots)
	}
	wantLegacy := []PlanStep{planStep("one", PlanCompleted), planStep("two", PlanInProgress)}
	if !slices.Equal(legacy.Plan, wantLegacy) {
		t.Fatalf("legacy snapshot = %#v, want %#v", legacy.Plan, wantLegacy)
	}

	delta, ok := snapshots["delta-1"]
	if !ok {
		t.Fatalf("delta call produced no snapshot: %#v", snapshots)
	}
	wantDelta := []PlanStep{planStep("one", PlanCompleted), planStep("two", PlanInProgress), planStep("three", PlanPending)}
	if !slices.Equal(delta.Plan, wantDelta) {
		t.Fatalf("delta snapshot = %#v, want %#v", delta.Plan, wantDelta)
	}
	if delta.Explanation == nil || *delta.Explanation != explanation {
		t.Fatalf("delta explanation = %#v, want %q", delta.Explanation, explanation)
	}

	if snap, ok := snapshots["show-1"]; ok {
		t.Fatalf("read-only call produced a snapshot: %#v", snap)
	}
	if snap, ok := snapshots["rejected-1"]; ok {
		t.Fatalf("rejected call produced a snapshot: %#v", snap)
	}

	// Each snapshot must own its slice: mutating the newest must not corrupt an
	// earlier checklist the transcript still renders.
	delta.Plan[0].Step = "mutated"
	if legacy.Plan[0].Step != "one" {
		t.Fatalf("snapshots share a backing slice: legacy = %#v", legacy.Plan)
	}
}
