package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bnema/zut/packages/core"
)

// TestPlanToolDecodesEachAction covers the six actions the schema advertises.
// The tool only parses: state-dependent validation and result text belong to
// core, so these tests assert exactly what the operation carries.
func TestPlanToolDecodesEachAction(t *testing.T) {
	explanation := "why"
	tests := []struct {
		name string
		raw  string
		want core.PlanOperation
	}{
		{
			name: "set",
			raw:  `{"action":"set","explanation":"why","steps":[{"step":"one","status":"pending"},{"step":"two"}]}`,
			want: core.PlanOperation{
				Action:      "set",
				Explanation: &explanation,
				Steps:       []core.PlanStep{{Step: "one", Status: core.PlanPending}, {Step: "two"}},
			},
		},
		{
			name: "add",
			raw:  `{"action":"add","steps":[{"step":"three","status":"in_progress"}]}`,
			want: core.PlanOperation{
				Action: "add",
				Steps:  []core.PlanStep{{Step: "three", Status: core.PlanInProgress}},
			},
		},
		{
			name: "update status",
			raw:  `{"action":"update","index":2,"status":"completed"}`,
			want: core.PlanOperation{Action: "update", Index: 2, Status: core.PlanCompleted},
		},
		{
			name: "update text",
			raw:  `{"action":"update","index":1,"step":"renamed"}`,
			want: core.PlanOperation{Action: "update", Index: 1},
		},
		{
			name: "remove",
			raw:  `{"action":"remove","index":3}`,
			want: core.PlanOperation{Action: "remove", Index: 3},
		},
		{
			name: "clear",
			raw:  `{"action":"clear"}`,
			want: core.PlanOperation{Action: "clear"},
		},
		{
			name: "show",
			raw:  `{"action":"show"}`,
			want: core.PlanOperation{Action: "show"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := (&PlanTool{}).Execute(context.Background(), json.RawMessage(tt.raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError {
				t.Fatalf("result = %#v, want success", result)
			}
			op, ok := result.Details.(core.PlanOperation)
			if !ok {
				t.Fatalf("details = %T, want core.PlanOperation", result.Details)
			}
			if op.Action != tt.want.Action || op.Index != tt.want.Index || op.Status != tt.want.Status {
				t.Fatalf("operation = %#v, want %#v", op, tt.want)
			}
			if (op.Explanation == nil) != (tt.want.Explanation == nil) {
				t.Fatalf("explanation = %#v, want %#v", op.Explanation, tt.want.Explanation)
			}
			if op.Explanation != nil && *op.Explanation != *tt.want.Explanation {
				t.Fatalf("explanation = %q, want %q", *op.Explanation, *tt.want.Explanation)
			}
			if len(op.Steps) != len(tt.want.Steps) {
				t.Fatalf("steps = %#v, want %#v", op.Steps, tt.want.Steps)
			}
			for idx := range op.Steps {
				if op.Steps[idx] != tt.want.Steps[idx] {
					t.Fatalf("step %d = %#v, want %#v", idx, op.Steps[idx], tt.want.Steps[idx])
				}
			}
			if tt.name == "update text" {
				if op.Text == nil || *op.Text != "renamed" {
					t.Fatalf("text = %#v, want renamed", op.Text)
				}
			} else if op.Text != nil {
				t.Fatalf("text = %#v, want nil", op.Text)
			}
		})
	}
}

// TestPlanToolLeavesStateDependentValuesToCore guards the ownership split: an
// unknown status and an out-of-range index decode successfully and reach core,
// which rejects them against the current plan.
func TestPlanToolLeavesStateDependentValuesToCore(t *testing.T) {
	result, err := (&PlanTool{}).Execute(context.Background(), json.RawMessage(
		`{"action":"set","steps":[{"step":"one","status":"done"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("result = %#v, want decode to succeed", result)
	}
	op := result.Details.(core.PlanOperation)
	if op.Steps[0].Status != core.PlanStepStatus("done") {
		t.Fatalf("status = %q, want it passed through unchanged", op.Steps[0].Status)
	}

	result, err = (&PlanTool{}).Execute(context.Background(), json.RawMessage(`{"action":"remove","index":0}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("result = %#v, want decode to succeed", result)
	}
}

func TestPlanToolRejectsStructuralDecodeFailures(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "missing action", raw: `{}`},
		{name: "empty action", raw: `{"action":""}`},
		{name: "unknown top-level field", raw: `{"action":"clear","unexpected":true}`},
		{name: "unknown step field", raw: `{"action":"set","steps":[{"step":"one","unexpected":true}]}`},
		{name: "step text missing", raw: `{"action":"set","steps":[{"status":"pending"}]}`},
		{name: "trailing token", raw: `{"action":"clear"} trailing`},
		{name: "malformed json", raw: `{"action":`},
		{name: "wrong index type", raw: `{"action":"remove","index":"one"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := (&PlanTool{}).Execute(context.Background(), json.RawMessage(tt.raw), nil)
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError {
				t.Fatalf("result = %#v, want tool error", result)
			}
			if result.Details != nil {
				t.Fatalf("failed decode exposed details: %#v", result.Details)
			}
		})
	}
}

func TestPlanToolSchemaShape(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Enum  []string `json:"enum"`
			Items *struct {
				Required             []string `json:"required"`
				AdditionalProperties *bool    `json:"additionalProperties"`
			} `json:"items"`
		} `json:"properties"`
		Required             []string `json:"required"`
		AdditionalProperties *bool    `json:"additionalProperties"`
	}
	if err := json.Unmarshal((&PlanTool{}).Schema(), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "action" {
		t.Fatalf("required = %#v, want [action]", schema.Required)
	}
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		t.Fatalf("top-level additionalProperties = %#v, want false", schema.AdditionalProperties)
	}
	wantActions := []string{"set", "add", "update", "remove", "clear", "show"}
	if got := schema.Properties["action"].Enum; len(got) != len(wantActions) {
		t.Fatalf("action enum = %#v, want %#v", got, wantActions)
	}
	items := schema.Properties["steps"].Items
	if items == nil || len(items.Required) != 1 || items.Required[0] != "step" {
		t.Fatalf("step schema = %#v, want required [step]", items)
	}
	if items.AdditionalProperties == nil || *items.AdditionalProperties {
		t.Fatalf("step additionalProperties = %#v, want false", items.AdditionalProperties)
	}
}

func TestPlanToolKeepsArgumentRewriteDisabled(t *testing.T) {
	var tool core.ToolArgumentRewritePolicy = &PlanTool{}
	if tool.AllowArgumentRewrite() {
		t.Fatal("AllowArgumentRewrite = true, want false so guards cannot rewrite the checklist")
	}
}
