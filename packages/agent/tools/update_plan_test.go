package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestUpdatePlanToolReturnsPlanUpdate(t *testing.T) {
	tool := &UpdatePlanTool{}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{
		"explanation":"Finished discovery",
		"plan":[
			{"step":"Map the implementation","status":"completed"},
			{"step":"Add the built-in tool","status":"in_progress"},
			{"step":"Verify the behavior","status":"pending"}
		]
	}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("result = %#v, want success", result)
	}
	update, ok := result.Details.(core.PlanUpdate)
	if !ok {
		t.Fatalf("details = %T, want core.PlanUpdate", result.Details)
	}
	if update.Explanation == nil || *update.Explanation != "Finished discovery" || len(update.Plan) != 3 {
		t.Fatalf("update = %#v", update)
	}
	if update.Plan[1].Status != core.PlanInProgress {
		t.Fatalf("second status = %q, want %q", update.Plan[1].Status, core.PlanInProgress)
	}
	if len(result.Content) != 1 {
		t.Fatalf("content = %#v, want one text block", result.Content)
	}
	text, ok := result.Content[0].(provider.TextBlock)
	if !ok || text.Text != "Plan updated" {
		t.Fatalf("content = %#v, want Plan updated", result.Content)
	}
}

func TestUpdatePlanToolAcceptsEmptyPlan(t *testing.T) {
	result, err := (&UpdatePlanTool{}).Execute(context.Background(), json.RawMessage(`{"plan":[]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("result = %#v, want success", result)
	}
	update, ok := result.Details.(core.PlanUpdate)
	if !ok || update.Explanation != nil || len(update.Plan) != 0 {
		t.Fatalf("details = %#v, want empty plan update without explanation", result.Details)
	}
}

func TestUpdatePlanToolAcceptsMultipleInProgressSteps(t *testing.T) {
	result, err := (&UpdatePlanTool{}).Execute(context.Background(), json.RawMessage(`{"plan":[{"step":"one","status":"in_progress"},{"step":"two","status":"in_progress"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("result = %#v, want Codex-compatible acceptance", result)
	}
}

func TestUpdatePlanToolRejectsMalformedArguments(t *testing.T) {
	tests := []string{
		`{}`,
		`{"plan":[{"status":"pending"}]}`,
		`{"plan":[{"step":"work"}]}`,
		`{"plan":[{"step":"work","status":"unknown"}]}`,
		`{"plan":[],"unexpected":true}`,
		`{"plan":[{"step":"work","status":"pending","unexpected":true}]}`,
		`{"plan":[]} trailing`,
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			result, err := (&UpdatePlanTool{}).Execute(context.Background(), json.RawMessage(raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError {
				t.Fatalf("result = %#v, want tool error", result)
			}
			if _, ok := result.Details.(core.PlanUpdate); ok {
				t.Fatalf("invalid result exposed plan update: %#v", result.Details)
			}
		})
	}
}
