package session

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

const detachedActionReason = "user detached: action recorded for review"
const detachedQuestionReason = "user detached: question recorded for review"

type pendingResult struct {
	decision core.ConfirmDecision
	answers  []tools.QuestionAnswer
	err      error
}
type pendingEntry struct {
	request PendingRequest
	result  chan pendingResult
}
type confirmState struct {
	dispatched map[string]bool
	pending    map[string]*pendingEntry
	deferred   []DeferredAction
}

// initConfirm hydrates review state before the first actor projection.
func (c *Controller) initConfirm() {
	if c.deps.DeferredStore != nil {
		c.confirm.deferred = confirmCopy(c.deps.DeferredStore.List())
	}
}

// confirmCopy keeps request, event and snapshot payloads independently owned.
func confirmCopy[T any](v T) T {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err = json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}
func (s *confirmState) snapshotInto(out *Snapshot) {
	for _, p := range s.pending {
		out.Pending = append(out.Pending, confirmCopy(p.request))
	}
	out.Deferred = confirmCopy(s.deferred)
}

// registerPending runs on the actor; callers wait outside it.
func (c *Controller) registerPending(req PendingRequest) *pendingEntry {
	req = confirmCopy(req)
	// Caller IDs are never registry keys. Retry any random collision before mutation.
	for {
		req.ID = "pending-" + rand.Text()
		if c.confirm.pending[req.ID] == nil {
			break
		}
	}
	req.CreatedAt = c.deps.Clock()
	p := &pendingEntry{req, make(chan pendingResult, 1)}
	if c.confirm.pending == nil {
		c.confirm.pending = make(map[string]*pendingEntry)
	}
	c.confirm.pending[req.ID] = p
	c.publish(EvPending, PendingEvent{Pending: confirmCopy(req)})
	return p
}
func (c *Controller) finishPending(id string, result pendingResult) {
	p := c.confirm.pending[id]
	if p == nil {
		return
	}
	delete(c.confirm.pending, id)
	p.result <- result
	c.publish(EvPendingResolved, PendingResolvedEvent{ID: id, Outcome: result.decision.Reason})
}
func (c *Controller) waitPending(ctx context.Context, p *pendingEntry) pendingResult {
	select {
	case r := <-p.result:
		return r
	case <-ctx.Done():
		_ = c.call(context.Background(), func() {
			c.finishPending(p.request.ID, pendingResult{err: ctx.Err(), decision: core.ConfirmDecision{Reason: ctx.Err().Error()}})
		})
		return pendingResult{err: ctx.Err(), decision: core.ConfirmDecision{Reason: ctx.Err().Error()}}
	case <-c.done:
		return pendingResult{err: ErrClosed, decision: core.ConfirmDecision{Reason: ErrClosed.Error()}}
	}
}

// AnswerConfirmation resolves one confirmation, preserving gate memory flags.
func (c *Controller) AnswerConfirmation(ctx context.Context, id string, d core.ConfirmDecision) error {
	var result error
	err := c.call(ctx, func() {
		p := c.confirm.pending[id]
		if p == nil || p.request.Kind != "confirm" {
			result = ErrUnknownRequest
			return
		}
		c.finishPending(id, pendingResult{decision: d})
	})
	if err != nil {
		return err
	}
	return result
}

// AnswerQuestion resolves question and rescue requests in the shared registry.
func (c *Controller) AnswerQuestion(ctx context.Context, id string, answers []tools.QuestionAnswer) error {
	var result error
	err := c.call(ctx, func() {
		p := c.confirm.pending[id]
		if p == nil || p.request.Kind == "confirm" {
			result = ErrUnknownRequest
			return
		}
		if answers == nil {
			// nil is the Port's decline: questions get the TUI decline result,
			// rescues count as cancelled.
			c.finishPending(id, pendingResult{err: tools.ErrQuestionsDeclined})
			return
		}
		c.finishPending(id, pendingResult{answers: confirmCopy(answers)})
	})
	if err != nil {
		return err
	}
	return result
}

// awaitPending serves question and rescue callers. Detached callers never block.
func (c *Controller) awaitPending(ctx context.Context, req PendingRequest) ([]tools.QuestionAnswer, error) {
	var p *pendingEntry
	var result error
	err := c.call(context.Background(), func() {
		if result = ctx.Err(); result != nil {
			return
		}
		if c.attach != AttachStateAttached {
			result = ErrNotAttached
			return
		}
		p = c.registerPending(req)
	})
	if err != nil {
		return nil, err
	}
	if result != nil {
		return nil, result
	}
	r := c.waitPending(ctx, p)
	return r.answers, r.err
}

type controllerConfirmer struct{ c *Controller }

func (x controllerConfirmer) Confirm(name, preview string) core.ConfirmDecision {
	return x.ConfirmToolCall(core.ToolCallConfirmation{Name: name, Summary: preview})
}
func (x controllerConfirmer) ConfirmToolCall(call core.ToolCallConfirmation) core.ConfirmDecision {
	c := x.c
	var p *pendingEntry
	var result core.ConfirmDecision
	err := c.call(context.Background(), func() {
		if c.deps.Extensions != nil {
			c.deps.Extensions.EmitEvent(extproto.EventFromHost{Event: "tool_confirmation_requested", ToolID: call.ID, ToolName: call.Name, ToolPreview: call.Summary})
		}
		req := PendingRequest{Kind: "confirm", Confirm: &call}
		if c.attach != AttachStateAttached {
			c.recordDeferred(req)
			result.Reason = detachedActionReason
			return
		}
		p = c.registerPending(req)
	})
	if err != nil {
		return core.ConfirmDecision{Reason: err.Error()}
	}
	if p == nil {
		return result
	}
	return c.waitPending(context.Background(), p).decision
}
func (c *Controller) confirmer() core.ToolCallConfirmer { return controllerConfirmer{c} }

type controllerQuestionAsker struct{ c *Controller }

func (x controllerQuestionAsker) AskQuestions(ctx context.Context, q []tools.Question) ([]tools.QuestionAnswer, error) {
	// Register and detach-check atomically, so a concurrent detach cannot lose review data.
	c := x.c
	var p *pendingEntry
	var detached bool
	var canceled error
	err := c.call(context.Background(), func() {
		if canceled = ctx.Err(); canceled != nil {
			return
		}
		req := PendingRequest{Kind: "question", Questions: q}
		if c.attach != AttachStateAttached {
			detached = true
			c.recordDeferred(req)
			return
		}
		p = c.registerPending(req)
	})
	if err != nil {
		return nil, err
	}
	if canceled != nil {
		return nil, canceled
	}
	if detached {
		return nil, errors.New(detachedQuestionReason)
	}
	r := c.waitPending(ctx, p)
	return r.answers, r.err
}
func (c *Controller) questionAsker() tools.QuestionAsker { return controllerQuestionAsker{c} }

// denyAllPending is actor-only. Detach records review actions; abort only cancels.
func (c *Controller) denyAllPending(reason string) {
	detached := c.attach == AttachStateDetached || strings.Contains(reason, "detach") || strings.Contains(reason, "disconnect")
	for id, p := range c.confirm.pending {
		r := reason
		if detached && p.request.Kind != "rescue" {
			c.recordDeferred(p.request)
			r = detachedActionReason
			if p.request.Kind == "question" {
				r = detachedQuestionReason
			}
		}
		c.finishPending(id, pendingResult{decision: core.ConfirmDecision{Reason: r}, err: errors.New(r)})
	}
}
func (c *Controller) recordDeferred(req PendingRequest) {
	// Random IDs remain unique across daemon restarts and independent stores.
	d := DeferredAction{ID: "deferred-" + rand.Text(), Kind: req.Kind, Questions: confirmCopy(req.Questions), At: c.deps.Clock()}
	if req.Confirm != nil {
		d.Tool = req.Confirm.Name
		d.Summary = req.Confirm.Summary
	}
	if c.deps.DeferredStore != nil {
		if err := c.deps.DeferredStore.Append(d); err != nil {
			c.publish(EvNote, NoteEvent{Note: Note{Level: "error", Text: "Could not persist deferred action; retained in memory for review."}})
			if c.deps.Logger != nil {
				c.deps.Logger.Error("persist deferred action", "error", err)
			}
		}
	}
	c.confirm.deferred = append(c.confirm.deferred, d)
	c.publish(EvDeferred, DeferredEvent{Deferred: confirmCopy(d)})
}

// ResolveDeferred dismisses an action or queues explicit user approval for retry.
func (c *Controller) ResolveDeferred(ctx context.Context, id, action string) error {
	var result error
	err := c.call(ctx, func() {
		if action != "dismiss" && action != "send_to_agent" {
			result = fmt.Errorf("unknown deferred action %q", action)
			return
		}
		list := c.confirm.deferred
		for _, d := range list {
			if d.ID != id {
				continue
			}
			if action == "send_to_agent" && !c.confirm.dispatched[id] {
				in := Input{Origin: InputOriginUser, Text: fmt.Sprintf("The user reviewed a previously denied action and approves it: %s — %s. Retry it if still relevant.", d.Tool, d.Summary)}
				if c.turnSeam.isBusy() {
					c.turnSeam.enqueue(in)
				} else if result = c.turnSeam.startTurn(in); result != nil {
					return
				}

				if c.confirm.dispatched == nil {
					c.confirm.dispatched = make(map[string]bool)
				}
				c.confirm.dispatched[id] = true
			}
			if c.deps.DeferredStore != nil {
				if result = c.deps.DeferredStore.Remove(id); result != nil && !errors.Is(result, ErrUnknownRequest) {
					c.publish(EvNote, NoteEvent{Note: Note{Level: "error", Text: "Could not remove deferred action from storage."}})
					return
				}
				result = nil
			}
			for i, v := range c.confirm.deferred {
				if v.ID == id {
					c.confirm.deferred = append(c.confirm.deferred[:i], c.confirm.deferred[i+1:]...)
					break
				}
			}
			c.publish(EvInfo, InfoEvent{})
			return
		}
		result = ErrUnknownRequest
	})
	if err != nil {
		return err
	}
	return result
}

// deferredCount is actor-only and includes actions whose persistence failed.
func (c *Controller) deferredCount() int { return len(c.confirm.deferred) }
