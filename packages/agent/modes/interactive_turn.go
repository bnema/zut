package modes

import (
	"context"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func shellEscapeCommand(text string) (string, bool) {
	return ShellEscapeCommand(text)
}
func ShellEscapeCommand(text string) (string, bool) {
	trimmed := strings.TrimLeft(text, " \t")
	if !strings.HasPrefix(trimmed, "!") {
		return "", false
	}
	cmd := strings.TrimSpace(strings.TrimPrefix(trimmed, "!"))
	if cmd == "" {
		return "", false
	}
	return cmd, true
}
func (i *Interactive) startShellEscape(parent context.Context, cmd string) {
	i.mu.Lock()
	if i.busy || i.shellRunning {
		i.statusErr = "busy — wait for the current turn to finish before running a shell command"
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if parent == nil {
		parent = i.runCtx
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	i.busy = true
	i.shellRunning = true
	i.shellLive = "$ " + cmd + "\n\n"
	i.cancelTurn = cancel
	i.statusErr = ""
	i.statusOK = ""
	i.spin.Start()
	i.activity = agentActivity{activity: activity{kind: activityRunningShellCommand}}
	// Clear stale extension notes the same way a new turn would so the
	// screen doesn't accumulate transient state.
	i.extNotes = nil
	i.scrollOffset = 0
	i.parkedTurn = 0
	i.parkedTotal = 0
	i.helpBlock = nil
	sandbox := i.cfg.Sandbox
	cwd := i.cfg.CWD
	i.mu.Unlock()
	i.invalidate()

	go func() {
		defer cancel()
		raw, _ := json.Marshal(map[string]any{
			"command": cmd,
			"timeout": ShellEscapeTimeoutSeconds,
		})
		bash := &tools.BashTool{CWD: cwd, Sandbox: sandbox}
		progress := func(chunk string) {
			i.mu.Lock()
			i.shellLive += chunk
			i.mu.Unlock()
			i.invalidate()
		}
		res, err := bash.Execute(ctx, raw, progress)

		var out string
		if err != nil {
			out = "$ " + cmd + "\n\n" + err.Error() + "\n\n[error]"
		} else {
			for _, c := range res.Content {
				if tb, ok := c.(provider.TextBlock); ok {
					out += tb.Text
				}
			}
		}
		cancelled := ctx.Err() != nil
		failed := err != nil || res.IsError || cancelled
		if cancelled {
			out += "\n\n[cancelled]"
		}

		if i.agent != nil {
			i.agent.AppendUserContext(out, map[string]string{shellEscapeMetaKey: "true"})
		}

		i.mu.Lock()
		i.shellRunning = false
		i.shellLive = ""
		i.busy = false
		i.cancelTurn = nil
		pendingIdleWork := i.takePendingIdleWorkLocked()
		awaitingPre := i.awaitingStartupPre
		if failed {
			if cancelled {
				i.statusErr = "shell command cancelled"
			} else {
				i.statusErr = "shell command failed"
			}
			i.statusOK = ""
		} else {
			i.statusOK = "shell command finished"
			i.statusErr = ""
		}
		i.mu.Unlock()
		runPendingIdleWork(pendingIdleWork)
		i.invalidate()
		if awaitingPre {
			i.completeStartupPre()
		}
	}()
}
func (i *Interactive) startTurn(parent context.Context, prompt string) {
	i.startTurnWithImages(parent, prompt, nil)
}
func (i *Interactive) startTurnWithImages(parent context.Context, prompt string, images []provider.ImageBlock) {
	i.startTurnRequest(parent, prompt, images, false, false)
}
func (i *Interactive) startTurnRequest(parent context.Context, prompt string, images []provider.ImageBlock, continueExisting, overflowRecoveryAttempted bool) {
	if i.agent == nil {
		// Text startup pre cannot run without credentials; continue so
		// deferred InitialInput (pre-fill or auto-submit) still applies.
		i.mu.Lock()
		awaitingPre := i.awaitingStartupPre
		i.mu.Unlock()
		if awaitingPre {
			i.completeStartupPre()
		}
		return
	}
	// Pre-turn safety: if the most recent context measurement is
	// already past the auto-compact threshold, condense before
	// sending so the next outbound request stays under the limit.
	// The condense flow re-fires the user's queued prompt for us, so
	// we just hand it off and exit.
	i.mu.Lock()
	var resetHandoff json.RawMessage
	var persistResetHandoff bool
	if !continueExisting {
		resetHandoff, persistResetHandoff = i.resetCompactContinuationLocked()
	}
	needsPreCompact := !overflowRecoveryAttempted && !i.autoCompacting && i.shouldAutoCompactLocked()
	if needsPreCompact {
		// Reserve the compaction hand-off before releasing i.mu. A completion
		// update arriving in the small gap before runCompact acquires the
		// mutex must join the host queue rather than starting a competing turn.
		i.busy = true
		i.compacting = true
		i.autoCompacting = true
		if continueExisting {
			i.continueAfterCompact = true
		} else {
			i.pendingCompactPrompt = prompt
			i.pendingCompactImages = append([]provider.ImageBlock(nil), images...)
			i.hasPendingCompactPrompt = true
		}
		i.statusErr = ""
		i.extNotes = append(i.extNotes, autoCompactNoteLine(i.cfg.Theme, "context near limit — condensing history before sending..."))
		i.pendingPostCompactNote = "context auto-compacted; sending your last message"
		i.mu.Unlock()
		if persistResetHandoff {
			i.persistCompactHandoff(resetHandoff)
		}
		i.invalidate()
		i.runCompact(parent, compactContinuationRequest{origin: compactOriginPreTurnThreshold})
		return
	}
	i.mu.Unlock()
	if persistResetHandoff {
		i.persistCompactHandoff(resetHandoff)
	}

	ctx, cancel := context.WithCancel(parent)
	i.mu.Lock()
	i.busy = true
	i.spin.Start()
	i.activity = newAgentActivity(i.cfg.Provider, i.cfg.Model)
	i.cancelTurn = cancel
	i.statusErr = ""
	i.statusOK = ""
	i.streaming.Reset()
	i.streamOn = true
	i.pendingAlert = nil
	i.toolCalls = map[string]*tui.ToolCallView{}
	i.toolOrder = nil
	i.toolGate = map[string]int{}
	i.extNotes = nil   // ext notes are one-shot; a new prompt clears them
	i.scrollOffset = 0 // jump back to the bottom on new turn
	// Lift the resume tail cap once the user starts interacting. The
	// cap is purely a first-paint optimization (don't markdown the
	// whole history before showing anything). Keeping it active during
	// a turn makes the rendered chat a sliding window: appended
	// messages push older ones off the TOP of the buffer, which the
	// renderer must treat as a change above the viewport and repaint
	// fully, snapping the terminal's native scrollback to the bottom on
	// every streamed chunk. A fresh session has no cap (append-only),
	// which is why the jump only shows up in resumed sessions. Dropping
	// the cap here makes resumed turns append-only too.
	i.view.TailLimit = 0
	// Reset the auto-follow baseline so the very next render at
	// interactive.go:1053 doesn't see a synthetic shrink between
	// "last frame had the previous turn's tool overlay" and
	// "this frame had it cleared above". Without this, the guard
	// reads delta = -(rows in cleared overlay) and decrements
	// scrollOffset, which on terminals that mirror zut's pane
	// scroll into the host scrollbar visibly yanks the viewport.
	// See autofollow_shrink_test.go for the exact arithmetic.
	i.prevChatLen = 0
	i.prevChatCols = 0
	i.parkedTurn = 0 // starting a turn clears the /jump parked state
	i.parkedTotal = 0
	i.helpBlock = nil // hide the help block once the user asks something
	i.mu.Unlock()
	i.invalidate()

	var (
		lastStop    provider.StopReason
		lastTurnErr error
	)
	sink := func(ev core.AgentEvent) {
		if e, ok := ev.(core.EvTurnEnd); ok {
			lastStop = e.Stop
			lastTurnErr = e.Err
		}
		i.handleEventForPresentation(ev)
	}

	releaseCompletionHold := i.beginCompletionDeliveryHold()
	go func() {
		defer releaseCompletionHold()
		var err error
		if continueExisting {
			err = i.agent.Continue(ctx, sink)
		} else {
			err = i.agent.Prompt(ctx, prompt, images, sink)
		}
		i.mu.Lock()
		// Keep busy asserted through final cleanup and queue selection. A
		// watcher may enqueue a completion summary concurrently with the
		// provider's final event; publishing idle before inspecting queues can
		// strand that summary in the core agent queue or start an overlapping
		// turn.
		// Don't touch streamPending / streamFlushPending here — the
		// pacer may still be draining the final deltas and needs to
		// paint them even though Prompt has returned. It will reset
		// streamOn on its own once the buffer empties.
		if len(i.streamPending) == 0 {
			i.streamOn = false
		}
		i.cancelTurn = nil
		pendingIdleWork := i.takePendingIdleWorkLocked()
		if err != nil && ctx.Err() == nil {
			i.statusErr = err.Error()
		}
		// Decide whether to offer a model rescue picker for recoverable
		// provider failures (auth/rate/temporary). The picker opens after
		// the mutex is released so it can take its own locks freely.
		var (
			offer       bool
			rescueWhy   string
			rescueImgs  []provider.ImageBlock
			rescueModel string
			rescueProv  string
			rescueFprov string
		)
		if err != nil && ctx.Err() == nil {
			if ok, reason := classifyRescueError(err); ok {
				offer = true
				rescueWhy = reason
				rescueImgs = images
				rescueModel = i.cfg.Model
				rescueProv = i.cfg.Provider
				rescueFprov = extractFailedProvider(err)
				if rescueFprov == "" {
					rescueFprov = i.cfg.Provider
				}
				// Suppress the red banner — the rescue dialog already
				// surfaces the failure.
				i.statusErr = ""
			}
		}
		// Detect responses that reject the current context, either as an
		// HTTP 413 payload limit or a model context-window error. Token-
		// based auto-compact can miss both when metadata is stale or the
		// limit is measured in raw bytes. Compact once, then continue the
		// user message already present in the transcript.
		contextOverflow := err != nil && ctx.Err() == nil && provider.IsContextOverflowError(err)
		recoverContextOverflow := contextOverflow && !overflowRecoveryAttempted
		if recoverContextOverflow {
			i.statusErr = ""
			i.continueAfterCompact = true
			i.extNotes = append(i.extNotes, autoCompactNoteLine(i.cfg.Theme, "request was too large. condensing history before retrying ..."))
			i.pendingPostCompactNote = "context auto-compacted; retrying your last message"
		}
		// Persist the assistant's reply (and every tool row before
		// it) to the session file while the turn memory is hot.
		// Without this, WriteNewTranscript only fires at zut exit,
		// meaning a crash or ungraceful kill drops the whole
		// conversation. FlushSession is idempotent (it advances the
		// baseline so subsequent flushes only write new rows).
		flush := i.cfg.FlushSession
		i.mu.Unlock()
		runPendingIdleWork(pendingIdleWork)
		if flush != nil {
			flush()
		}
		terminalGoalError := ctx.Err() == nil && !offer && !recoverContextOverflow && (err != nil || lastTurnErr != nil || lastStop == provider.StopError)
		if terminalGoalError {
			i.updateActiveGoal(core.GoalBlocked, "turn ended with an error")
		}
		i.mu.Lock()
		awaitingPre := i.awaitingStartupPre
		// A newer explicit prompt may have cleared the handoff while the
		// completed turn was being flushed. Re-read it under the mutex before
		// deciding whether this continuation may spend another rescue attempt.
		statusRescueActive := i.compactContinuation.reason == compactContinuationStatusRescue
		// Pop the next queued message, if any, and relaunch.
		var next string
		var hasNext bool
		if !awaitingPre && len(i.queued) > 0 && ctx.Err() == nil && err == nil {
			next, i.queued = i.queued[0], i.queued[1:]
			hasNext = true
		}
		// If the turn was cancelled or errored, drop the queue so the
		// user isn't bombarded with stale messages after an interrupt.
		if ctx.Err() != nil || (err != nil && !recoverContextOverflow) {
			i.queued = nil
			if i.agent != nil {
				i.agent.DrainQueuedMessages()
			}
		}
		// Decide whether the next thing to do is an auto-compaction.
		// Only fires when the turn completed cleanly AND no host-side
		// or agent-side queued messages are waiting (otherwise a queued
		// message would race the condense).
		agentQueued := 0
		if i.agent != nil {
			agentQueued = i.agent.QueuedMessageCount()
		}
		continueQueued := !awaitingPre && !hasNext && agentQueued > 0 && err == nil && ctx.Err() == nil
		shouldAutoCompact := !awaitingPre && !hasNext && agentQueued == 0 && err == nil && ctx.Err() == nil && i.shouldAutoCompactLocked()
		continueStatusRescue := false
		var handoff json.RawMessage
		var persistHandoff bool
		if statusRescueActive && i.agent != nil && !awaitingPre && !hasNext && !continueQueued && !shouldAutoCompact && err == nil && ctx.Err() == nil && lastStop == provider.StopEnd && lastTurnErr == nil {
			followUpMessages := i.agent.Messages()
			reason := classifyCompactionContinuation(compactOriginManual, true, lastStop, lastTurnErr, followUpMessages)
			if reason == compactContinuationStatusRescue && i.compactContinuation.rescueAttempts < maxStatusRescueContinuations {
				handoff, persistHandoff = i.setCompactContinuationLocked(compactContinuationState{
					reason:         compactContinuationStatusRescue,
					rescueAttempts: i.compactContinuation.rescueAttempts + 1,
				})
				continueStatusRescue = true
			} else {
				handoff, persistHandoff = i.resetCompactContinuationLocked()
			}
		}
		if !continueStatusRescue && (i.agent == nil || ctx.Err() != nil || err != nil || awaitingPre || hasNext || continueQueued || offer || recoverContextOverflow || (!shouldAutoCompact && !statusRescueActive)) {
			handoff, persistHandoff = i.resetCompactContinuationLocked()
		}
		// The agent run can finish before the paced final text reaches the
		// transcript. A compaction replaces that transcript, so it must never
		// race the still-live stream frame; otherwise stale deltas can repaint
		// after the replacement and corrupt the scrollback renderer's model.
		if recoverContextOverflow || shouldAutoCompact {
			i.resetStreamingStateLocked()
		}
		alertReason := mainAlertReason(ctx, err, lastTurnErr, lastStop, awaitingPre, hasNext || agentQueued > 0, offer, recoverContextOverflow, shouldAutoCompact)
		i.busy = hasNext || continueQueued || continueStatusRescue || recoverContextOverflow || shouldAutoCompact
		i.mu.Unlock()
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
		if alertReason != "" {
			i.scheduleMainAlert(alertReason)
		}
		i.invalidate()
		parent := i.runCtx
		if parent == nil {
			parent = context.Background()
		}
		if awaitingPre {
			i.completeStartupPre()
			return
		}
		switch {
		case hasNext:
			i.startTurn(parent, next)
		case continueQueued:
			i.startTurnRequest(parent, "", nil, true, false)
		case continueStatusRescue:
			i.startAutoCompactContinuation(parent)
		case offer:
			i.openRescueDialog(rescueProv, rescueFprov, rescueModel, rescueWhy, prompt, rescueImgs)
		case recoverContextOverflow:
			i.runCompact(parent, compactContinuationRequest{origin: compactOriginRecovery})
		case shouldAutoCompact:
			i.runCompact(parent, compactContinuationRequest{
				origin:    compactOriginAfterTurnThreshold,
				force:     lastStop == provider.StopLength,
				lastStop:  lastStop,
				turnError: lastTurnErr,
			})
		}
	}()
}
func mainAlertReason(ctx context.Context, err, turnErr error, stop provider.StopReason, awaitingPre, hasNext, rescue, recovering, autoCompacting bool) string {
	if awaitingPre || ctx.Err() != nil || hasNext || recovering || autoCompacting {
		return ""
	}
	if rescue {
		return "rescue_required"
	}
	if stop == provider.StopAborted {
		return ""
	}
	if err != nil || turnErr != nil || stop == provider.StopError {
		return "agent_error"
	}
	if stop == provider.StopLength {
		return "response_truncated"
	}
	return "agent_done"
}
func (i *Interactive) openRescueDialog(activeProvider, failedProvider, failedModel, reason, prompt string, images []provider.ImageBlock) {
	if i.rescueDialog == nil {
		return
	}
	loggedIn := []string{}
	if i.cfg.LoggedInProviders != nil {
		loggedIn = i.cfg.LoggedInProviders()
	}
	fprov := failedProvider
	if fprov == "" {
		fprov = activeProvider
	}
	i.mu.Lock()
	i.pendingRescuePrompt = prompt
	i.pendingRescueImages = images
	i.mu.Unlock()
	i.rescueDialog.Open(failedModel, loggedIn, fprov, failedModel, reason, prompt)
	i.invalidate()
}
func (i *Interactive) applyRescueSelection(prov, model, prompt string) {
	if model == "" {
		return
	}
	i.applyRescueModelSelection(prov, model)
	i.mu.Lock()
	images := i.pendingRescueImages
	if prompt == "" {
		prompt = i.pendingRescuePrompt
	}
	i.pendingRescuePrompt = ""
	i.pendingRescueImages = nil
	i.mu.Unlock()
	parent := i.runCtx
	if parent == nil {
		parent = context.Background()
	}
	i.startTurnWithImages(parent, prompt, images)
}
func stripAutoCompactNotes(notes []string) []string {
	if len(notes) == 0 {
		return notes
	}
	out := notes[:0]
	for _, n := range notes {
		if strings.Contains(n, "condensing history") {
			continue
		}
		out = append(out, n)
	}
	return out
}
func autoCompactNoteLine(th tui.Theme, msg string) string {
	return "  " + th.FGColor(th.Warning, "⚠ "+msg)
}
func NormalizeAutoCompactThreshold(threshold *int) int {
	const defaultAutoCompactThreshold = 85
	if threshold == nil {
		return defaultAutoCompactThreshold
	}
	switch *threshold {
	case 0, 70, 80, 85, 90, 95:
		return *threshold
	default:
		return defaultAutoCompactThreshold
	}
}
func ShouldAutoCompact(inputTokens, contextWindow, thresholdPercent int) bool {
	if inputTokens <= 0 || contextWindow <= 0 || thresholdPercent <= 0 {
		return false
	}
	return float64(inputTokens)/float64(contextWindow) >= float64(thresholdPercent)/100
}
func classifyCompactionContinuation(origin compactContinuationOrigin, statusRescueActive bool, stop provider.StopReason, turnErr error, msgs []provider.Message) compactContinuationReason {
	if origin != compactOriginManual && structuralCompactionContinuation(msgs) {
		return compactContinuationStructuralTail
	}
	if (origin != compactOriginAfterTurnThreshold && !statusRescueActive) || stop != provider.StopEnd || turnErr != nil {
		return compactContinuationNone
	}
	if likelyForwardWorkStatus(msgs) {
		return compactContinuationStatusRescue
	}
	return compactContinuationNone
}
func structuralCompactionContinuation(msgs []provider.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	if last.Role != provider.RoleAssistant {
		return true
	}
	hasText := false
	for _, content := range last.Content {
		switch block := content.(type) {
		case provider.ToolCallBlock:
			return true
		case provider.TextBlock:
			if strings.TrimSpace(block.Text) != "" {
				hasText = true
			}
		case provider.ReasoningBlock:
			// Reasoning without a visible answer is not terminal.
		default:
			return true
		}
	}
	return !hasText
}
func likelyForwardWorkStatus(msgs []provider.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	if last.Role != provider.RoleAssistant {
		return false
	}
	var text strings.Builder
	for _, content := range last.Content {
		block, ok := content.(provider.TextBlock)
		if !ok {
			if _, hasTool := content.(provider.ToolCallBlock); hasTool {
				return false
			}
			continue
		}
		if strings.TrimSpace(block.Text) == "" {
			continue
		}
		if text.Len() > 0 {
			text.WriteByte(' ')
		}
		text.WriteString(block.Text)
	}
	if text.Len() == 0 {
		return false
	}
	normalized := strings.ToLower(strings.Join(strings.Fields(text.String()), " "))
	normalized = strings.NewReplacer("’", "'", "‘", "'", "`", "'").Replace(normalized)
	for _, phrase := range []string{
		"next i will",
		"next, i will",
		"i'll now",
		"i will now",
		"i am going to",
		"i need to continue",
		"then i will",
		"then, i will",
		"i will proceed",
	} {
		if containsFutureWorkPhrase(normalized, phrase) {
			return true
		}
	}
	return false
}
func containsFutureWorkPhrase(text, phrase string) bool {
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], phrase)
		if index < 0 {
			return false
		}
		index += offset
		end := index + len(phrase)
		if !wordRuneBefore(text, index) && !wordRuneAfter(text, end) {
			return true
		}
		offset = end
	}
	return false
}
func wordRuneBefore(text string, index int) bool {
	if index == 0 {
		return false
	}
	rune_, _ := utf8.DecodeLastRuneInString(text[:index])
	return unicode.IsLetter(rune_) || unicode.IsDigit(rune_) || rune_ == '_'
}
func wordRuneAfter(text string, index int) bool {
	if index == len(text) {
		return false
	}
	rune_, _ := utf8.DecodeRuneInString(text[index:])
	return unicode.IsLetter(rune_) || unicode.IsDigit(rune_) || rune_ == '_'
}
func (i *Interactive) shouldAutoCompactLocked() bool {
	if i.agent == nil {
		return false
	}
	if i.autoCompacting {
		return false
	}
	m, err := provider.FindModel(i.cfg.Provider, i.cfg.Model)
	if err != nil || m.ContextWindow <= 0 {
		return false
	}
	threshold := NormalizeAutoCompactThreshold(i.cfg.AutoCompactThreshold)
	return ShouldAutoCompact(i.lastCtxInput, m.ContextWindow, threshold)
}
