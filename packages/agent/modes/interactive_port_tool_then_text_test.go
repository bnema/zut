package modes

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// A turn that runs a tool and then replies must show the closing reply once
// the daemon reports idle.
func TestInteractivePortShowsReplyAfterToolRound(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	events := make(chan session.Event, 32)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: session.Info{ID: "s1", Run: session.RunStateIdle}, Seq: 1}, nil).Once()
	expectPortSubscribe(port, events, nil)
	r := startPortInteractive(t, port, nil)

	args := json.RawMessage(`{"command":"ls"}`)
	call := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "c1", Name: "bash", Arguments: args}}}
	final := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "closingwombat summary"}}}
	seq := uint64(1)
	send := func(ev session.Event) { seq++; ev.Seq = seq; events <- ev }
	send(session.Event{Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{ID: "s1", Run: session.RunStateRunning}}})
	send(session.Event{Kind: session.EvAgent, Agent: core.EvUserMessage{Message: portUserMessage("askprompt")}})
	send(session.Event{Kind: session.EvAgent, Agent: core.EvAssistantStart{}})
	send(session.Event{Kind: session.EvAgent, Agent: core.EvToolUseStart{ID: "c1", Name: "bash"}})
	send(session.Event{Kind: session.EvAgent, Agent: core.EvAssistantMessage{Message: call}})
	send(session.Event{Kind: session.EvAgent, Agent: core.EvTurnEnd{Stop: provider.StopToolUse}})
	send(session.Event{Kind: session.EvAgent, Agent: core.EvToolCall{ID: "c1", Name: "bash", Args: args}})
	send(session.Event{Kind: session.EvAgent, Agent: core.EvToolResult{ID: "c1", Result: core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "out"}}}}})
	send(session.Event{Kind: session.EvAgent, Agent: core.EvAssistantStart{}})
	send(session.Event{Kind: session.EvAgent, Agent: core.EvTextDelta{Delta: "closingwombat summary"}})
	send(session.Event{Kind: session.EvAgent, Agent: core.EvAssistantMessage{Message: final}})
	send(session.Event{Kind: session.EvAgent, Agent: core.EvTurnEnd{Stop: provider.StopEnd}})
	send(session.Event{Kind: session.EvAgent, Agent: core.EvDone{}})
	send(session.Event{Kind: session.EvInfo, Data: session.InfoEvent{Info: session.Info{ID: "s1", Run: session.RunStateIdle}}})
	r.waitOutput(t, "closingwombat summary")
	// The terminal log keeps transient frames; check the settled view too.
	require.Eventually(t, func() bool {
		r.i.mu.Lock()
		defer r.i.mu.Unlock()
		return !r.i.stream.Active() && !r.i.busy
	}, 5*time.Second, 10*time.Millisecond, "stream never settled")
	r.i.mu.Lock()
	cols, _ := r.i.cfg.Terminal.Size()
	r.i.renderOutsideLock = true
	joined := stripANSIBytes(strings.Join(r.i.cachedChatLocked(cols), "\n"))
	r.i.renderOutsideLock = false
	r.i.mu.Unlock()
	require.Contains(t, joined, "closingwombat summary")
}
