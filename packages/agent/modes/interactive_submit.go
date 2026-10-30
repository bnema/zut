package modes

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// Submit sends user input to the attached session (or runs a local shell escape).
func (i *Interactive) Submit(text string) {
	if cmd, ok := shellEscapeCommand(text); ok {
		i.startShellEscape(i.runCtx, cmd)
		return
	}
	parent := i.runCtx
	if parent == nil {
		parent = context.Background()
	}
	i.portSubmit(parent, text, nil)
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
	i.portSubmitInput(ctx, session.Input{Text: text, Images: images, Mode: session.SubmitModeAuto, Origin: session.InputOriginUser})
}

func (i *Interactive) portSubmitInput(ctx context.Context, in session.Input) {
	port := i.SessionPort()
	if port == nil {
		i.ReportError(errors.New("submit: no session attached"))
		return
	}
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

// portHasUserQueued reports whether the attached session queue holds user input.
func (i *Interactive) portHasUserQueued() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, q := range i.portQueue {
		if !q.Input.HostEvent {
			return true
		}
	}
	return false
}

// portClearQueuedUser is the port-mode Ctrl+C queue clear. Host events stay
// queued in the session; the QueueEvent it publishes refreshes portQueue.
// Only after the RPC succeeds are pinned-skill preloads carried by the dropped
// drafts released, as on the legacy path. An older daemon gets a status hint
// and is remembered for this session so later presses reach the exit menu.
func (i *Interactive) portClearQueuedUser(ctx context.Context) {
	port := i.SessionPort()
	gen := i.sessionGen
	i.mu.Lock()
	var texts []string
	for _, q := range i.portQueue {
		if !q.Input.HostEvent {
			texts = append(texts, q.Input.Text)
		}
	}
	i.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		callCtx, cancel := context.WithTimeout(ctx, i.sessionCallTimeout())
		err := port.ClearQueuedUser(callCtx)
		cancel()
		select {
		case i.loopCalls <- func() {
			if gen != i.sessionGen {
				return
			}
			i.mu.Lock()
			switch {
			case err == nil:
				for _, text := range texts {
					i.skillPins.recall(text)
				}
				i.statusOK, i.statusErr = "queue cleared", ""
			case errors.Is(err, session.ErrUnknownRequest):
				i.portClearUnsupportedGen = gen + 1
				i.statusOK, i.statusErr = "", "daemon too old to clear the queue; run \"zut daemon restart\""
			default:
				i.statusOK, i.statusErr = "", fmt.Sprintf("clear queue: %v", err)
			}
			i.mu.Unlock()
			i.invalidate()
		}:
		case <-ctx.Done():
		}
	}()
}

type portRecallOperation struct {
	gen            uint64
	port           session.Port
	abortRequested bool
}

// portSlideBackQueued runs on the input loop. The worker retrieves before
// aborting, and all editor changes are applied by the loop for this session only.
func (i *Interactive) portSlideBackQueued(ctx context.Context, abort bool) {
	if op := i.portRecall; op != nil && op.gen == i.sessionGen {
		if abort {
			op.abortRequested = true
		}
		return
	}
	op := &portRecallOperation{gen: i.sessionGen, port: i.SessionPort(), abortRequested: abort}
	i.portRecall = op
	text := i.ed.Value()
	images := slices.Clone(i.clipboardImages)
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		callCtx, cancel := context.WithTimeout(ctx, i.sessionCallTimeout())
		in, err := op.port.SlideBackQueued(callCtx)
		cancel()
		select {
		case i.loopCalls <- func() {
			if op.gen == i.sessionGen && i.portRecall == op {
				if err != nil && !errors.Is(err, session.ErrUnknownRequest) {
					i.ReportError(fmt.Errorf("slide back queued: %w", err))
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
			}
			if op.abortRequested {
				go func() {
					callCtx, cancel := context.WithTimeout(ctx, i.sessionCallTimeout())
					defer cancel()
					if err := op.port.Abort(callCtx); err != nil {
						i.ReportError(fmt.Errorf("abort: %w", err))
					}
				}()
			}
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
