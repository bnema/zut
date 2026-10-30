package session

import (
	"context"
	"time"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

// closeReasonDaemonStop is the reason used when the daemon context ends;
// it equals daemon.ReasonDaemonStop (session cannot import daemon).
const closeReasonDaemonStop = "daemon_stop"

// turnStopTimeout bounds how long close waits for a cancelled turn goroutine.
const turnStopTimeout = 5 * time.Second

// actorKey marks contexts handed to code running on the actor loop.
type actorKey struct{}

// OnActor reports whether ctx was handed out by c's actor loop (Do), i.e.
// whether the caller runs on the loop and must never wait on it.
func (c *Controller) OnActor(ctx context.Context) bool {
	owner, _ := ctx.Value(actorKey{}).(*Controller)
	// A cancelled actor context outlived its Do call: not on the actor.
	return owner == c && ctx.Err() == nil
}

// Do runs fn on the actor loop and waits for it. fn receives an actor-marked
// context, derived per call and cancelled when fn returns: Close(actx, …) and
// SetAttach(actx, …) inside fn do not wait on the actor. actx must not escape
// fn (stored or handed to goroutines); after fn returns it is cancelled and
// only marks code that is no longer on the actor.
func (c *Controller) Do(ctx context.Context, fn func(actx context.Context)) error {
	return c.call(ctx, func() {
		actx, cancel := context.WithCancel(c.actorBase)
		defer cancel()
		fn(actx)
	})
}

// Close ends the session: the running turn is cancelled and awaited (bounded),
// pending confirmations and questions are resolved as for a detach (D3),
// EvClosed{reason} is published, residents and extensions are shut down and
// the actor stops. It is idempotent; the first reason wins. With a ctx from
// the actor (OnActor) it only requests the close and returns; otherwise it
// waits for Done or ctx.
func (c *Controller) Close(ctx context.Context, reason string) error {
	c.requestClose(reason)
	if c.OnActor(ctx) {
		return nil
	}
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// requestClose asks the loop to stop with reason; it never waits and is safe
// from any goroutine, including the actor.
func (c *Controller) requestClose(reason string) {
	c.closeOnce.Do(func() {
		c.closeWhy = reason
		close(c.closeReq)
	})
}

// closeOnActor runs on the loop right before it exits. Turn callbacks post
// to the loop only through post, which fails fast once closeReq is closed, so
// the turn goroutine never needs the actor to finish.
func (c *Controller) closeOnActor() {
	reason := c.closeWhy
	// Resolve pending requests first so a detached session records them as
	// deferred (D3) before turn cancellation denies them. Close acts on actor
	// state directly (not via the cluster seams): it must not trigger goal
	// abort bookkeeping and must work whatever seam a test injected.
	c.denyAllPending("session closed: " + reason)
	if c.turn.cancel != nil {
		c.turn.cancel()
	}
	c.abortCompaction()
	c.waitBackground(turnStopTimeout)
	c.publish(EvClosed, ClosedEvent{Reason: reason})
	c.residentsShutdown()
	c.extShutdown()
}

// waitBackground waits, under one shared bound, for the latest turn and
// compaction goroutines to return after cancellation so assembly cleanup
// never races a running agent.
func (c *Controller) waitBackground(limit time.Duration) {
	t := time.NewTimer(limit)
	defer t.Stop()
	for name, done := range map[string]chan struct{}{"turn": c.turnDone, "compaction": c.compactDone} {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-t.C:
			if c.deps.Logger != nil {
				c.deps.Logger.Warn(name+" did not stop before close", "timeout", limit)
			}
			return
		}
	}
}

// SetAttach records the attach state (D2), publishes EvAttach and, on
// detach, resolves pending requests as detached (D3). With an actor context
// (OnActor) it applies inline; otherwise it posts to the actor and waits.
func (c *Controller) SetAttach(ctx context.Context, state AttachState, reason string) {
	if c.OnActor(ctx) {
		c.setAttachOnActor(state, reason)
		return
	}
	_ = c.call(context.WithoutCancel(ctx), func() { c.setAttachOnActor(state, reason) })
}

// setAttachOnActor is the actor-only form of SetAttach.
func (c *Controller) setAttachOnActor(state AttachState, reason string) {
	if c.attach == state {
		return
	}
	c.attach = state
	if state == AttachStateDetached {
		c.confirmSeam.denyAllPending("detached: " + reason)
	}
	c.publish(EvAttach, AttachEvent{State: state, Reason: reason})
}

// Idle reports the session-owned parts of the D7 eligibility predicate.
func (c *Controller) Idle() bool {
	var idle bool
	err := c.call(context.Background(), func() {
		idle = !c.turnSeam.isBusy() && c.schedSeam.scheduleCount() == 0 && c.goalSeam.goalIdle() && !c.residentSeam.residentsRunning()
	})
	return err == nil && idle
}

// ToolConfirmer is the controller's confirmer; it implements both the simple
// and the detailed confirmation interfaces used by core.ConfirmGate.
type ToolConfirmer interface {
	core.Confirmer
	core.ToolCallConfirmer
}

// HookBindings returns the confirmer and question asker that assembly binds to
// the confirmation gate and ask_user_question tool.
func (c *Controller) HookBindings() (ToolConfirmer, tools.QuestionAsker) {
	return controllerConfirmer{c}, c.questionAsker()
}
