package modes

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
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

// portSlideBackQueued retrieves a draft off-loop, optionally aborting only after
// retrieval so cancellation cannot discard the draft first.
func (i *Interactive) portSlideBackQueued(ctx context.Context, abort bool) {
	if i.portRecallPending {
		if abort {
			i.portAbort(ctx)
		}
		return
	}
	if n := len(i.portRecalled); n > 0 {
		i.restoreQueuedMessageToEditor(i.portRecalled[n-1])
		i.portRecalled = i.portRecalled[:n-1]
		i.invalidate()
		if abort {
			i.portAbort(ctx)
		}
		return
	}
	i.portRecallPending = true
	text := i.ed.Value()
	images := slices.Clone(i.clipboardImages)
	port := i.SessionPort()
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		callCtx, cancel := context.WithTimeout(ctx, i.sessionCallTimeout())
		in, err := port.SlideBackQueued(callCtx)
		cancel()
		if err != nil && !errors.Is(err, session.ErrUnknownRequest) {
			i.ReportError(fmt.Errorf("slide back queued: %w", err))
		}
		// Schedule the draft before aborting, but retain the pending flag until
		// both calls finish so another recall cannot overtake this one.
		select {
		case i.loopCalls <- func() {
			if err == nil && (in.Text != "" || len(in.Images) > 0) {
				message := core.QueuedMessage{Text: in.Text, Images: in.Images}
				if i.ed.Value() == text && reflect.DeepEqual(i.clipboardImages, images) {
					i.restoreQueuedMessageToEditor(message)
				} else {
					i.portRecalled = append(i.portRecalled, message)
					i.mu.Lock()
					i.statusOK = "recalled draft saved; Alt+Up to restore"
					i.mu.Unlock()
				}
				i.invalidate()
			}
		}:
		case <-ctx.Done():
		}
		if abort {
			callCtx, cancel := context.WithTimeout(ctx, i.sessionCallTimeout())
			if err := port.Abort(callCtx); err != nil {
				i.ReportError(fmt.Errorf("abort: %w", err))
			}
			cancel()
		}
		select {
		case i.loopCalls <- func() { i.portRecallPending = false }:
		case <-ctx.Done():
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
