package modes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/session"
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

func planProgress(plan []core.PlanStep) (current, total int) {
	total = len(plan)
	for idx, step := range plan {
		if step.Status == core.PlanInProgress {
			return idx + 1, total
		}
	}
	return 0, total
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
		// Fires at the top of every model request, including follow-ups
		// after tool use. The presenter anchors the transcript here so the
		// reply is never shown both as live text and as a finished message.
		i.startStreamLocked()
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
	case core.EvTextDelta:
		// The pacer paints buffered text a few runes per tick so every
		// provider has the same typewriter feel regardless of chunk size.
		i.stream.Push(e.Delta)
		i.wakeStreamPacer()
	case core.EvAssistantMessage:
		// Side effects read the final message, which is complete even
		// while the pacer is still painting it.
		i.assistantMessageSideEffects(e.Message)
		// The presenter keeps the live text until the last rune is painted,
		// then reveals the transcript in the same frame.
		i.stream.Finish()
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
			i.stream.Gate(e.ID)
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
			i.stream.Gate(e.ID)
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
		if update, ok := toolspkg.GoalUpdateFromResult(e.Result); ok && i.port == nil {
			if update.Status == core.GoalSuperseded && i.cfg.CurrentGoal != nil {
				if current := i.cfg.CurrentGoal(); current != nil {
					i.goalStatus = current.Status
				}
				// A superseding replacement owns its own lifecycle; a pending
				// Esc return for the replaced goal must not fire.
				i.interruptedGoalReturn = nil
			} else if update.Status == core.GoalActive {
				// Persistence runs before this event. Reflect a manager resume or
				// newly active goal, but do not let a stale result undo a user pause.
				if i.cfg.CurrentGoal != nil {
					if current := i.cfg.CurrentGoal(); current != nil {
						i.goalStatus = current.Status
					}
				} else if i.goalStatus != core.GoalPaused {
					i.goalStatus = core.GoalActive
				}
			} else if i.goalStatus == core.GoalActive {
				i.alertGoalTransitionLocked(i.goalStatus, update.Status)
				i.goalStatus = update.Status
				// Paused and terminal goals must not inherit an Esc-armed return.
				if update.Status == core.GoalPaused || update.Status == core.GoalDone || update.Status == core.GoalBlocked {
					i.interruptedGoalReturn = nil
				}
			}
		}
	case core.EvPlanUpdate:
		i.planCurrent, i.planTotal = planProgress(e.Update.Plan)
		// Copy-on-write: the live view may still reference the previous map while
		// a render reads it. Replacing the map instead of mutating it in place
		// keeps every published map immutable, so no reader can race a map write.
		next := make(map[string]core.PlanUpdate, len(i.planSnapshots)+1)
		for id, update := range i.planSnapshots {
			next[id] = update
		}
		next[e.CallID] = e.Update
		i.planSnapshots = next
		// A checklist can arrive after the tool-result message was first cached.
		// Bumping the revision invalidates those rows through msgVisualKey.
		i.planRevision++
	case core.EvUsage:
		if i.port != nil {
			// Usage and context gauges are domain projections (EvInfo).
			break
		}
		i.cumUsage = e.Cumulative
		if contextUsed := e.Usage.InputTokens + e.Usage.CacheReadTokens + e.Usage.CacheWriteTokens; contextUsed > 0 {
			i.lastCtxInput = contextUsed
		}
	case core.EvTurnRecovery:
		// One bounded core continuation fired because the turn ended
		// without a final answer. Surface it once so the extended work
		// does not read as a stall. The inner continuation is owned by
		// core, so no goal or scheduler follow-up is started here.
		i.extNotes = append(i.extNotes, "  "+i.cfg.Theme.FGColor(i.cfg.Theme.Tool, "Continuing: no final answer received."))
	case core.EvRepetitionGuard:
		switch e.Stage {
		case core.RepetitionGuardWarning:
			i.extNotes = append(i.extNotes, "  "+i.cfg.Theme.FGColor(i.cfg.Theme.Tool, "Repeated content detected; trying a different approach."))
		case core.RepetitionGuardStopped:
			i.statusErr = fmt.Sprintf("stopped a repetitive loop after %d identical attempts", e.Count)
			i.statusOK = ""
		}
	case core.EvTurnEnd:
		// Cancellation usually surfaces as a context error rather than
		// StopAborted. Either way, stop typing at once: text that never
		// reached the transcript must not keep painting and then vanish.
		if e.Stop == provider.StopAborted || errors.Is(e.Err, context.Canceled) {
			i.stream.Reset()
			i.statusErr = ""
			i.statusOK = "cancelled"
			return
		}
		// A request can end without an assistant message (empty reply,
		// extension suppression, provider error). Close the stream so the
		// transcript anchor never outlives its request.
		i.stream.Finish()
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
	// A draining stream is anchored in the old agent's transcript.
	i.stream.Reset()
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
		i.alertNeedsInput(alertAwaitingConfirmation)
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
	i.alertNeedsInput(alertAwaitingConfirmation)
	i.invalidate()
	return <-resp
}

// ---- Port mode (daemon client) -------------------------------------------

// portTranscriptLocked returns the transcript the view renders from and its
// revision. In Port mode it is the snapshot-hydrated copy; otherwise the
// legacy in-process agent (L16 SHIM). ok is false when neither exists.
func (i *Interactive) portTranscriptLocked() (msgs []provider.Message, rev uint64, ok bool) {
	if i.port != nil {
		return i.portMsgs, i.portRev, true
	}
	if i.agent != nil {
		return i.agent.Messages(), i.agent.Revision(), true
	}
	return nil, 0, false
}

// transcriptLenLocked is the raw transcript length and revision used to
// anchor the stream presenter.
func (i *Interactive) transcriptLenLocked() (int, uint64, bool) {
	if i.port != nil {
		return len(i.portMsgs), i.portRev, true
	}
	if i.agent != nil {
		return i.agent.MessageCount(), i.agent.Revision(), true
	}
	return 0, 0, false
}

// SessionPort returns the attached session port (nil in legacy mode).
func (i *Interactive) SessionPort() session.Port {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.port
}

// SessionClient returns the lifecycle client (nil when not configured).
func (i *Interactive) SessionClient() SessionClient {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.client
}

// SessionID returns the id of the attached session.
func (i *Interactive) SessionID() session.ID {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.sessionID
}

// SessionInfo returns the latest Info projection received from the daemon.
func (i *Interactive) SessionInfo() session.Info {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.portInfo
}

// PendingRequests returns the open confirm/question/rescue requests.
func (i *Interactive) PendingRequests() []session.PendingRequest {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]session.PendingRequest(nil), i.portPending...)
}

// DeferredActions returns the deferred actions recorded while detached (D3).
func (i *Interactive) DeferredActions() []session.DeferredAction {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]session.DeferredAction(nil), i.portDeferred...)
}

// QueuedInputs returns the daemon-side queue as last published.
func (i *Interactive) QueuedInputs() []session.QueuedInput {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]session.QueuedInput(nil), i.portQueue...)
}

// attachPort snapshots the session, hydrates the view and subscribes after
// the snapshot sequence so no event is missed or applied twice.
func (i *Interactive) attachPort(ctx context.Context) error {
	i.mu.Lock()
	port := i.port
	i.mu.Unlock()
	snap, err := port.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("session snapshot: %w", err)
	}
	subCtx, cancel := context.WithCancel(ctx)
	events, err := port.Subscribe(subCtx, snap.Seq)
	if err != nil {
		cancel()
		return fmt.Errorf("session subscribe: %w", err)
	}
	i.mu.Lock()
	if i.subCancel != nil {
		i.subCancel()
	}
	i.subCancel = cancel
	i.events = events
	i.hydrateLocked(snap)
	i.mu.Unlock()
	i.scrollToBottom()
	i.invalidate()
	return nil
}

// stopSubscription ends the live subscription.
func (i *Interactive) stopSubscription() {
	i.mu.Lock()
	if i.subCancel != nil {
		i.subCancel()
		i.subCancel = nil
	}
	i.events = nil
	i.mu.Unlock()
}

// hydrateLocked replaces every presentation projection with snap.
func (i *Interactive) hydrateLocked(snap session.Snapshot) {
	if snap.Info.ID != "" {
		i.sessionID = session.ID(snap.Info.ID)
	}
	i.lastSeq = snap.Seq
	i.portMsgs = append([]provider.Message(nil), snap.Messages...)
	i.portRev++
	i.portToolMsg = nil
	i.view.Messages = filterHiddenTranscriptMessages(i.portMsgs)
	i.view.TailLimit = 0
	if len(i.view.Messages) > initialResumeTailLimit {
		i.view.TailLimit = initialResumeTailLimit
	}
	i.resetTranscriptRenderLocked()
	i.stableChatCacheValid = false
	i.stream.Reset()
	i.applyInfoLocked(snap.Info)
	i.portQueue = append([]session.QueuedInput(nil), snap.Queue...)
	i.portPending = append([]session.PendingRequest(nil), snap.Pending...)
	i.portDeferred = append([]session.DeferredAction(nil), snap.Deferred...)
	i.planSnapshots = snap.Plan
	i.planRevision++
	i.applyExtChromeLocked(snap.ExtChrome)
	for _, n := range snap.Notes {
		i.applyNoteLocked(n)
	}
	// Live turn: rebuild streamed text and tool panels as of snap.Seq.
	i.toolCalls = map[string]*tui.ToolCallView{}
	i.toolOrder = nil
	if snap.Live.AssistantText != "" || len(snap.Live.Tools) > 0 {
		i.startStreamLocked()
		i.stream.Push(snap.Live.AssistantText)
		i.wakeStreamPacer()
	}
	for _, lt := range snap.Live.Tools {
		tc := &tui.ToolCallView{ID: lt.ID, Name: lt.Name, RawJSONBuf: string(lt.Args), Streaming: lt.State == "composing"}
		if len(lt.Args) > 0 && lt.State != "composing" {
			tc.Args = tui.ShortArgs(lt.Name, lt.Args)
		}
		if lt.Result != nil {
			tc.Done = true
			tc.Error = lt.Result.IsError
			tc.Result = toolResultText(*lt.Result)
		}
		i.bumpToolRevisionLocked(tc)
		i.toolCalls[lt.ID] = tc
		i.toolOrder = append(i.toolOrder, lt.ID)
		i.stream.Gate(lt.ID)
	}
}

func toolResultText(r core.ToolResult) string {
	var text strings.Builder
	for _, c := range r.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(tb.Text)
		}
	}
	return text.String()
}

// applyInfoLocked projects session.Info onto the status-bar view state.
func (i *Interactive) applyInfoLocked(info session.Info) {
	i.portInfo = info
	if info.Provider != "" {
		i.cfg.Provider = info.Provider
	}
	if info.Model != "" {
		i.cfg.Model = info.Model
	}
	i.cfg.Reasoning = info.Reasoning
	fast := info.FastMode
	i.cfg.FastMode = &fast
	if info.CWD != "" {
		i.cfg.CWD = info.CWD
	}
	busy := info.Run == session.RunStateRunning || info.Run == session.RunStateCompacting
	if busy && !i.busy {
		i.spin.Start()
	}
	i.busy = busy
	i.autoCompacting = info.Run == session.RunStateCompacting
	i.cumUsage = info.Usage
	i.lastCtxInput = info.ContextUsed
	i.goalStatus = ""
	if info.Goal != nil {
		i.goalStatus = core.GoalStatus(info.Goal.Status)
	}
	if info.PlanStep != nil {
		i.planCurrent, i.planTotal = info.PlanStep.Index, info.PlanStep.Total
	} else {
		i.planCurrent, i.planTotal = 0, 0
	}
	if info.Title != "" {
		i.sessionTitle = core.NormalizeSessionTitle(info.Title)
	}
	switch {
	case !busy:
		i.activity.activity = activity{kind: activityIdle}
	case info.Run == session.RunStateCompacting:
		i.activity.activity = activity{kind: activityCompactingHistory}
	}
	if !busy {
		i.flushPortToolMsgLocked()
		work := i.takePendingIdleWorkLocked()
		if len(work) > 0 {
			go runPendingIdleWork(work)
		}
	}
}

func (i *Interactive) applyExtChromeLocked(ch session.ExtChrome) {
	i.extStatuses = map[string]map[string]extensionStatus{}
	for ext, items := range ch.Statuses {
		i.extStatuses[ext] = map[string]extensionStatus{}
		for k, v := range items {
			i.extStatuses[ext][k] = extensionStatus{Level: v.Level, Text: v.Text}
		}
	}
	i.extWidgets = map[string]map[string]extensionWidget{}
	for ext, items := range ch.Widgets {
		i.extWidgets[ext] = map[string]extensionWidget{}
		for k, v := range items {
			i.extWidgets[ext][k] = extensionWidget{Position: v.Position, Title: v.Title, Lines: append([]string(nil), v.Lines...)}
		}
	}
	if len(ch.Panels) > 0 && i.extPanel != nil {
		p := ch.Panels[len(ch.Panels)-1]
		i.extPanel.Open(p.Ext, p.Spec.ID, p.Spec.Title, p.Spec.Lines, p.Spec.Footer)
	}
	if ch.Alert != nil {
		alert := *ch.Alert
		i.pendingAlert = &alert
	}
}

func (i *Interactive) applyNoteLocked(n session.Note) {
	switch n.Level {
	case "error":
		i.statusErr, i.statusOK = n.Text, ""
	case "warning", "warn":
		i.extNotes = append(i.extNotes, "  "+i.cfg.Theme.FGColor(i.cfg.Theme.Warning, n.Text))
	default:
		i.statusOK, i.statusErr = n.Text, ""
	}
}

// flushPortToolMsgLocked commits buffered tool results as the tool-role
// message the agent appended after executing the batch.
func (i *Interactive) flushPortToolMsgLocked() {
	if i.portToolMsg == nil {
		return
	}
	i.portMsgs = append(i.portMsgs, *i.portToolMsg)
	i.portRev++
	i.portToolMsg = nil
}

// applyPortAgentTranscript mirrors the agent's transcript appends from
// events, so the presenter's anchor logic works as with an in-process agent.
func (i *Interactive) applyPortAgentTranscriptLocked(ev core.AgentEvent) {
	switch e := ev.(type) {
	case core.EvTurnStart, core.EvDone, core.EvAssistantStart:
		i.flushPortToolMsgLocked()
	case core.EvUserMessage:
		i.flushPortToolMsgLocked()
		i.portMsgs = append(i.portMsgs, e.Message)
		i.portRev++
	case core.EvAssistantMessage:
		i.portMsgs = append(i.portMsgs, e.Message)
		i.portRev++
	case core.EvToolResult:
		if i.portToolMsg == nil {
			i.portToolMsg = &provider.Message{Role: provider.RoleTool, Time: time.Now()}
		}
		i.portToolMsg.Content = append(i.portToolMsg.Content, provider.ToolResultBlock{
			CallID: e.ID, Content: e.Result.Content, IsError: e.Result.IsError, Timing: e.Result.Timing, Context: e.Result.Context,
		})
	}
}

// handleSessionEvent applies one daemon event. done reports that Run must
// return (nil error): the session was taken over or closed without successor.
func (i *Interactive) handleSessionEvent(ctx context.Context, ev session.Event) (done bool) {
	// Seq 0 marks client-synthesized terminal events; never drop those.
	if ev.Seq != 0 {
		i.mu.Lock()
		if ev.Seq <= i.lastSeq {
			i.mu.Unlock()
			return false
		}
		i.lastSeq = ev.Seq
		i.mu.Unlock()
	}
	switch ev.Kind {
	case session.EvAgent:
		if ev.Agent == nil {
			return false
		}
		i.mu.Lock()
		i.applyPortAgentTranscriptLocked(ev.Agent)
		i.mu.Unlock()
		i.handleEventForPresentation(ev.Agent)
		return false
	case session.EvMessagesReplaced:
		i.resnapshot(ctx)
		return false
	case session.EvAttach:
		a, _ := ev.Data.(session.AttachEvent)
		if a.State == session.AttachStateDetached {
			i.mu.Lock()
			switch a.Reason {
			case "detached_by_other":
				i.exitNotice = detachedByOtherNotice
			default:
				i.exitNotice = "detached: " + a.Reason
			}
			i.mu.Unlock()
			return true
		}
		return false
	case session.EvClosed:
		c, _ := ev.Data.(session.ClosedEvent)
		if c.Successor != "" {
			if err := i.reattachSuccessor(ctx, c.Successor); err != nil {
				i.mu.Lock()
				i.exitNotice = "session closed; reattach failed: " + err.Error()
				i.mu.Unlock()
				return true
			}
			return false
		}
		i.mu.Lock()
		if i.exitNotice == "" {
			i.exitNotice = "session closed"
			if c.Reason != "" {
				i.exitNotice += ": " + c.Reason
			}
		}
		i.mu.Unlock()
		return true
	}
	i.mu.Lock()
	i.applySessionViewEventLocked(ev)
	i.mu.Unlock()
	i.invalidate()
	return false
}

func (i *Interactive) applySessionViewEventLocked(ev session.Event) {
	switch d := ev.Data.(type) {
	case session.InfoEvent:
		if d.Info.ID != "" {
			i.applyInfoLocked(d.Info)
		}
	case session.QueueEvent:
		i.portQueue = append([]session.QueuedInput(nil), d.Queue...)
	case session.PendingEvent:
		i.portPending = append(i.portPending, d.Pending)
	case session.PendingResolvedEvent:
		kept := i.portPending[:0:0]
		for _, p := range i.portPending {
			if p.ID != d.ID {
				kept = append(kept, p)
			}
		}
		i.portPending = kept
	case session.DeferredEvent:
		i.portDeferred = append(i.portDeferred, d.Deferred)
	case session.NoteEvent:
		i.applyNoteLocked(d.Note)
	case session.ExtChromeEvent:
		i.applyExtChromeLocked(d.ExtChrome)
	case session.ExtPanelEvent:
		switch d.Op {
		case "open":
			i.extPanel.Open(d.Panel.Ext, d.Panel.Spec.ID, d.Panel.Spec.Title, d.Panel.Spec.Lines, d.Panel.Spec.Footer)
		case "update":
			if i.extPanel.Active() && i.extPanel.ext == d.Panel.Ext && i.extPanel.id == d.Panel.Spec.ID {
				i.extPanel.Update(d.Panel.Spec.Title, d.Panel.Spec.Lines, d.Panel.Spec.Footer)
			}
		case "close":
			if i.extPanel.Active() && i.extPanel.ext == d.Panel.Ext && i.extPanel.id == d.Panel.Spec.ID {
				i.extPanel.Close()
			}
		}
	case session.ExtAlertEvent:
		alert := d.Alert
		i.pendingAlert = &alert
	case session.ExtDisplayEvent:
		prefix := i.cfg.Theme.FGColor(i.cfg.Theme.Accent, "["+d.Ext+"] ")
		for _, line := range strings.Split(d.Text, "\n") {
			i.extNotes = append(i.extNotes, prefix+i.cfg.Theme.FGColor(i.cfg.Theme.Muted, line))
		}
	case session.ExtInsertEvent:
		// Submit=true slash execution is routed by L16 (input owner);
		// presentation inserts the text either way.
		if i.ed != nil {
			i.ed.Insert(d.Text)
		}
	case session.PlanEvent:
		i.planSnapshots = d.Plan
		i.planRevision++
	case session.GoalEvent:
		i.goalStatus = core.GoalStatus(d.Status)
	case session.TitleEvent:
		i.sessionTitle = core.NormalizeSessionTitle(d.Title)
		i.writeTerminalTitleLocked(i.sessionTitle)
	case session.CompactionEvent:
		switch d.Phase {
		case "start":
			i.activity.activity = activity{kind: activityCompactingHistory}
		case "failed":
			i.statusErr, i.statusOK = "compaction failed", ""
		}
	case session.InterSessionEvent:
		label := "[message from " + string(d.From.SessionID)
		if d.From.Title != "" {
			label += " (" + d.From.Title + ")"
		}
		label += "] "
		i.extNotes = append(i.extNotes, "  "+i.cfg.Theme.FGColor(i.cfg.Theme.Accent, label)+i.cfg.Theme.FGColor(i.cfg.Theme.Muted, d.Text))
	}
}

// defaultSessionTimeout bounds async daemon calls made from the Run loop.
const defaultSessionTimeout = 10 * time.Second

type sessionResultKind uint8

const (
	sessionResultResnapshot sessionResultKind = iota
	sessionResultSuccessor
	sessionResultExit
)

// sessionResult is the outcome of an async daemon call, applied by the Run
// loop only when gen still matches the attached session.
type sessionResult struct {
	kind   sessionResultKind
	gen    uint64
	id     session.ID
	port   session.Port
	snap   session.Snapshot
	events <-chan session.Event
	cancel context.CancelFunc
	err    error
}

func (i *Interactive) sessionCallTimeout() time.Duration {
	if i.sessionTimeout > 0 {
		return i.sessionTimeout
	}
	return defaultSessionTimeout
}

func (i *Interactive) deliverSessionResult(ctx context.Context, res sessionResult) {
	select {
	case i.sessionResults <- res:
	case <-ctx.Done():
		if res.cancel != nil {
			res.cancel()
		}
	}
}

// resnapshot refetches the full state after the daemon replaced messages
// (compaction, fork, clear) without blocking the Run loop. The live
// subscription continues; events the snapshot already covers are dropped by
// Seq once it is applied.
func (i *Interactive) resnapshot(ctx context.Context) {
	i.mu.Lock()
	port, gen := i.port, i.sessionGen
	i.mu.Unlock()
	timeout := i.sessionCallTimeout()
	go func() {
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		snap, err := port.Snapshot(callCtx)
		if err != nil {
			err = fmt.Errorf("session snapshot: %w", err)
		}
		i.deliverSessionResult(ctx, sessionResult{kind: sessionResultResnapshot, gen: gen, snap: snap, err: err})
	}()
}

// reattachSuccessor follows /clear (D9): the daemon closed this session and
// created successor. The old stream is dropped at once; attach, snapshot and
// subscribe run off the Run loop and are applied by applySessionResult.
func (i *Interactive) reattachSuccessor(ctx context.Context, successor session.ID) error {
	i.mu.Lock()
	client := i.client
	i.mu.Unlock()
	if client == nil {
		return errors.New("no session client")
	}
	i.stopSubscription()
	i.mu.Lock()
	gen := i.sessionGen
	i.mu.Unlock()
	timeout := i.sessionCallTimeout()
	go func() {
		res := sessionResult{kind: sessionResultSuccessor, gen: gen, id: successor}
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		port, err := client.Attach(callCtx, successor)
		if err != nil {
			res.err = fmt.Errorf("attach %s: %w", successor, err)
			i.deliverSessionResult(ctx, res)
			return
		}
		snap, err := port.Snapshot(callCtx)
		if err != nil {
			res.err = fmt.Errorf("session snapshot: %w", err)
			i.deliverSessionResult(ctx, res)
			return
		}
		// The stream outlives the bounded call: scope it to the Run ctx.
		subCtx, subCancel := context.WithCancel(ctx)
		events, err := port.Subscribe(subCtx, snap.Seq)
		if err != nil {
			subCancel()
			res.err = fmt.Errorf("session subscribe: %w", err)
			i.deliverSessionResult(ctx, res)
			return
		}
		res.port, res.snap, res.events, res.cancel = port, snap, events, subCancel
		i.deliverSessionResult(ctx, res)
	}()
	return nil
}

// applySessionResult applies an async result on the Run loop. done reports
// that Run must end (successor reattach failed).
func (i *Interactive) applySessionResult(ctx context.Context, res sessionResult) (done bool) {
	i.mu.Lock()
	if res.gen != i.sessionGen {
		i.mu.Unlock()
		if res.cancel != nil {
			res.cancel()
		}
		return false
	}
	switch res.kind {
	case sessionResultResnapshot:
		if res.err != nil {
			i.statusErr, i.statusOK = res.err.Error(), ""
		} else if res.snap.Seq < i.lastSeq {
			// Newer events were applied while the call ran; this snapshot
			// would roll them back. Fetch a fresh one instead.
			i.mu.Unlock()
			i.resnapshot(ctx)
			return false
		} else {
			i.hydrateLocked(res.snap)
		}
	case sessionResultSuccessor:
		if res.err != nil {
			i.exitNotice = "session closed; reattach failed: " + res.err.Error()
			i.mu.Unlock()
			return true
		}
		i.sessionGen++
		if i.subCancel != nil {
			i.subCancel()
		}
		i.port, i.sessionID = res.port, res.id
		i.subCancel, i.events = res.cancel, res.events
		i.extNotes = nil
		i.statusErr, i.statusOK = "", ""
		i.hydrateLocked(res.snap)
	case sessionResultExit:
		if res.err != nil {
			i.statusErr, i.statusOK = res.err.Error(), ""
			i.mu.Unlock()
			i.invalidate()
			return false
		}
		i.mu.Unlock()
		return true
	}
	i.mu.Unlock()
	i.scrollToBottom()
	i.invalidate()
	return false
}

// SwitchSession replaces the attached session with port (/sessions live
// pick, /fork) and rehydrates the view. Pending async results for the old
// session are discarded. session.Port has no Detach; releasing the old
// session is the caller's job (cfg.Detach / SessionClient).
func (i *Interactive) SwitchSession(ctx context.Context, id session.ID, port session.Port) error {
	i.stopSubscription()
	i.mu.Lock()
	i.sessionGen++
	i.port = port
	i.sessionID = id
	i.extNotes = nil
	i.statusErr, i.statusOK = "", ""
	i.mu.Unlock()
	return i.attachPort(ctx)
}
