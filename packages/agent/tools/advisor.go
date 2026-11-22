package tools

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/bnema/zut/packages/agent/advisor"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// AdvisorToolName is the model-visible name of the advisor tool.
const AdvisorToolName = provider.AdvisorToolName

// ErrNoAdvisorModel is returned by an AdvisorTarget resolver when the advisor
// is enabled but no model has been configured.
var ErrNoAdvisorModel = advisor.ErrNoModel

const advisorDescription = "Consult the advisor, a stronger reviewer that already watches your work and sees your full conversation transcript. No parameters. Call it only when the user asks you to, or when an [advisor checkpoint] message asks you to."

// AdvisorTarget is a ready advisor endpoint for one call.
type AdvisorTarget = advisor.Target

// AdvisorTool lets the executor consult the advisor explicitly. It shares
// the advisor conversation with the passive watcher, so the advisor answers
// with everything it has already reviewed. It has no arguments and no local
// side effects.
type AdvisorTool struct {
	// Target resolves the configured advisor model and builds its client on
	// each call, so config changes apply without rebuilding the tool.
	Target func(ctx context.Context) (AdvisorTarget, error)

	mu       sync.Mutex
	reviewer *advisor.Reviewer
}

// Clone returns an unbound copy sharing the same Target resolver.
func (t *AdvisorTool) Clone() *AdvisorTool {
	return &AdvisorTool{Target: t.Target}
}

// Bind attaches the reviewer of the agent whose transcript the tool
// forwards.
func (t *AdvisorTool) Bind(r *advisor.Reviewer) {
	t.mu.Lock()
	t.reviewer = r
	t.mu.Unlock()
}

func (t *AdvisorTool) Name() string        { return AdvisorToolName }
func (t *AdvisorTool) Description() string { return advisorDescription }
func (t *AdvisorTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func advisorError(msg string, usage *provider.Usage) core.ToolResult {
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: msg}},
		IsError: true,
		Usage:   usage,
	}
}

func (t *AdvisorTool) Execute(ctx context.Context, _ json.RawMessage, progress func(string)) (core.ToolResult, error) {
	t.mu.Lock()
	r := t.reviewer
	t.mu.Unlock()
	if r == nil {
		return advisorError("advisor is not available in this session", nil), nil
	}
	callID, _ := core.ToolCallIDFrom(ctx)
	text, usage, err := r.Consult(ctx, callID, progress)
	if err != nil {
		return advisorError(err.Error(), usage), nil
	}
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: text}},
		Usage:   usage,
	}, nil
}
