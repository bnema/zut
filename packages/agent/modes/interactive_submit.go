package modes

import "context"

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
	i.mu.Lock()
	awaitingStartupPre := i.awaitingStartupPre
	i.mu.Unlock()
	if !awaitingStartupPre {
		i.maybeStartSessionTitle(parent, text)
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
