package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// planTestMessages builds the assistant tool_use + tool result pair a plan call
// leaves in the transcript. Rendering now reads the checklist from the snapshot
// map keyed by the call id, not from these arguments.
func planTestMessages(callID, name, args string) []provider.Message {
	return []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.ToolCallBlock{ID: callID, Name: name, Arguments: []byte(args)},
		}},
		{Role: provider.RoleTool, Content: []provider.Content{
			provider.ToolResultBlock{CallID: callID, Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}}},
		}},
	}
}

func TestViewRendersPlanSnapshotAsChecklist(t *testing.T) {
	callID := "plan-1"
	explanation := "Finished discovery"
	view := &View{
		Theme:    Dark,
		Messages: planTestMessages(callID, "plan", `{"action":"set"}`),
		PlanUpdates: map[string]core.PlanUpdate{
			callID: {
				Explanation: &explanation,
				Plan: []core.PlanStep{
					{Step: "Map the implementation", Status: core.PlanCompleted},
					{Step: "Add the built-in tool", Status: core.PlanInProgress},
					{Step: "Verify the behavior", Status: core.PlanPending},
				},
			},
		},
	}

	plain := stripANSI(strings.Join(view.Build(60), "\n"))
	for _, want := range []string{
		"Updated Plan",
		"Finished discovery",
		"✓ Map the implementation",
		"□ Add the built-in tool",
		"□ Verify the behavior",
	} {
		if !strings.Contains(plain, want) {
			t.Fatalf("rendered plan omitted %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "plan ") || strings.Contains(plain, "Plan updated") {
		t.Fatalf("rendered plan leaked generic tool chrome:\n%s", plain)
	}
}

func TestViewRendersLegacyUpdatePlanSnapshotFromMap(t *testing.T) {
	callID := "legacy-1"
	view := &View{
		Theme:    Dark,
		Messages: planTestMessages(callID, "update_plan", `{"plan":[{"step":"Implement","status":"in_progress"}]}`),
		PlanUpdates: map[string]core.PlanUpdate{
			callID: {Plan: []core.PlanStep{{Step: "Implement", Status: core.PlanInProgress}}},
		},
	}
	plain := stripANSI(strings.Join(view.Build(60), "\n"))
	if !strings.Contains(plain, "Updated Plan") || !strings.Contains(plain, "□ Implement") {
		t.Fatalf("legacy plan rendering = %q", plain)
	}
}

func TestViewRendersLivePlanFromSnapshot(t *testing.T) {
	view := &View{
		Theme: Dark,
		PlanUpdates: map[string]core.PlanUpdate{
			"plan-1": {Plan: []core.PlanStep{{Step: "Implement", Status: core.PlanInProgress}}},
		},
	}
	plain := stripANSI(strings.Join(view.RenderToolCall(ToolCallView{
		ID:         "plan-1",
		Name:       "plan",
		RawJSONBuf: `{"action":"update","index":1,"status":"in_progress"}`,
		Done:       true,
	}, 50), "\n"))
	if !strings.Contains(plain, "Updated Plan") || !strings.Contains(plain, "□ Implement") {
		t.Fatalf("live plan rendering = %q", plain)
	}
	if strings.Contains(plain, "plan:") || strings.Contains(plain, "Plan updated") {
		t.Fatalf("live plan leaked generic tool chrome: %q", plain)
	}
}

// TestViewRendersCompactPlanLineWithoutSnapshot covers a plan call whose
// arguments are still streaming, before execution materialized any checklist.
func TestViewRendersCompactPlanLineWithoutSnapshot(t *testing.T) {
	view := &View{Theme: Dark}
	plain := stripANSI(strings.Join(view.RenderToolCall(ToolCallView{
		ID:         "plan-1",
		Name:       "plan",
		RawJSONBuf: `{"action":"update"`,
	}, 50), "\n"))
	if !strings.Contains(plain, "plan: update") {
		t.Fatalf("compact plan line = %q", plain)
	}
	if strings.Contains(plain, "Updated Plan") {
		t.Fatalf("compact plan line rendered a checklist: %q", plain)
	}
}

func TestViewRenderSnapshotsPreservePlanMetadata(t *testing.T) {
	callID := "plan-1"
	messages := planTestMessages(callID, "plan", `{"action":"set"}`)
	seed := func() *View {
		return &View{
			Theme:            Dark,
			Messages:         messages,
			MessagesRevision: 7,
			toolPathRevision: 7,
			toolCallNames:    map[string]string{callID: "plan"},
			PlanUpdates: map[string]core.PlanUpdate{
				callID: {Plan: []core.PlanStep{{Step: "Implement", Status: core.PlanInProgress}}},
			},
		}
	}
	assertChecklist := func(t *testing.T, view *View) {
		t.Helper()
		plain := stripANSI(strings.Join(view.Build(50), "\n"))
		if !strings.Contains(plain, "Updated Plan") || strings.Contains(plain, "Plan updated") {
			t.Fatalf("plan rendering = %q", plain)
		}
	}

	original := seed()
	assertChecklist(t, original.CloneForRender())

	owner := &View{Theme: Dark, Messages: messages, MessagesRevision: 7}
	owner.AdoptRenderCacheFrom(seed())
	assertChecklist(t, owner)
}

// TestViewCloneForRenderCopiesPlanSnapshots pins the deep copy: the render
// goroutine reads the clone while the live view keeps mutating its own map.
func TestViewCloneForRenderCopiesPlanSnapshots(t *testing.T) {
	callID := "plan-1"
	view := &View{
		Theme: Dark,
		PlanUpdates: map[string]core.PlanUpdate{
			callID: {Plan: []core.PlanStep{{Step: "one", Status: core.PlanPending}}},
		},
	}
	clone := view.CloneForRender()

	entry := view.PlanUpdates[callID]
	entry.Plan[0].Step = "changed"
	view.PlanUpdates[callID] = entry
	view.PlanUpdates["plan-2"] = core.PlanUpdate{}

	if got := clone.PlanUpdates[callID]; len(got.Plan) != 1 || got.Plan[0].Step != "one" {
		t.Fatalf("clone shares a plan slice with the source: %#v", got)
	}
	if _, ok := clone.PlanUpdates["plan-2"]; ok {
		t.Fatalf("clone observed a map insertion made after it was cloned")
	}
}

// TestViewPlanSnapshotsNotSharedWithRenderGoroutine is the -race guard for the
// render clone: the goroutine renders from its own copy of the map while the
// live view writes new snapshots.
func TestViewPlanSnapshotsNotSharedWithRenderGoroutine(t *testing.T) {
	callID := "plan-1"
	view := &View{
		Theme:       Dark,
		Messages:    planTestMessages(callID, "plan", `{"action":"set"}`),
		PlanUpdates: map[string]core.PlanUpdate{callID: {Plan: []core.PlanStep{{Step: "one", Status: core.PlanPending}}}},
	}
	clone := view.CloneForRender()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 25; i++ {
			_ = clone.Build(60)
		}
	}()
	for i := 0; i < 500; i++ {
		view.PlanUpdates[callID] = core.PlanUpdate{
			Plan: []core.PlanStep{{Step: fmt.Sprintf("step-%d", i), Status: core.PlanInProgress}},
		}
	}
	<-done
}

// TestViewPlanWithoutSnapshotFallsBackToToolChrome covers a plan call that
// produced no checklist: a failed call, or a read such as show.
func TestViewPlanWithoutSnapshotFallsBackToToolChrome(t *testing.T) {
	callID := "plan-1"
	view := &View{Theme: Dark, Messages: planTestMessages(callID, "plan", `{"action":"show"}`)}
	plain := stripANSI(strings.Join(view.Build(60), "\n"))
	if strings.Contains(plain, "Updated Plan") || !strings.Contains(plain, "Plan updated") {
		t.Fatalf("plan without a snapshot did not use generic fallback: %q", plain)
	}
}

func TestViewRendersEmptyPlanSnapshot(t *testing.T) {
	callID := "plan-1"
	view := &View{
		Theme:       Dark,
		Messages:    planTestMessages(callID, "plan", `{"action":"clear"}`),
		PlanUpdates: map[string]core.PlanUpdate{callID: {}},
	}
	plain := stripANSI(strings.Join(view.Build(40), "\n"))
	if !strings.Contains(plain, "(no steps provided)") {
		t.Fatalf("empty plan rendering = %q", plain)
	}
}

// TestViewCachedPlanRowInvalidatesOnRevision guards GO-009: PlanRevision is part
// of the render-cache key so a late or changed snapshot invalidates a message
// row that already rendered a checklist. Without it the cached path serves the
// old steps forever.
func TestViewCachedPlanRowInvalidatesOnRevision(t *testing.T) {
	callID := "plan-1"
	view := &View{
		Theme:            Dark,
		MessagesRevision: 1,
		Messages:         planTestMessages(callID, "plan", `{"action":"update"}`),
		PlanUpdates: map[string]core.PlanUpdate{
			callID: {Plan: []core.PlanStep{{Step: "old step", Status: core.PlanInProgress}}},
		},
	}

	first := stripANSI(strings.Join(view.Build(60), "\n"))
	if !strings.Contains(first, "old step") {
		t.Fatalf("first render = %q", first)
	}

	view.PlanUpdates = map[string]core.PlanUpdate{
		callID: {Plan: []core.PlanStep{{Step: "new step", Status: core.PlanInProgress}}},
	}
	view.PlanRevision++

	second := stripANSI(strings.Join(view.Build(60), "\n"))
	if strings.Contains(second, "old step") {
		t.Fatalf("second render reused the cached checklist: %q", second)
	}
	if !strings.Contains(second, "new step") {
		t.Fatalf("second render omitted the new checklist: %q", second)
	}
}
