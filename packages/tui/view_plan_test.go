package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestViewRendersUpdatePlanAsChecklist(t *testing.T) {
	callID := "plan-1"
	args := json.RawMessage(`{
		"explanation":"Finished discovery",
		"plan":[
			{"step":"Map the implementation","status":"completed"},
			{"step":"Add the built-in tool","status":"in_progress"},
			{"step":"Verify the behavior","status":"pending"}
		]
	}`)
	view := &View{Theme: Dark, Messages: []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.ToolCallBlock{ID: callID, Name: "update_plan", Arguments: args},
		}},
		{Role: provider.RoleTool, Content: []provider.Content{
			provider.ToolResultBlock{CallID: callID, Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}}},
		}},
	}}

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
	if strings.Contains(plain, "update_plan") || strings.Contains(plain, "Plan updated") {
		t.Fatalf("rendered plan leaked generic tool chrome:\n%s", plain)
	}
}

func TestViewRendersLiveUpdatePlanAsChecklist(t *testing.T) {
	args := json.RawMessage(`{"plan":[{"step":"Implement","status":"in_progress"}]}`)
	view := &View{Theme: Dark}
	plain := stripANSI(strings.Join(view.RenderToolCall(ToolCallView{
		ID:         "plan-1",
		Name:       "update_plan",
		RawJSONBuf: string(args),
		Result:     "Plan updated",
		Done:       true,
	}, 50), "\n"))
	if !strings.Contains(plain, "Updated Plan") || !strings.Contains(plain, "□ Implement") {
		t.Fatalf("live plan rendering = %q", plain)
	}
	if strings.Contains(plain, "update_plan") || strings.Contains(plain, "Plan updated") {
		t.Fatalf("live plan leaked generic tool chrome: %q", plain)
	}
}

func TestViewRenderSnapshotsPreservePlanMetadata(t *testing.T) {
	callID := "plan-1"
	args := json.RawMessage(`{"plan":[{"step":"Implement","status":"in_progress"}]}`)
	messages := []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.ToolCallBlock{ID: callID, Name: "update_plan", Arguments: args},
		}},
		{Role: provider.RoleTool, Content: []provider.Content{
			provider.ToolResultBlock{CallID: callID, Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}}},
		}},
	}
	seed := func() *View {
		return &View{
			Theme:            Dark,
			Messages:         messages,
			MessagesRevision: 7,
			toolPathRevision: 7,
			toolCallNames:    map[string]string{callID: "update_plan"},
			toolCallArgs:     map[string]json.RawMessage{callID: args},
			toolCallLabels:   map[string]string{callID: "update_plan"},
			toolPaths:        map[string]string{},
			toolStartLines:   map[string]int{},
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

func TestViewFallsBackForMalformedUpdatePlan(t *testing.T) {
	cases := []string{
		`{"plan":[{"status":"pending"}]}`,
		`{"plan":[{"step":"work"}]}`,
		`{"plan":[{"step":"work","status":"unknown"}]}`,
		`{"plan":[{"step":"work","status":"pending","extra":true}]}`,
		`{"plan":[],"extra":true}`,
		`{"plan":[]} trailing`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			callID := "plan-1"
			view := &View{Theme: Dark, Messages: []provider.Message{
				{Role: provider.RoleAssistant, Content: []provider.Content{
					provider.ToolCallBlock{ID: callID, Name: "update_plan", Arguments: json.RawMessage(raw)},
				}},
				{Role: provider.RoleTool, Content: []provider.Content{
					provider.ToolResultBlock{CallID: callID, Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}}},
				}},
			}}
			plain := stripANSI(strings.Join(view.Build(50), "\n"))
			if strings.Contains(plain, "Updated Plan") || !strings.Contains(plain, "Plan updated") {
				t.Fatalf("malformed plan did not use generic fallback: %q", plain)
			}
		})
	}
}

func TestViewRendersEmptyUpdatePlan(t *testing.T) {
	callID := "plan-1"
	view := &View{Theme: Dark, Messages: []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.ToolCallBlock{ID: callID, Name: "update_plan", Arguments: json.RawMessage(`{"plan":[]}`)},
		}},
		{Role: provider.RoleTool, Content: []provider.Content{
			provider.ToolResultBlock{CallID: callID, Content: []provider.Content{provider.TextBlock{Text: "Plan updated"}}},
		}},
	}}

	plain := stripANSI(strings.Join(view.Build(40), "\n"))
	if !strings.Contains(plain, "(no steps provided)") {
		t.Fatalf("empty plan rendering = %q", plain)
	}
}
