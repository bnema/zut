package subagents

import (
	"sync"

	"github.com/bnema/zut/packages/core"
)

const residentLiveArgumentBytes = 64 << 10

// ResidentLiveTool is the bounded in-memory state of one active tool call.
// It is never written to the journal; finalized calls and results are served
// from the authoritative transcript instead.
type ResidentLiveTool struct {
	ID    string
	Name  string
	Args  []byte
	State ResidentLiveToolState
}

type ResidentLiveToolState string

const (
	ResidentLiveToolComposing ResidentLiveToolState = "composing"
	ResidentLiveToolReady     ResidentLiveToolState = "ready"
	ResidentLiveToolRunning   ResidentLiveToolState = "running"
)

// ResidentLiveSnapshot is an immutable copy of the unfinished visible turn.
// Hidden reasoning and unbounded tool output deliberately do not belong here.
type ResidentLiveSnapshot struct {
	TurnID        string
	State         ResidentState
	Revision      uint64
	AssistantText string
	Tools         []ResidentLiveTool
}

type residentLiveProjection struct {
	mu       sync.RWMutex
	snapshot ResidentLiveSnapshot
}

func newResidentLiveProjection() *residentLiveProjection { return &residentLiveProjection{} }

func (p *residentLiveProjection) Start(turnID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.snapshot.TurnID = turnID
	p.snapshot.State = ResidentRunning
	p.snapshot.AssistantText = ""
	p.snapshot.Tools = nil
	p.snapshot.Revision++
	p.mu.Unlock()
}

func (p *residentLiveProjection) Finish(state ResidentState) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.snapshot.State = state
	p.snapshot.AssistantText = ""
	p.snapshot.Tools = nil
	p.snapshot.Revision++
	p.mu.Unlock()
}

func (p *residentLiveProjection) Apply(event core.AgentEvent) {
	if p == nil || event == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := true
	switch value := event.(type) {
	case core.EvTextDelta:
		if len(p.snapshot.AssistantText) < residentLiveArgumentBytes {
			remaining := residentLiveArgumentBytes - len(p.snapshot.AssistantText)
			if len(value.Delta) > remaining {
				value.Delta = value.Delta[:remaining]
			}
			p.snapshot.AssistantText += value.Delta
		}
	case core.EvToolUseStart:
		p.upsertTool(value.ID, value.Name, ResidentLiveToolComposing)
	case core.EvToolUseArgs:
		if tool := p.tool(value.ID); tool != nil && len(tool.Args) < residentLiveArgumentBytes {
			remaining := residentLiveArgumentBytes - len(tool.Args)
			if len(value.Delta) > remaining {
				value.Delta = value.Delta[:remaining]
			}
			tool.Args = append(tool.Args, value.Delta...)
		}
	case core.EvToolCall:
		tool := p.upsertTool(value.ID, value.Name, ResidentLiveToolReady)
		tool.Args = append(tool.Args[:0], value.Args...)
		if len(tool.Args) > residentLiveArgumentBytes {
			tool.Args = tool.Args[:residentLiveArgumentBytes]
		}
	case core.EvToolExecutionStarted:
		p.upsertTool(value.ID, value.Name, ResidentLiveToolRunning)
	case core.EvToolResult:
		for index := range p.snapshot.Tools {
			if p.snapshot.Tools[index].ID == value.ID {
				p.snapshot.Tools = append(p.snapshot.Tools[:index], p.snapshot.Tools[index+1:]...)
				break
			}
		}
	case core.EvAssistantMessage:
		p.snapshot.AssistantText = ""
	default:
		changed = false
	}
	if changed {
		p.snapshot.Revision++
	}
}

func (p *residentLiveProjection) Snapshot() ResidentLiveSnapshot {
	if p == nil {
		return ResidentLiveSnapshot{}
	}
	p.mu.RLock()
	snapshot := p.snapshot
	snapshot.Tools = make([]ResidentLiveTool, len(p.snapshot.Tools))
	for index, tool := range p.snapshot.Tools {
		snapshot.Tools[index] = tool
		snapshot.Tools[index].Args = append([]byte(nil), tool.Args...)
	}
	p.mu.RUnlock()
	return snapshot
}

func (p *residentLiveProjection) tool(id string) *ResidentLiveTool {
	for index := range p.snapshot.Tools {
		if p.snapshot.Tools[index].ID == id {
			return &p.snapshot.Tools[index]
		}
	}
	return nil
}

func (p *residentLiveProjection) upsertTool(id, name string, state ResidentLiveToolState) *ResidentLiveTool {
	if tool := p.tool(id); tool != nil {
		if name != "" {
			tool.Name = name
		}
		tool.State = state
		return tool
	}
	p.snapshot.Tools = append(p.snapshot.Tools, ResidentLiveTool{ID: id, Name: name, State: state})
	return &p.snapshot.Tools[len(p.snapshot.Tools)-1]
}
