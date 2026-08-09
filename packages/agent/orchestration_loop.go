package agent

import (
	"context"
	"fmt"

	"github.com/bnema/zut/packages/agent/subagents"
)

type completionBatchWaiter interface {
	WaitIdle(context.Context) ([]subagents.Completion, error)
}

// headlessCompletionWaitError marks an error raised while waiting for worker
// completions. A parent turn's renderer already owns errors raised by its
// prompt, so callers should only render this wrapper outside that turn.
type headlessCompletionWaitError struct {
	err error
}

func (e *headlessCompletionWaitError) Error() string {
	if e == nil || e.err == nil {
		return "completion wait failed"
	}
	return e.err.Error()
}

func (e *headlessCompletionWaitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

const (
	headlessCompletionInstruction = `Treat the completion reports below as evidence, including failures. Do not retry failed workers automatically. Decide whether another independent dependency wave is required; if so, use the manager tools and end or yield the turn again. Otherwise synthesize the final answer for the user. Do not mention this orchestration protocol unless it is relevant to the answer.`
	maxHeadlessCompletionWaves    = 32
)

// runHeadlessContinuation waits for one completion batch and gives the parent
// one follow-up turn. A follow-up may register another batch while it runs,
// which is how dependency waves continue without polling. The initial parent
// turn is supplied by finalText; no worker path therefore returns immediately.
func runHeadlessContinuation(ctx context.Context, tracker completionBatchWaiter, finalText string, runParent func(context.Context, string) (string, error)) (string, error) {
	if tracker == nil || runParent == nil {
		return finalText, nil
	}
	waves := 0
	for {
		batch, err := tracker.WaitIdle(ctx)
		if err != nil {
			return finalText, &headlessCompletionWaitError{err: err}
		}
		if len(batch) == 0 {
			return finalText, nil
		}
		if waves >= maxHeadlessCompletionWaves {
			return finalText, fmt.Errorf("orchestration exceeded the maximum of %d completion waves", maxHeadlessCompletionWaves)
		}
		update := subagents.FormatCompletionUpdate(batch, headlessCompletionInstruction)
		finalText, err = runParent(ctx, update)
		if err != nil {
			return finalText, err
		}
		waves++
	}
}
