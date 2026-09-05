package modes

import (
	"context"
	"strings"
	"time"

	toolspkg "github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func eventAffectsPresentation(ev core.AgentEvent) bool {
	switch ev.(type) {
	case core.EvToolProgress:
		return false
	default:
		return true
	}
}
func (i *Interactive) bumpToolRevisionLocked(tc *tui.ToolCallView) {
	i.toolRenderRevision++
	if i.toolRenderRevision == 0 {
		// Overflow is practically unreachable, but resetting the sequence must
		// also drop old revisions before they can alias new frames.
		if i.view != nil {
			i.view.InvalidateRenderCache()
		}
		i.toolRenderRevision = 1
	}
	tc.Revision = i.toolRenderRevision
}
func (i *Interactive) handleEventForPresentation(ev core.AgentEvent) {
	i.handleEvent(ev)
	// Progress payloads are still delivered to the core sink and applied
	// in order, but they have no interactive-visible representation.
	if eventAffectsPresentation(ev) {
		i.invalidate()
	}
}
func (i *Interactive) handleEvent(ev core.AgentEvent) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.activity.apply(ev)
	switch e := ev.(type) {
	case core.EvAssistantStart:
		// Fires at the top of every oneTurn, including follow-up
		// turns after tool use. Without this, the streaming buffer
		// is still marked off from the previous assistant message
		// and the final summary text pops in all at once instead
		// of typewriter-streaming delta by delta.
		i.streaming.Reset()
		i.streamPending = i.streamPending[:0]
		i.streamFlushPending = false
		i.streamOn = true
		// Clear the live tool-call overlay. Any tools from the
		// previous round are now fully folded into the transcript
		// (assistant tool_use block + tool role message with the
		// result), so keeping them in the overlay would duplicate
		// them in the view — once inside the finalised transcript
		// and once below the streaming block, with the streaming
		// summary sandwiched in between. The next EvToolUseStart
		// will populate fresh entries for this turn's tools.
		i.toolCalls = map[string]*tui.ToolCallView{}
		i.toolOrder = nil
		i.toolGate = map[string]int{}
	case core.EvTextDelta:
		// Buffer into streamPending; the paintPace ticker drains
		// it into i.streaming a few runes at a time for a smooth
		// typewriter effect independent of upstream chunk size.
		i.streamPending = append(i.streamPending, []rune(e.Delta)...)
		i.streamOn = true
	case core.EvAssistantMessage:
		// OnAssistant + telegram mirroring always fire on message
		// arrival — they read the FINAL message content, which is
		// complete regardless of what's still in the pacer.
		i.assistantMessageSideEffects(e.Message)
		// If the pacer still has characters to drain, keep streamOn
		// true and mark flush pending; the paintPace ticker will
		// drain the remainder and reset streaming state when done.
		// Otherwise (rare: full-replay sessions, abort paths) clear
		// synchronously so a later render doesn't show stale text.
		if len(i.streamPending) > 0 {
			i.streamFlushPending = true
			return
		}
		i.resetStreamingStateLocked()
	case core.EvToolUseStart:
		// Live streaming: pre-create the view so the user sees the
		// tool call being composed in real time. Any subsequent
		// EvToolCall for the same ID updates the same struct (the
		// final parsed args + name are already known here).
		if _, exists := i.toolCalls[e.ID]; !exists {
			tc := &tui.ToolCallView{
				ID:        e.ID,
				Name:      e.Name,
				Streaming: true,
			}
			i.bumpToolRevisionLocked(tc)
			i.toolCalls[e.ID] = tc
			i.toolOrder = append(i.toolOrder, e.ID)
			i.gateToolLocked(e.ID)
		}
	case core.EvToolUseArgs:
		if tc, ok := i.toolCalls[e.ID]; ok {
			tc.RawJSONBuf += e.Delta
			i.bumpToolRevisionLocked(tc)
			// Refresh the live path as soon as it parses; used in
			// the header (write /Users/example/Desktop/demo.ts)
			// while the content is still streaming.
			if p, pok, _ := tui.ExtractPartialStringField(tc.RawJSONBuf, "path"); pok {
				tc.LivePath = p
			} else if p, pok, _ := tui.ExtractPartialStringField(tc.RawJSONBuf, "file_path"); pok {
				tc.LivePath = p
			}
		}
	case core.EvToolUseEnd:
		if tc, ok := i.toolCalls[e.ID]; ok {
			tc.Streaming = false
			i.bumpToolRevisionLocked(tc)
		}
	case core.EvToolCall:
		// If we already pre-created the view during streaming, just
		// refresh the final Args summary. Otherwise create a new one
		// (non-streaming providers or legacy paths).
		if tc, ok := i.toolCalls[e.ID]; ok {
			tc.Args = tui.ShortArgs(e.Name, e.Args)
			i.bumpToolRevisionLocked(tc)
			if tc.RawJSONBuf == "" {
				tc.RawJSONBuf = string(e.Args)
			}
			tc.Streaming = false
		} else {
			tc := &tui.ToolCallView{
				ID:         e.ID,
				Name:       e.Name,
				Args:       tui.ShortArgs(e.Name, e.Args),
				RawJSONBuf: string(e.Args),
			}
			i.bumpToolRevisionLocked(tc)
			i.toolCalls[e.ID] = tc
			i.toolOrder = append(i.toolOrder, e.ID)
			i.gateToolLocked(e.ID)
		}
	case core.EvToolResult:
		if tc, ok := i.toolCalls[e.ID]; ok {
			tc.Done = true
			tc.Error = e.Result.IsError
			tc.Preview = ""
			var text strings.Builder
			for _, c := range e.Result.Content {
				if tb, ok := c.(provider.TextBlock); ok {
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(tb.Text)
				}
			}
			tc.Result = text.String()
			i.bumpToolRevisionLocked(tc)
		}
		if update, ok := toolspkg.GoalUpdateFromResult(e.Result); ok {
			if update.Status == core.GoalActive {
				// A manager may advance a terminal goal to the next persisted
				// goal in the same mission. Do not let a late tool result resume
				// a goal explicitly paused by the user.
				if i.goalStatus != core.GoalPaused {
					i.goalStatus = core.GoalActive
				}
			} else if i.goalStatus == core.GoalActive {
				i.goalStatus = update.Status
			}
		}
	case core.EvUsage:
		i.cumUsage = e.Cumulative
		if contextUsed := e.Usage.InputTokens + e.Usage.CacheReadTokens + e.Usage.CacheWriteTokens; contextUsed > 0 {
			i.lastCtxInput = contextUsed
		}
	case core.EvTurnEnd:
		if e.Stop == provider.StopAborted {
			i.resetStreamingStateLocked()
			i.statusErr = ""
			i.statusOK = "cancelled"
			return
		}
		if e.Stop == provider.StopLength {
			// The model hit its output-token cap mid-response, so the
			// reply (often a long write/edit) is truncated. Surface it
			// explicitly, otherwise the turn just ends and reads like
			// the UI gave up. The agent already requests the model's
			// full MaxOutput budget, so this means the response genuinely
			// exceeded that ceiling; ask the user to continue.
			i.statusErr = "response hit the model's output-token limit and was cut off, ask it to continue"
			i.statusOK = ""
			return
		}
		// Don't surface mid-loop stream errors as a red banner here.
		// EvTurnEnd fires after every step in a multi-step tool loop,
		// so a transient 503 / network blip would briefly paint a red
		// banner over the still-streaming chat before the agent loop
		// either retries or exits. The final error (if any) is set by
		// startTurnWithImages once Prompt() returns, and recoverable
		// failures are routed to the rescue picker instead — which
		// keeps the chat clean while the agent is working.
		_ = e.Err
	}
}
func (i *Interactive) WebSearchPolicyGeneration() uint64 {
	return i.webSearchPolicyGeneration.Load()
}
func (i *Interactive) ApplyAgentPromptConfig(ag *core.Agent, system string, tools core.Registry) (core.Registry, bool) {
	return i.applyAgentPromptConfig(ag, system, tools, 0, false)
}
func (i *Interactive) ApplyAgentPromptConfigAtWebSearchGeneration(ag *core.Agent, system string, tools core.Registry, generation uint64) (core.Registry, bool) {
	return i.applyAgentPromptConfig(ag, system, tools, generation, true)
}
func (i *Interactive) applyAgentPromptConfig(ag *core.Agent, system string, tools core.Registry, generation uint64, checkGeneration bool) (core.Registry, bool) {
	if ag == nil {
		return nil, false
	}
	// Match the lock order used by dynamic tool mutations so a refresh
	// cannot race a snapshot-and-replace operation that would reintroduce
	// the old registry after the atomic prompt swap.
	i.agentMu.Lock()
	defer i.agentMu.Unlock()
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.agent != ag || checkGeneration && i.webSearchPolicyGeneration.Load() != generation {
		return nil, false
	}
	if i.telegramBridge != nil {
		// Telegram prompts arrive over an external messaging channel without a
		// per-request confirmation surface. Keep the normal interactive
		// registry from reintroducing web capabilities from the moment the
		// bridge is attached, including while its startup handshake is in flight.
		toolspkg.RemoveWebCapabilities(tools)
	}
	oldTools := ag.SetPromptConfig(system, tools)
	_, webSearchAvailable := tools["web_search"]
	i.setWebSearchAvailable(webSearchAvailable)
	return oldTools, true
}
func (i *Interactive) prepareReplacementAgentLocked(ag *core.Agent) {
	// This runs inside the replacement commit, never during candidate builds.
	i.resetCodexUsageLocked()
	if ag == nil {
		i.setWebSearchAvailable(false)
		return
	}
	registry := ag.ToolsSnapshot()
	if i.telegramBridge != nil {
		// External Telegram prompts may arrive as soon as the replacement is
		// published, so post-swap cleanup is too late.
		toolspkg.RemoveWebCapabilities(registry)
		ag.SetTools(registry)
	}
	_, webSearchAvailable := registry["web_search"]
	i.setWebSearchAvailable(webSearchAvailable && i.telegramBridge == nil)
}
func (i *Interactive) DeferUntilIdle(fn func()) {
	if fn == nil {
		return
	}
	i.mu.Lock()
	if i.busy {
		i.pendingIdleWork = append(i.pendingIdleWork, fn)
		i.mu.Unlock()
		return
	}
	i.mu.Unlock()
	fn()
}
func (i *Interactive) takePendingIdleWorkLocked() []func() {
	work := i.pendingIdleWork
	i.pendingIdleWork = nil
	return work
}
func runPendingIdleWork(work []func()) {
	for _, fn := range work {
		if fn != nil {
			fn()
		}
	}
}
func (i *Interactive) Agent() *core.Agent {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.agent
}
func (i *Interactive) runReloadExt(ctx context.Context) {
	if i.cfg.Extensions == nil {
		i.mu.Lock()
		i.statusErr = "no extension manager in this build"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.mu.Lock()
	i.statusOK = "Reloading extensions..."
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()

	go func() {
		stats := i.cfg.Extensions.Reload(ctx, 2*time.Second)
		msg, failed := formatReloadStatus(stats)
		seq := i.setReloadStatus(msg, failed)
		i.invalidate()
		go i.dismissReloadStatus(ctx, seq, msg, failed)
	}()
}
func (i *Interactive) Confirm(toolName string, preview string) core.ConfirmDecision {
	return i.ConfirmToolCall(core.ToolCallConfirmation{Name: toolName, Summary: preview})
}
func (i *Interactive) ConfirmToolCall(call core.ToolCallConfirmation) core.ConfirmDecision {
	resp := make(chan core.ConfirmDecision, 1)
	if isBtwOrigin(call.Origin) {
		req := &confirmRequest{
			toolName:      call.Name,
			preview:       call.Summary,
			resp:          resp,
			returnToChild: true,
		}
		if call.ID == "" || i.btwDialog == nil || !i.btwDialog.enqueueToolConfirmation(call.Origin, call.ID, call.Summary, call.Content, func() {
			i.confirmDialog.Enqueue(req)
		}) {
			return core.ConfirmDecision{Allow: false, Reason: "side-chat tool call canceled"}
		}
		i.invalidate()
		return <-resp
	}
	if call.ID != "" {
		i.mu.Lock()
		if tc, ok := i.toolCalls[call.ID]; ok {
			tc.Args = call.Summary
			tc.Preview = call.Content
			i.activity.activity = activity{kind: activityAwaitingConfirmation, tool: call.Name, provider: i.cfg.Provider, model: i.cfg.Model}
		}
		i.mu.Unlock()
	}
	i.confirmDialog.Enqueue(&confirmRequest{
		toolName: call.Name,
		preview:  call.Summary,
		resp:     resp,
	})
	i.invalidate()
	return <-resp
}
