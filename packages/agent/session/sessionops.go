package session

import (
	"context"
	"time"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

// interruptedNote is published once when a restored session's previous turn
// was stopped by a daemon restart (D6).
var interruptedNote = Note{Level: "warning", Text: "Previous turn was interrupted by a daemon restart; the session was restored idle."}

type opsState struct {
	// interrupted is actor-owned; it clears once a new turn starts.
	interrupted bool
	restoreNote bool
}

func (s *opsState) snapshotInto(out *Snapshot) {
	if s.restoreNote {
		out.Notes = append(out.Notes, interruptedNote)
	}
}

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
	return owner == c
}

// Do runs fn on the actor loop with an actor-marked context and waits for it.
// Code inside fn may call Close(actx, …) without deadlocking.
func (c *Controller) Do(ctx context.Context, fn func(actx context.Context)) error {
	return c.call(ctx, func() { fn(c.actorCtx) })
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
	c.waitTurnGoroutine(turnStopTimeout)
	c.publish(EvClosed, ClosedEvent{Reason: reason})
	c.residentsShutdown()
	c.extShutdown()
}

// waitTurnGoroutine waits, bounded, for the latest turn goroutine to return
// after cancellation so assembly cleanup never races a running agent.
func (c *Controller) waitTurnGoroutine(limit time.Duration) {
	if c.turnDone == nil {
		return
	}
	t := time.NewTimer(limit)
	defer t.Stop()
	select {
	case <-c.turnDone:
	case <-t.C:
		if c.deps.Logger != nil {
			c.deps.Logger.Warn("turn did not stop before close", "session", c.id, "timeout", limit)
		}
	}
}

// SetAttach records the attach state (D2), publishes EvAttach and, on
// detach, resolves pending requests as detached (D3). It posts to the actor
// and waits; actor-side code uses setAttachOnActor.
func (c *Controller) SetAttach(state AttachState, reason string) {
	_ = c.call(context.Background(), func() { c.setAttachOnActor(state, reason) })
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
