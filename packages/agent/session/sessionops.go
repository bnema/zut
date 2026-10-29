package session

import (
	"bytes"
	"context"
	"runtime"
	"strconv"

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

// Close ends the session: pending confirmations and questions are resolved as
// for a detach (D3), the running turn is cancelled, EvClosed{reason} is
// published, residents and extension work are shut down and the actor stops.
// It is idempotent. Called from the actor it only schedules the stop; from
// any other goroutine it waits for Done or ctx.
func (c *Controller) Close(ctx context.Context, reason string) error {
	c.closeOnce.Do(func() {
		c.closeWhy = reason
		close(c.closeReq)
	})
	if c.onActor() {
		return nil
	}
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// closeOnActor runs on the loop right before it exits.
func (c *Controller) closeOnActor() {
	reason := c.closeWhy
	if c.turnSeam.isBusy() {
		c.turnSeam.cancelTurn()
	}
	c.confirmSeam.denyAllPending("session closed: " + reason)
	c.publish(EvClosed, ClosedEvent{Reason: reason})
	c.residentsShutdown()
	c.extShutdown()
}

// SetAttach records the attach state (D2), publishes EvAttach and, on detach,
// resolves pending requests as detached (D3). On the actor it applies
// inline; elsewhere it runs on the actor and returns once applied (or once
// the session is closed).
func (c *Controller) SetAttach(state AttachState, reason string) {
	apply := func() {
		if c.attach == state {
			return
		}
		c.attach = state
		if state == AttachStateDetached {
			c.confirmSeam.denyAllPending("detached: " + reason)
		}
		c.publish(EvAttach, AttachEvent{State: state, Reason: reason})
	}
	if c.onActor() {
		apply()
		return
	}
	_ = c.call(context.Background(), apply)
}

// Idle reports the session-owned parts of the D7 eligibility predicate.
func (c *Controller) Idle() bool {
	var idle bool
	err := c.call(context.Background(), func() {
		idle = !c.turnSeam.isBusy() && c.schedSeam.scheduleCount() == 0 && c.goalSeam.goalIdle() && !c.residentSeam.residentsRunning()
	})
	return err == nil && idle
}

// onActor reports whether the caller runs on the actor loop goroutine.
func (c *Controller) onActor() bool {
	id := c.actorGID.Load()
	return id != 0 && id == goroutineID()
}

// goroutineID parses the current goroutine ID from its stack header. It is
// only used to refuse synchronous self-waits.
func goroutineID() uint64 {
	var buf [64]byte
	b := buf[:runtime.Stack(buf[:], false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	if i := bytes.IndexByte(b, ' '); i > 0 {
		b = b[:i]
	}
	id, _ := strconv.ParseUint(string(b), 10, 64)
	return id
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
