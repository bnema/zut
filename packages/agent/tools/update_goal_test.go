package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bnema/zut/packages/core"
)

func TestUpdateGoalToolReturnsPersistableStatus(t *testing.T) {
	tool := &UpdateGoalTool{}
	raw, err := json.Marshal(map[string]string{"status": "complete"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(context.Background(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("result = %#v, want success", result)
	}
	update, ok := GoalUpdateFromResult(result)
	if !ok {
		t.Fatalf("details = %#v, want goal update", result.Details)
	}
	if update.Status != core.GoalDone || update.Reason != "" {
		t.Fatalf("update = %#v", update)
	}
}

func TestUpdateGoalToolRejectsUnknownFields(t *testing.T) {
	tool := &UpdateGoalTool{}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"status":"complete","unexpected":true}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("result = %#v, want tool error", result)
	}
}

func TestUpdateGoalToolRejectsUnsupportedStatus(t *testing.T) {
	tool := &UpdateGoalTool{}
	raw := json.RawMessage(`{"status":"active"}`)
	result, err := tool.Execute(context.Background(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatalf("result = %#v, want tool error", result)
	}
	if _, ok := GoalUpdateFromResult(result); ok {
		t.Fatal("invalid result exposed a goal update")
	}
}
