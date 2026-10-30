package modes

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"

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

type portRecallOperation struct {
	gen            uint64
	abortRequested atomic.Bool
}

// portSlideBackQueued runs on the input loop. The worker retrieves before
// aborting, and all editor changes are applied by the loop for this session only.
func (i *Interactive) portSlideBackQueued(ctx context.Context, abort bool) {
	if op := i.portRecall; op != nil && op.gen == i.sessionGen {
		if abort {
			op.abortRequested.Store(true)
		}
		return
	}
	op := &portRecallOperation{gen: i.sessionGen}
	op.abortRequested.Store(abort)
	i.portRecall = op
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
		select {
		case i.loopCalls <- func() {
			if op.gen != i.sessionGen {
				if i.portRecall == op {
					i.portRecall = nil
				}
				return
			}
			if err == nil && (in.Text != "" || len(in.Images) > 0) {
				message := core.QueuedMessage{Text: in.Text, Images: in.Images}
				if i.ed.Value() == text && reflect.DeepEqual(i.clipboardImages, images) {
					i.restoreQueuedMessageToEditor(message)
				} else {
					i.appendRecalledMessageToEditor(message)
				}
				i.invalidate()
			}
		}:
		case <-ctx.Done():
		}
		if op.abortRequested.Load() {
			callCtx, cancel := context.WithTimeout(ctx, i.sessionCallTimeout())
			if err := port.Abort(callCtx); err != nil {
				i.ReportError(fmt.Errorf("abort: %w", err))
			}
			cancel()
		}
		select {
		case i.loopCalls <- func() {
			if i.portRecall == op {
				i.portRecall = nil
			}
		}:
		case <-ctx.Done():
		}
	}()
}

// appendRecalledMessageToEditor preserves current input and attachment markers.
func (i *Interactive) appendRecalledMessageToEditor(message core.QueuedMessage) {
	text := i.ed.Value()
	i.mu.Lock()
	recalled := i.skillPins.rawPrompt(message.Text)
	i.mu.Unlock()
	if text != "" {
		text += "\n"
	}
	text += recalled
	for _, image := range message.Images {
		n := 1
		marker := fmt.Sprintf("[clipboard image #%d]", n)
		for strings.Contains(text, marker) {
			n++
			marker = fmt.Sprintf("[clipboard image #%d]", n)
		}
		if text != "" {
			text += " "
		}
		text += marker
		i.clipboardImages = append(i.clipboardImages, clipboardImageAttachment{Marker: marker, Image: image})
	}
	i.ed.SetValue(text)
	i.inputHistoryIndex = -1
}
