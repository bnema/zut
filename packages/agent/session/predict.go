package session

import (
	"context"
	"errors"

	"github.com/bnema/zut/packages/core"
)

// PredictNextPrompt asks the current agent for the user's likely next
// message. It returns "" with a nil error, without a provider call, when the
// session is headless or not quiescent: a turn is running, input is queued,
// a rescue holds the queue, a confirmation or question is pending, a
// compaction is in flight, or residents are still running. The request runs
// off the actor and is cancelled by ctx or session close. It never publishes
// events, persists state, or updates usage.
func (c *Controller) PredictNextPrompt(ctx context.Context) (string, error) {
	if c.deps.Headless {
		return "", nil
	}
	var ag *core.Agent
	if err := c.call(ctx, func() { ag = c.predictAgent() }); err != nil {
		return "", err
	}
	if ag == nil {
		return "", nil
	}
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-c.done:
			cancel()
		case <-pctx.Done():
		}
	}()
	text, err := ag.PredictNextPrompt(pctx)
	if err != nil && ctx.Err() == nil && pctx.Err() != nil {
		// Cancelled by session close rather than the caller.
		return "", errors.Join(ErrClosed, err)
	}
	return text, err
}

// predictAgent is actor-only: it returns the agent when the session is
// quiescent, or nil.
func (c *Controller) predictAgent() *core.Agent {
	if c.turnSeam == nil || c.turnSeam.isBusy() || len(c.turn.queue) > 0 || c.turn.rescuing {
		return nil
	}
	if c.compactSeam != nil && c.compactSeam.compacting() {
		return nil
	}
	if c.compact.busy || c.compact.rescuing || len(c.confirm.pending) > 0 {
		return nil
	}
	if c.residentSeam != nil && (c.residentSeam.residentsRunning() || c.residentSeam.residentsUndelivered()) {
		return nil
	}
	return c.turnSeam.currentAgent()
}
