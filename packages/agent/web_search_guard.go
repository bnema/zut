package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

var errWebSearchSessionRevoked = errors.New("web capability is unavailable in this session")

// webSearchSessionGuard is shared by every web capability tool instance built
// for one interactive session. Registry refreshes replace the advertised
// tools, but older tools already snapshotted by core still consult this guard
// at their execution boundary.
type webSearchSessionGuard struct {
	available  atomic.Bool
	generation atomic.Uint64
}

func (g *webSearchSessionGuard) setAvailable(available bool) {
	if g == nil {
		return
	}
	if !available {
		// Increment only on an actual allow-to-deny transition. A wrapper built
		// while denied then committed by a later successful refresh belongs to
		// the new generation; wrappers from before revocation never revive.
		if g.available.Swap(false) {
			g.generation.Add(1)
		}
		return
	}
	g.available.Store(true)
}

func (g *webSearchSessionGuard) wrapRegistry(reg core.Registry) core.Registry {
	if g == nil || reg == nil {
		return reg
	}
	for name, tool := range reg {
		if !tools.IsWebCapabilityName(name) || webSearchToolGuard(tool) == g {
			continue
		}
		guarded := &guardedWebSearchTool{
			Tool:       tool,
			guard:      g,
			generation: g.generation.Load(),
		}
		if previewer, ok := tool.(core.ToolPreviewer); ok {
			reg[name] = &guardedWebSearchPreviewTool{
				guardedWebSearchTool: guarded,
				previewer:            previewer,
			}
			continue
		}
		reg[name] = guarded
	}
	return reg
}

// guardedWebSearchTool wraps every member of the web capability. All extension
// interception, confirmation, and permission behavior remains in the normal
// core path; this final check only closes the stale-registry execution window.
type guardedWebSearchTool struct {
	core.Tool
	guard      *webSearchSessionGuard
	generation uint64
}

func webSearchToolGuard(tool core.Tool) *webSearchSessionGuard {
	switch tool := tool.(type) {
	case *guardedWebSearchTool:
		return tool.guard
	case *guardedWebSearchPreviewTool:
		return tool.guard
	default:
		return nil
	}
}

func (t *guardedWebSearchTool) available() bool {
	return t != nil && t.guard != nil && t.guard.available.Load() && t.generation == t.guard.generation.Load()
}

func (t *guardedWebSearchTool) Execute(ctx context.Context, args json.RawMessage, progress func(string)) (core.ToolResult, error) {
	if !t.available() {
		return core.ToolResult{}, errWebSearchSessionRevoked
	}
	return t.Tool.Execute(ctx, args, progress)
}

// guardedWebSearchPreviewTool preserves preview support only when the wrapped
// tool already implements core.ToolPreviewer.
type guardedWebSearchPreviewTool struct {
	*guardedWebSearchTool
	previewer core.ToolPreviewer
}

func (t *guardedWebSearchPreviewTool) Preview(ctx context.Context, args json.RawMessage) (core.ToolResult, error) {
	if !t.available() {
		return core.ToolResult{}, errWebSearchSessionRevoked
	}
	return t.previewer.Preview(ctx, args)
}
