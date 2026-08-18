package modes

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func (i *Interactive) clearPendingCompactTurnLocked() {
	i.pendingCompactPrompt = ""
	i.pendingCompactImages = nil
	i.hasPendingCompactPrompt = false
	i.continueAfterCompact = false
	i.pendingPostCompactNote = ""
}
func (i *Interactive) compactHandoffLocked() json.RawMessage {
	return encodeCompactHandoff(i.compactContinuation)
}
func (i *Interactive) setCompactContinuationLocked(state compactContinuationState) (json.RawMessage, bool) {
	if i.compactContinuation == state {
		return i.compactHandoffLocked(), false
	}
	i.compactContinuation = state
	return i.compactHandoffLocked(), true
}
func (i *Interactive) resetCompactContinuationLocked() (json.RawMessage, bool) {
	return i.setCompactContinuationLocked(compactContinuationState{})
}
func (i *Interactive) persistCompactHandoff(state json.RawMessage) {
	if persist := i.cfg.PersistCompactHandoff; persist != nil {
		if err := persist(state); err != nil {
			i.ReportError(fmt.Errorf("persist compact handoff: %w", err))
		}
	}
}
func (i *Interactive) resetCompactHandoff() {
	i.mu.Lock()
	state, persist := i.resetCompactContinuationLocked()
	i.mu.Unlock()
	if persist {
		i.persistCompactHandoff(state)
	}
}
func (i *Interactive) currentCompactHandoff() json.RawMessage {
	if current := i.cfg.CurrentCompactHandoff; current != nil {
		return current()
	}
	return nil
}
func (i *Interactive) restoreCompactHandoff(state compactContinuationState) {
	i.mu.Lock()
	i.compactContinuation = state
	i.mu.Unlock()
}
func (i *Interactive) restoreCurrentCompactHandoff() compactContinuationState {
	state := decodeCompactHandoff(i.currentCompactHandoff())
	i.restoreCompactHandoff(state)
	return state
}
func classifyCompactHandoffResume(messages []provider.Message) compactHandoffResume {
	lastHandoff := -1
	for idx := len(messages) - 1; idx >= 0; idx-- {
		message := messages[idx]
		if message.Role == provider.RoleUser && message.Meta[autoCompactContinueMetaKey] == "true" {
			lastHandoff = idx
			break
		}
	}
	if lastHandoff < 0 {
		return compactHandoffAppendPrompt
	}
	for _, message := range messages[lastHandoff+1:] {
		switch message.Role {
		case provider.RoleTool:
			continue
		case provider.RoleAssistant:
			hasToolCall := false
			for _, content := range message.Content {
				if _, ok := content.(provider.ToolCallBlock); ok {
					hasToolCall = true
					break
				}
			}
			if !hasToolCall {
				return compactHandoffDiscard
			}
		default:
			return compactHandoffDiscard
		}
	}
	return compactHandoffContinueExisting
}
func (i *Interactive) startRestoredCompactHandoff(parent context.Context) {
	if parent == nil {
		parent = i.runCtx
	}
	if parent == nil {
		parent = context.Background()
	}
	i.mu.Lock()
	state := i.compactContinuation
	ag := i.agent
	busy := i.busy || i.compacting || i.autoCompacting || i.shellRunning
	if state.reason == compactContinuationNone || ag == nil || busy {
		i.mu.Unlock()
		return
	}

	var next core.QueuedMessage
	var hasNext bool
	continueQueued := false
	var resume compactHandoffResume
	var handoff json.RawMessage
	var persistHandoff bool
	switch {
	case state.reason != compactContinuationForcedLength && len(i.queued) > 0:
		next, i.queued = i.queued[0], i.queued[1:]
		hasNext = true
		handoff, persistHandoff = i.resetCompactContinuationLocked()
	case state.reason != compactContinuationForcedLength && ag.QueuedMessageCount() > 0:
		continueQueued = true
		handoff, persistHandoff = i.resetCompactContinuationLocked()
	default:
		resume = classifyCompactHandoffResume(ag.Messages())
		if resume == compactHandoffDiscard {
			handoff, persistHandoff = i.resetCompactContinuationLocked()
		}
	}
	// Reserve the turn slot before persisting or dispatching so concurrent input
	// can only queue behind the restored handoff, never start a competing turn.
	starting := hasNext || continueQueued || resume == compactHandoffContinueExisting || resume == compactHandoffAppendPrompt
	i.busy = starting
	i.mu.Unlock()
	if persistHandoff {
		i.persistCompactHandoff(handoff)
	}
	if !starting {
		i.invalidate()
		return
	}
	switch {
	case hasNext:
		i.startTurnWithImages(parent, next.Text, next.Images)
	case continueQueued:
		i.startTurnRequest(parent, "", nil, true, false)
	case resume == compactHandoffContinueExisting:
		i.startTurnRequest(parent, "", nil, true, false)
	case resume == compactHandoffAppendPrompt:
		i.startAutoCompactContinuation(parent)
	}
}
func (i *Interactive) startAutoCompactContinuation(parent context.Context) {
	i.mu.Lock()
	ag := i.agent
	if ag == nil {
		i.busy = false
		pendingIdleWork := i.takePendingIdleWorkLocked()
		i.mu.Unlock()
		runPendingIdleWork(pendingIdleWork)
		i.invalidate()
		return
	}
	// The caller selected the continuation while holding i.mu. Keep that
	// ordering through the hidden append so an explicitly queued user prompt
	// cannot be overtaken by an old-task handoff. Forced truncation is the
	// existing exception: it keeps priority and the queued prompt runs after it.
	if i.compactContinuation.reason != compactContinuationForcedLength && len(i.queued) > 0 {
		next := i.queued[0]
		i.queued = i.queued[1:]
		handoff, persistHandoff := i.resetCompactContinuationLocked()
		i.mu.Unlock()
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
		i.startTurnWithImages(parent, next.Text, next.Images)
		return
	}
	if i.compactContinuation.reason != compactContinuationForcedLength && ag.QueuedMessageCount() > 0 {
		handoff, persistHandoff := i.resetCompactContinuationLocked()
		i.mu.Unlock()
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
		i.startTurnRequest(parent, "", nil, true, false)
		return
	}
	ag.AppendUserContext(autoCompactContinuationPrompt, map[string]string{
		autoCompactContinueMetaKey: "true",
	})
	i.mu.Unlock()
	i.startTurnRequest(parent, "", nil, true, false)
}
func (i *Interactive) runCompact(parent context.Context, request compactContinuationRequest) {
	auto := request.origin != compactOriginManual
	if i.agent == nil {
		i.mu.Lock()
		i.statusErr = "not logged in. type /login first."
		i.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	i.mu.Lock()
	i.busy = true
	i.compacting = true
	i.spin.Start()
	i.activity = newAgentActivity(i.cfg.Provider, i.cfg.Model)
	if auto {
		i.activity.kind = activityCondensingHistory
		i.autoCompacting = true
	} else {
		i.activity.kind = activityCompactingHistory
	}
	i.cancelTurn = cancel
	var initialHandoff json.RawMessage
	persistInitialHandoff := false
	if request.origin == compactOriginManual || request.origin == compactOriginRecovery {
		initialHandoff, persistInitialHandoff = i.resetCompactContinuationLocked()
	}
	i.statusErr = ""
	i.statusOK = ""
	// Do NOT set streamOn: the summary text should not be visible
	// in the chat while compacting. The user just sees the spinner
	// and can keep typing / queue prompts.
	i.scrollOffset = 0
	i.helpBlock = nil
	i.mu.Unlock()
	if persistInitialHandoff {
		i.persistCompactHandoff(initialHandoff)
	}
	i.invalidate()

	go func() {
		// Sink discards deltas — we don't stream the summary to the UI.
		sink := func(delta string) {}
		msgsBefore := i.agent.Messages()
		i.mu.Lock()
		statusRescueActive := i.compactContinuation.reason == compactContinuationStatusRescue
		i.mu.Unlock()
		continuationReason := classifyCompactionContinuation(request.origin, statusRescueActive, request.lastStop, request.turnError, msgsBefore)
		if request.force {
			continuationReason = compactContinuationForcedLength
		}
		// Keep the usual recent tail when possible, but never let it cover
		// the whole transcript. A short session can still be at 70–90% of
		// a model's window because one prompt or tool result is large; the
		// automatic path must summarize that session instead of failing
		// before it reaches the compaction provider.
		keepTail := 4
		if n := len(msgsBefore); n > 0 && keepTail >= n {
			keepTail = n - 1
		}
		summary, err := i.agent.Compact(ctx, keepTail, sink)
		_ = summary
		goalMessage, goalActive := i.goalContinuationMessage()
		i.mu.Lock()
		// Keep busy/compacting asserted while cleanup and queue selection run.
		// Completion updates can arrive in this window; clearing busy before
		// inspecting the queues lets them race a new turn or be stranded.
		i.resetStreamingStateLocked()
		i.cancelTurn = nil
		i.autoCompacting = false
		pendingIdleWork := i.takePendingIdleWorkLocked()

		// Drain pending work after the transcript is clean. Overflow recovery
		// continues the user message already in the transcript. Pre-turn
		// compaction preserves its complete text-and-images request. Regular
		// prompts typed during compaction remain in the host queue.
		var next string
		var nextImages []provider.ImageBlock
		var hasNext bool
		var continueExisting bool
		var continueAutomatically bool
		var continueGoal bool
		var handoff json.RawMessage
		var persistHandoff bool

		switch {
		case err != nil && ctx.Err() != nil:
			i.statusErr = ""
			if auto {
				i.statusOK = "auto-condense cancelled"
			} else {
				i.statusOK = "compaction cancelled"
			}
			i.queued = nil // drop queue on cancel
			i.clearPendingCompactTurnLocked()
			handoff, persistHandoff = i.resetCompactContinuationLocked()
			if i.agent != nil {
				i.agent.DrainQueuedMessages()
			}
		case err != nil:
			i.statusErr = "compaction failed: " + err.Error()
			i.statusOK = ""
			i.queued = nil // drop queue on error
			i.clearPendingCompactTurnLocked()
			handoff, persistHandoff = i.resetCompactContinuationLocked()
			if i.agent != nil {
				i.agent.DrainQueuedMessages()
			}
		default:
			i.statusErr = ""
			// Read token count from the compaction message meta.
			tokens := ""
			msgs := i.agent.Messages()
			if len(msgs) > 0 && msgs[0].Meta["compaction"] == "true" {
				tokens = msgs[0].Meta["tokens_before"]
			}
			switch {
			case i.pendingPostCompactNote != "":
				i.statusOK = i.pendingPostCompactNote
			case tokens != "":
				i.statusOK = fmt.Sprintf("compacted from ~%s tokens (ctrl+o to expand)", tokens)
			default:
				i.statusOK = "compacted (ctrl+o to expand)"
			}
			i.pendingPostCompactNote = ""
			i.extNotes = stripAutoCompactNotes(i.extNotes)
			i.lastCtxInput = 0
			i.toolCalls = map[string]*tui.ToolCallView{}
			i.toolOrder = nil
			i.toolGate = map[string]int{}
			i.resetTranscriptRenderLocked()
			switch {
			case i.continueAfterCompact:
				continueExisting = true
				i.continueAfterCompact = false
			case i.hasPendingCompactPrompt:
				next = i.pendingCompactPrompt
				nextImages = i.pendingCompactImages
				i.pendingCompactPrompt = ""
				i.pendingCompactImages = nil
				i.hasPendingCompactPrompt = false
				hasNext = true
			case continuationReason == compactContinuationForcedLength:
				// Forced truncated-output continuation keeps its existing
				// priority over an explicitly queued prompt.
				handoff, persistHandoff = i.setCompactContinuationLocked(compactContinuationState{reason: continuationReason})
				continueAutomatically = true
			case len(i.queued) > 0:
				queued := i.queued[0]
				i.queued = i.queued[1:]
				next = queued.Text
				nextImages = queued.Images
				hasNext = true
				handoff, persistHandoff = i.resetCompactContinuationLocked()
			case continuationReason == compactContinuationStructuralTail || continuationReason == compactContinuationStatusRescue:
				if continuationReason == compactContinuationStatusRescue {
					attempts := 1
					if i.compactContinuation.reason == compactContinuationStatusRescue {
						attempts = i.compactContinuation.rescueAttempts + 1
					}
					if attempts > maxStatusRescueContinuations {
						handoff, persistHandoff = i.resetCompactContinuationLocked()
						break
					}
					handoff, persistHandoff = i.setCompactContinuationLocked(compactContinuationState{
						reason:         continuationReason,
						rescueAttempts: attempts,
					})
				} else {
					handoff, persistHandoff = i.setCompactContinuationLocked(compactContinuationState{reason: continuationReason})
				}
				continueAutomatically = true
			case auto && goalActive && !i.coordinatorHasPendingWorkers():
				continueGoal = true
				handoff, persistHandoff = i.resetCompactContinuationLocked()
			default:
				handoff, persistHandoff = i.resetCompactContinuationLocked()
			}
		}
		// Keep the host busy until the hand-off decision is committed under
		// the mutex. A completion update arriving after the compaction result
		// but before this assignment is then queued for the selected follow-up
		// instead of starting a competing turn.
		i.busy = hasNext || continueExisting || continueAutomatically || continueGoal
		i.compacting = false
		i.autoCompacting = false
		i.mu.Unlock()
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
		runPendingIdleWork(pendingIdleWork)
		i.invalidate()

		if hasNext || continueExisting || continueAutomatically || continueGoal {
			p := i.runCtx
			if p == nil {
				p = context.Background()
			}
			switch {
			case continueExisting:
				i.startTurnRequest(p, "", nil, true, true)
			case hasNext:
				i.startTurnWithImages(p, next, nextImages)
			case continueAutomatically:
				i.startAutoCompactContinuation(p)
			case continueGoal:
				var run *goalContinuationRun
				if i.cfg.CurrentGoal != nil {
					goal := copySessionGoal(i.cfg.CurrentGoal())
					if !i.limitGoalBeforeRun(goal) {
						run, err = i.startGoalRun(goal)
					}
				}
				if err != nil || run == nil {
					i.mu.Lock()
					i.busy = false
					i.mu.Unlock()
					if err != nil {
						i.ReportError(err)
					}
					return
				}
				i.startGoalContinuation(p, goalMessage, run)
			}
		}
	}()
}
