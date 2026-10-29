package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

type recordingWebSearchTool struct {
	executions int
}

func (t *recordingWebSearchTool) Name() string        { return "web_search" }
func (t *recordingWebSearchTool) Description() string { return "test web search" }
func (t *recordingWebSearchTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (t *recordingWebSearchTool) Preview(context.Context, json.RawMessage) (core.ToolResult, error) {
	return core.ToolResult{}, nil
}
func (t *recordingWebSearchTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	t.executions++
	return core.ToolResult{}, nil
}

type executeOnlyWebSearchTool struct {
	executions int
}

func (t *executeOnlyWebSearchTool) Name() string        { return "web_search" }
func (t *executeOnlyWebSearchTool) Description() string { return "test web search" }
func (t *executeOnlyWebSearchTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (t *executeOnlyWebSearchTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	t.executions++
	return core.ToolResult{}, nil
}

func TestWebSearchSessionGuardPreservesPreviewCapability(t *testing.T) {
	guard := &webSearchSessionGuard{}
	previewable := &recordingWebSearchTool{}
	registry := guard.wrapRegistry(core.Registry{"web_search": previewable})
	wrapped := registry["web_search"]
	previewer, ok := wrapped.(core.ToolPreviewer)
	if !ok {
		t.Fatalf("preview-capable tool wrapper = %T, want core.ToolPreviewer", wrapped)
	}
	if _, err := previewer.Preview(context.Background(), json.RawMessage(`{}`)); !errors.Is(err, errWebSearchSessionRevoked) {
		t.Fatalf("revoked Preview error = %v, want session revocation", err)
	}
	if got := guard.wrapRegistry(registry)["web_search"]; got != wrapped {
		t.Fatalf("re-wrapped preview tool = %T, want existing wrapper", got)
	}

	executeOnly := &executeOnlyWebSearchTool{}
	registry = guard.wrapRegistry(core.Registry{"web_search": executeOnly})
	if _, ok := registry["web_search"].(core.ToolPreviewer); ok {
		t.Fatalf("execute-only tool wrapper = %T, unexpectedly advertises core.ToolPreviewer", registry["web_search"])
	}
}

func TestWebSearchSessionGuardRevokesSnapshottedToolBeforeExecute(t *testing.T) {
	guard := &webSearchSessionGuard{}
	underlying := &recordingWebSearchTool{}
	registry := guard.wrapRegistry(core.Registry{"web_search": underlying})
	guard.setAvailable(true)

	// This reference models core's per-execution registry snapshot.
	snapshotted := registry["web_search"]
	guard.setAvailable(false)
	delete(registry, "web_search")

	if _, err := snapshotted.Execute(context.Background(), json.RawMessage(`{}`), nil); !errors.Is(err, errWebSearchSessionRevoked) {
		t.Fatalf("snapshotted Execute error = %v, want revocation", err)
	}
	if underlying.executions != 0 {
		t.Fatalf("revoked underlying tool executed %d times", underlying.executions)
	}
}

type snapshottedWebSearchClient struct {
	calls int
}

func (c *snapshottedWebSearchClient) Name() string { return "snapshot-test" }

func (c *snapshottedWebSearchClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.calls++
	events := make(chan provider.Event, 2)
	if c.calls == 1 {
		events <- provider.EventStart{Provider: c.Name(), Model: req.Model}
		events <- provider.EventDone{Stop: provider.StopToolUse, Message: provider.Message{
			Role: provider.RoleAssistant,
			Content: []provider.Content{
				provider.ToolCallBlock{ID: "revoke", Name: "revoke_web_search", Arguments: json.RawMessage(`{}`)},
				provider.ToolCallBlock{ID: "search", Name: "web_search", Arguments: json.RawMessage(`{}`)},
			},
		}}
	} else {
		events <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.TextBlock{Text: "done"}},
		}}
	}
	close(events)
	return events, nil
}

type webSearchRevokerTool struct {
	guard *webSearchSessionGuard
	agent *core.Agent
}

func (t *webSearchRevokerTool) Name() string            { return "revoke_web_search" }
func (t *webSearchRevokerTool) Description() string     { return "revoke web search" }
func (t *webSearchRevokerTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *webSearchRevokerTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	t.guard.setAvailable(false)
	registry := t.agent.ToolsSnapshot()
	delete(registry, "web_search")
	t.agent.SetTools(registry)
	return core.ToolResult{}, nil
}

func TestWebSearchSessionGuardBlocksCoreSnapshotWithoutConfirmation(t *testing.T) {
	guard := &webSearchSessionGuard{}
	webSearch := &recordingWebSearchTool{}
	revoker := &webSearchRevokerTool{guard: guard}
	registry := guard.wrapRegistry(core.NewRegistry(revoker, webSearch))
	guard.setAvailable(true)
	agent := core.NewAgent(&snapshottedWebSearchClient{}, "model", "", registry)
	revoker.agent = agent

	// BeforeToolExecute is deliberately nil: this exercises the yolo/no-
	// confirmation path after core has snapshotted both tools for the batch.
	if err := agent.Prompt(context.Background(), "search", nil, nil); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if webSearch.executions != 0 {
		t.Fatalf("snapshotted web_search executed %d times after revocation", webSearch.executions)
	}
}

func TestWebSearchSessionGuardRefreshDoesNotReviveOldGeneration(t *testing.T) {
	guard := &webSearchSessionGuard{}
	oldUnderlying := &recordingWebSearchTool{}
	oldRegistry := guard.wrapRegistry(core.Registry{"web_search": oldUnderlying})
	guard.setAvailable(true)
	oldSnapshot := oldRegistry["web_search"]

	guard.setAvailable(false)
	freshUnderlying := &recordingWebSearchTool{}
	freshRegistry := guard.wrapRegistry(core.Registry{"web_search": freshUnderlying})
	guard.setAvailable(true)

	if _, err := oldSnapshot.Execute(context.Background(), json.RawMessage(`{}`), nil); !errors.Is(err, errWebSearchSessionRevoked) {
		t.Fatalf("old generation Execute error = %v, want revocation", err)
	}
	if _, err := freshRegistry["web_search"].Execute(context.Background(), json.RawMessage(`{}`), nil); err != nil {
		t.Fatalf("fresh generation Execute error: %v", err)
	}
	if oldUnderlying.executions != 0 || freshUnderlying.executions != 1 {
		t.Fatalf("execution counts old=%d fresh=%d, want 0 and 1", oldUnderlying.executions, freshUnderlying.executions)
	}
}

func TestWebSearchToolAllowedForSessionHonorsInvocationCeilings(t *testing.T) {
	cases := []struct {
		name        string
		args        Args
		wantAllowed bool
	}{
		{name: "default", args: Args{}, wantAllowed: true},
		{name: "no tools", args: Args{NoTools: true}, wantAllowed: false},
		{name: "permission set", args: Args{PermissionSet: &tools.PermissionSet{}}, wantAllowed: false},
		{name: "explicit web search", args: Args{ToolsSet: true, Tools: []string{"web_search"}}, wantAllowed: true},
		{name: "programmatic explicit web search", args: Args{Tools: []string{"web_search"}}, wantAllowed: true},
		{name: "no tools overrides explicit web search", args: Args{NoTools: true, ToolsSet: true, Tools: []string{"web_search"}}, wantAllowed: false},
		{name: "permission set overrides explicit web search", args: Args{PermissionSet: &tools.PermissionSet{}, ToolsSet: true, Tools: []string{"web_search"}}, wantAllowed: false},
		{name: "explicit tools exclude web search", args: Args{ToolsSet: true, Tools: []string{"read"}}, wantAllowed: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := webSearchToolAllowedForSession(tc.args); got != tc.wantAllowed {
				t.Fatalf("allowed = %v, want %v", got, tc.wantAllowed)
			}
		})
	}
}
