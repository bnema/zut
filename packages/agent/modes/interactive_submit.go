package modes

import (
	"context"
	"fmt"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/provider"
)

// Submit feeds text through the agent loop as if the user had typed it.
func (i *Interactive) Submit(text string) {
	if cmd, ok := shellEscapeCommand(text); ok {
		i.startShellEscape(i.runCtx, cmd)
		return
	}
	parent := i.runCtx
	if parent == nil {
		parent = context.Background()
	}
	if i.port != nil {
		i.portSubmit(parent, text, nil)
		return
	}
	i.mu.Lock()
	awaitingStartupPre := i.awaitingStartupPre
	i.mu.Unlock()
	if !awaitingStartupPre {
		i.maybeStartSessionTitle(parent, text)
		// Initial and deferred input are user prompts; the startup command
		// itself is not and leaves the pins pending.
		text = i.consumePinnedSkills(text)
	}
	i.startTurn(parent, text)
}

// completeStartupPre applies deferred InitialInput after entry.pre finishes.
// When AutoSubmitInitial was set, the deferred prompt is submitted; otherwise
// it only pre-fills the editor (CLI-supplied prompts).
func (i *Interactive) completeStartupPre() {
	i.mu.Lock()
	if !i.awaitingStartupPre {
		i.mu.Unlock()
		return
	}
	i.awaitingStartupPre = false
	deferred := i.deferredInitialInput
	auto := i.autoSubmitDeferred
	i.deferredInitialInput = ""
	i.autoSubmitDeferred = false
	onDone := i.cfg.OnStartupPreDone
	i.mu.Unlock()
	if onDone != nil {
		onDone()
	}
	i.startupPreDone <- startupPreResult{deferred: deferred, autoSubmit: auto}
	i.invalidate()
}

// applyStartupPreResult runs on the TUI event loop so the editor remains
// single-threaded. Input entered while resources were reloading wins over the
// deferred prefill rather than being overwritten.
func (i *Interactive) applyStartupPreResult(result startupPreResult) {
	// entry.pre may have added or removed skills; re-resolve the pending pins
	// without holding the interactive mutex.
	i.refreshPendingSkillPins()
	if result.deferred == "" {
		return
	}
	if result.autoSubmit {
		i.Submit(result.deferred)
		return
	}
	if i.ed.IsEmpty() {
		i.ed.SetValue(result.deferred)
	}
}

// portRunning reports whether the attached session is running a turn.
func (i *Interactive) portRunning() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.portInfo.Run == session.RunStateRunning
}

// portSubmit sends a user prompt to the session port without blocking the
// render loop. The resulting transcript arrives through session events.
func (i *Interactive) portSubmit(ctx context.Context, text string, images []provider.ImageBlock) {
	port := i.SessionPort()
	in := session.Input{Text: text, Images: images, Mode: session.SubmitModeAuto, Origin: session.InputOriginUser}
	go func() {
		if ctx == nil {
			ctx = context.Background()
		}
		callCtx, cancel := context.WithTimeout(ctx, i.sessionCallTimeout())
		defer cancel()
		if err := port.Submit(callCtx, in); err != nil {
			i.ReportError(fmt.Errorf("submit: %w", err))
		}
	}()
}

// portAbort asks the session port to abort the running turn off the render loop.
func (i *Interactive) portAbort(ctx context.Context) {
	port := i.SessionPort()
	go func() {
		if ctx == nil {
			ctx = context.Background()
		}
		callCtx, cancel := context.WithTimeout(ctx, i.sessionCallTimeout())
		defer cancel()
		if err := port.Abort(callCtx); err != nil {
			i.ReportError(fmt.Errorf("abort: %w", err))
		}
	}()
}
