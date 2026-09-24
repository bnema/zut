package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func facadeResultText(t *testing.T, result core.ToolResult) string {
	t.Helper()
	var sb strings.Builder
	for _, block := range result.Content {
		text, ok := block.(provider.TextBlock)
		if !ok {
			t.Fatalf("content block = %T, want provider.TextBlock", block)
		}
		sb.WriteString(text.Text)
	}
	return sb.String()
}

func newFacadeManager(t *testing.T) *subagents.ResidentManager {
	t.Helper()
	manager := subagents.NewResidentManager(t.TempDir(), nil)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	return manager
}

// allActionsFacade wires one enabled internal tool per action so dispatch
// targets can be observed independently.
func allActionsFacade(t *testing.T) *SubagentTool {
	t.Helper()
	manager := newFacadeManager(t)
	enabled := func() bool { return true }
	return &SubagentTool{
		Spawn:  &SubagentSpawnTool{ResidentManager: manager, Enabled: enabled},
		Status: &SubagentStatusTool{ResidentManager: manager, Enabled: enabled},
		Stop:   &SubagentStopTool{ResidentManager: manager, Enabled: enabled},
		Resume: &SubagentResumeTool{ResidentManager: manager, Enabled: enabled},
	}
}

func TestSubagentFacadeNameAndSchema(t *testing.T) {
	facade := &SubagentTool{}
	if got := facade.Name(); got != "subagent" {
		t.Fatalf("Name() = %q, want subagent", got)
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(facade.Schema(), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "action" {
		t.Fatalf("required = %v, want [action]", schema.Required)
	}
	for _, field := range []string{
		"action", "task", "agent", "model", "provider", "reasoning", "fast_mode",
		"required", "wait", "isolation", "agent_id", "include_result", "watch", "prompt",
	} {
		if _, ok := schema.Properties[field]; !ok {
			t.Fatalf("schema is missing property %q", field)
		}
	}
	if got := facade.Description(); got != "Spawn, inspect, resume, and stop resident sub-agents. One action per call.\n\n"+subagentSpawnGuidance {
		t.Fatalf("description = %q", got)
	}
}

// facadeSchemaProperties parses the model-facing facade schema into its
// property descriptions.
func facadeSchemaProperties(t *testing.T) map[string]string {
	t.Helper()
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal((&SubagentTool{}).Schema(), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	props := make(map[string]string, len(schema.Properties))
	for name, prop := range schema.Properties {
		props[name] = prop.Description
	}
	return props
}

// TestSubagentFacadeSchemaCoversActionFields ties the flat, merged schema to
// the runtime action/field map, so a field added to an action cannot silently
// disappear from what the model sees.
func TestSubagentFacadeSchemaCoversActionFields(t *testing.T) {
	props := facadeSchemaProperties(t)
	for action, fields := range subagentActionFields {
		for _, field := range fields {
			if _, ok := props[field]; !ok {
				t.Fatalf("action %s field %q is missing from the facade schema", action, field)
			}
		}
	}
}

// subagentActionAnnotation pins the action-scoping sentence each
// action-owned property must carry. Every field in subagentActionFields must
// appear here, and vice versa, so the merged schema cannot advertise an
// action-owned field as unconditionally valid.
var subagentActionAnnotation = map[string]string{
	"task":           "Required when action is spawn.",
	"agent":          "Only valid when action is spawn.",
	"model":          "Only valid when action is spawn.",
	"provider":       "Only valid when action is spawn.",
	"reasoning":      "Only valid when action is spawn.",
	"fast_mode":      "Only valid when action is spawn.",
	"required":       "Only valid when action is spawn.",
	"wait":           "Only valid when action is spawn or resume.",
	"isolation":      "Only valid when action is spawn.",
	"agent_id":       "Required when action is stop or resume.",
	"include_result": "Only valid when action is status.",
	"watch":          "Only valid when action is status.",
	"prompt":         "Required when action is resume.",
}

func TestSubagentFacadeSchemaAnnotatesActionFields(t *testing.T) {
	props := facadeSchemaProperties(t)
	for action, fields := range subagentActionFields {
		for _, field := range fields {
			annotation, pinned := subagentActionAnnotation[field]
			if !pinned {
				t.Fatalf("action %s owns %q with no pinned annotation", action, field)
			}
			desc, ok := props[field]
			if !ok {
				t.Fatalf("schema is missing property %q owned by action %s", field, action)
			}
			if !strings.Contains(desc, annotation) {
				t.Fatalf("property %q description %q is missing %q", field, desc, annotation)
			}
		}
	}
	inFields := map[string]bool{}
	for _, fields := range subagentActionFields {
		for _, field := range fields {
			inFields[field] = true
		}
	}
	for field := range subagentActionAnnotation {
		if !inFields[field] {
			t.Fatalf("annotation table names %q, which no action owns", field)
		}
	}
}

// TestSubagentFacadeResumeCaveat keeps the former resume-tool warning in the
// facade schema: a terminal failure must be inspected before resume, and a
// follow-up continues the retained session rather than satisfying the work.
func TestSubagentFacadeResumeCaveat(t *testing.T) {
	props := facadeSchemaProperties(t)
	prompt := props["prompt"]
	for _, want := range []string{"inspect its saved result first", "without discarding progress or satisfying required work"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt description %q is missing %q", prompt, want)
		}
	}
}

// TestSubagentFacadeConfirmationKey covers the per-action confirmation grant:
// each action yields its own key, and anything malformed falls back to the
// plain base name so a remembered grant cannot widen.
func TestSubagentFacadeConfirmationKey(t *testing.T) {
	facade := &SubagentTool{}
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{raw: `{"action":"spawn","task":"t"}`, want: "subagent:spawn"},
		{raw: `{"action":"status"}`, want: "subagent:status"},
		{raw: `{"action":"stop","agent_id":"x"}`, want: "subagent:stop"},
		{raw: `{"action":"resume","agent_id":"x","prompt":"p"}`, want: "subagent:resume"},
		{raw: `{}`, want: "subagent"},
		{raw: `{"action":""}`, want: "subagent"},
		{raw: `{"action":"  "}`, want: "subagent"},
		{raw: `{"action":42}`, want: "subagent"},
		{raw: `{"action":["spawn"]}`, want: "subagent"},
		{raw: `{"action":{}}`, want: "subagent"},
		{raw: `{"action":true}`, want: "subagent"},
		{raw: `{not json`, want: "subagent"},
	} {
		if got := facade.ConfirmationKey(json.RawMessage(tc.raw)); got != tc.want {
			t.Fatalf("ConfirmationKey(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}
	seen := map[string]bool{}
	for _, action := range []string{"spawn", "status", "stop", "resume"} {
		key := facade.ConfirmationKey(json.RawMessage(`{"action":"` + action + `"}`))
		if seen[key] {
			t.Fatalf("action %s reuses key %q", action, key)
		}
		seen[key] = true
	}
}

func TestSubagentFacadeRequiresAction(t *testing.T) {
	facade := allActionsFacade(t)
	for _, raw := range []string{`{}`, `{"action":""}`, `{"action":"  "}`} {
		result, err := facade.Execute(context.Background(), json.RawMessage(raw), nil)
		if err != nil {
			t.Fatalf("%s: unexpected host error: %v", raw, err)
		}
		if !result.IsError || facadeResultText(t, result) != "subagent: action is required" {
			t.Fatalf("%s: result = %#v", raw, result)
		}
	}
	for _, raw := range []string{`{"action":42}`, `{"action":["spawn"]}`, `{"action":{}}`, `{"action":true}`} {
		result, err := facade.Execute(context.Background(), json.RawMessage(raw), nil)
		if err != nil {
			t.Fatalf("%s: unexpected host error: %v", raw, err)
		}
		if !result.IsError || facadeResultText(t, result) != "subagent: action must be a string" {
			t.Fatalf("%s: result = %#v, want action-must-be-a-string", raw, result)
		}
	}
}

func TestSubagentFacadeRejectsUnknownActionAndMalformedArgs(t *testing.T) {
	facade := allActionsFacade(t)
	result, err := facade.Execute(context.Background(), json.RawMessage(`{"action":"explode"}`), nil)
	if err != nil {
		t.Fatalf("unexpected host error: %v", err)
	}
	if !result.IsError || facadeResultText(t, result) != "subagent: unknown action" {
		t.Fatalf("unknown action result = %#v", result)
	}
	if _, err := facade.Execute(context.Background(), json.RawMessage(`{"action":`), nil); err == nil || !strings.Contains(err.Error(), "invalid args") {
		t.Fatalf("malformed args error = %v", err)
	}
}

func TestSubagentFacadeReportsUnavailableActions(t *testing.T) {
	facade := &SubagentTool{}
	for action, want := range map[string]string{
		"spawn":  "subagent: spawn action is unavailable in this mode",
		"status": "subagent: status action is unavailable in this mode",
		"stop":   "subagent: stop action is unavailable in this mode",
		"resume": "subagent: resume action is unavailable in this mode",
	} {
		raw := json.RawMessage(`{"action":"` + action + `"}`)
		result, err := facade.Execute(context.Background(), raw, nil)
		if err != nil {
			t.Fatalf("%s: unexpected host error: %v", action, err)
		}
		if !result.IsError || facadeResultText(t, result) != want {
			t.Fatalf("%s: result = %#v, want %q", action, result, want)
		}
	}
}

// Each action must reach only its own internal tool.
func TestSubagentFacadeDispatchesToOwningAction(t *testing.T) {
	facade := allActionsFacade(t)
	for action, raw := range map[string]string{
		"status": `{"action":"status","agent_id":"nope"}`,
		"stop":   `{"action":"stop","agent_id":"nope"}`,
		"resume": `{"action":"resume","agent_id":"nope","prompt":"follow up"}`,
	} {
		result, err := facade.Execute(context.Background(), json.RawMessage(raw), nil)
		if err != nil {
			t.Fatalf("%s: unexpected host error: %v", action, err)
		}
		if !result.IsError || !strings.Contains(facadeResultText(t, result), `no such agent "nope"`) {
			t.Fatalf("%s: result = %#v", action, result)
		}
	}
	result, err := facade.Execute(context.Background(), json.RawMessage(`{"action":"resume","agent_id":"nope"}`), nil)
	if err != nil {
		t.Fatalf("unexpected host error: %v", err)
	}
	if !result.IsError || !strings.Contains(facadeResultText(t, result), "prompt is required") {
		t.Fatalf("resume without prompt = %#v", result)
	}
}

// A field owned by another action is rejected before dispatch; a field no
// action owns still reaches the sub-tool and is rejected by its strict decoder.
func TestSubagentFacadeRejectsForeignFieldsAndKeepsUnknownOnes(t *testing.T) {
	facade := allActionsFacade(t)
	result, err := facade.Execute(context.Background(), json.RawMessage(`{"action":"stop","agent_id":"nope","wait":5}`), nil)
	if err != nil {
		t.Fatalf("unexpected host error: %v", err)
	}
	if !result.IsError || facadeResultText(t, result) != "subagent: wait is not valid for action stop; it belongs to the resume, spawn actions" {
		t.Fatalf("foreign field result = %#v", result)
	}
	result, err = facade.Execute(context.Background(), json.RawMessage(`{"action":"status","wait":5}`), nil)
	if err != nil {
		t.Fatalf("unexpected host error: %v", err)
	}
	if !result.IsError || facadeResultText(t, result) != "subagent: wait is not valid for action status; it belongs to the resume, spawn actions" {
		t.Fatalf("status foreign field result = %#v", result)
	}
	result, err = facade.Execute(context.Background(), json.RawMessage(`{"action":"spawn","task":"t","agent_id":"child"}`), nil)
	if err != nil {
		t.Fatalf("unexpected host error: %v", err)
	}
	if !result.IsError || facadeResultText(t, result) != "subagent: agent_id is not valid for action spawn; it belongs to the resume, status, stop actions" {
		t.Fatalf("multi-owner foreign field result = %#v", result)
	}
	_, err = facade.Execute(context.Background(), json.RawMessage(`{"action":"stop","agent_id":"nope","bogus":1}`), nil)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
}

// The parser forwards the selected action's raw field bytes unchanged, so the
// sub-tools keep their own number and validation semantics.
func TestSubagentFacadeForwardsActionPayload(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{raw: `{"action":"spawn","task":"review","wait":300}`, want: `{"task":"review","wait":300}`},
		{raw: `{"action":"status","agent_id":"child","include_result":true}`, want: `{"agent_id":"child","include_result":true}`},
		{raw: `{"action":"resume","agent_id":"child","prompt":"confirm"}`, want: `{"agent_id":"child","prompt":"confirm"}`},
		{raw: `{"action":"resume","agent_id":"child","prompt":"confirm","wait":30}`, want: `{"agent_id":"child","prompt":"confirm","wait":30}`},
		{raw: `{"action":"stop","agent_id":"child"}`, want: `{"agent_id":"child"}`},
	}
	for _, tc := range cases {
		action, payload, err := parseSubagentAction(json.RawMessage(tc.raw))
		if err != nil {
			t.Fatalf("%s: parse error %v", tc.raw, err)
		}
		var got, want map[string]json.RawMessage
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("%s: payload %s is not an object: %v", tc.raw, payload, err)
		}
		if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: payload = %s, want %s", tc.raw, payload, tc.want)
		}
		if action == "" {
			t.Fatalf("%s: empty action", tc.raw)
		}
	}
}
