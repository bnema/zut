package session

import (
	"context"
	"errors"
	"time"

	"github.com/bnema/zut/packages/core"
)

// predictTimeout bounds a prediction server-side: client cancellation does
// not cross the daemon wire, so the daemon must stop on its own.
const predictTimeout = 20 * time.Second

// predictState is actor-owned: the cancel of the in-flight prediction and the
// generation that owns it.
type predictState struct {
	gen    uint64
	cancel context.CancelFunc
}

// PredictNextPrompt asks the current agent for the user's likely next
// message. It returns "" with a nil error, without a provider call, when the
// session is headless or not quiescent: a turn, rescue or compaction is
// running, input is queued, a confirmation or question is pending, or
// residents are still running. The request runs off the actor and ends on
// ctx, session close, predictTimeout, a newer prediction, Submit, a queued
// input, a turn start, or a compaction start. It never publishes events,
// persists state, or updates usage. A session close wraps ErrClosed.
func (c *Controller) PredictNextPrompt(ctx context.Context) (string, error) {
	if c.deps.Headless {
		return "", nil
	}
	pctx, cancel := context.WithTimeout(ctx, predictTimeout)
	defer cancel()
	var (
		ag  *core.Agent
		gen uint64
	)
	if err := c.call(ctx, func() {
		c.cancelPrediction()
		if ag = c.predictAgent(); ag == nil {
			return
		}
		c.predict.gen++
		gen = c.predict.gen
		c.predict.cancel = cancel
	}); err != nil {
		return "", err
	}
	if ag == nil {
		return "", nil
	}
	defer func() {
		// Never clear the cancel of a newer prediction.
		_ = c.post(context.Background(), func() {
			if c.predict.gen == gen {
				c.predict.cancel = nil
			}
		})
	}()
	go func() {
		select {
		case <-c.done:
			cancel()
		case <-pctx.Done():
		}
	}()
	text, err := ag.PredictNextPrompt(pctx)
	if err != nil {
		select {
		case <-c.done:
			return "", errors.Join(ErrClosed, err)
		default:
		}
	}
	return text, err
}

// cancelPrediction is actor-only: it stops the in-flight prediction, if any.
func (c *Controller) cancelPrediction() {
	if c.predict.cancel != nil {
		c.predict.cancel()
		c.predict.cancel = nil
	}
}

// predictAgent is actor-only: it returns the agent when the session is
// quiescent, or nil. isBusy covers turn rescue and compaction;
// residentsRunning covers undelivered resident completions.
func (c *Controller) predictAgent() *core.Agent {
	if c.turnSeam.isBusy() || len(c.turn.queue) > 0 || len(c.confirm.pending) > 0 || c.residentSeam.residentsRunning() {
		return nil
	}
	return c.turnSeam.currentAgent()
}
