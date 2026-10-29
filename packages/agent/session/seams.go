package session

import (
	"context"

	"github.com/bnema/zut/packages/agent/scheduler"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

// These actor-only seams preserve cluster ownership and permit generated mocks.
type turnSeam interface {
	// currentAgent lazily builds the active agent; actor-only, nil on build failure.
	currentAgent() *core.Agent
	// setAgent replaces the idle agent; actor-only.
	setAgent(*core.Agent) error
	startTurn(Input) error
	enqueue(Input)
	// enqueueFront puts in at the head of the queue (aborted compaction prompt).
	enqueueFront(Input)
	applyAgentEvent(core.AgentEvent)
	isBusy() bool
	turnOrigin() InputOrigin
	// turnInfo projects turn-owned status and accounting into Info.
	turnInfo() turnInfoProjection
	cancelTurn()
	// endRescue releases the queue held since a turn failed into rescue; call
	// it before retrying or draining.
	endRescue()
	// rescueFailed reports the original failed turn end to the goal cluster
	// when no retry runs; a started retry reports its own end instead.
	rescueFailed()
	// drainQueued starts the next queued input once compaction releases the
	// transcript. It is a no-op while busy (turn running or compaction active).
	drainQueued()
}
type confirmSeam interface {
	// awaitPending MUST be called OFF the actor loop: it blocks.
	// It registers and publishes a pending request, then waits for an
	// answer, detach or turn cancellation; detached requests return ErrNotAttached.
	// The rescue cluster uses Kind "rescue" with an answer value "provider/model".
	awaitPending(ctx context.Context, req PendingRequest) ([]tools.QuestionAnswer, error)
	confirmer() core.ToolCallConfirmer
	questionAsker() tools.QuestionAsker
	denyAllPending(string)
}
type goalSeam interface {
	goalAfterTurn(core.EvTurnEnd)
	goalIdle() bool
	// goalOnAbort arms interrupted-goal reassessment on abort or cancellation.
	goalOnAbort()
	// goalBeforeTurn consumes reassessment before the next user prompt.
	goalBeforeTurn(in *Input)
}
type schedSeam interface {
	deliverScheduled(scheduler.Task)
	scheduleCount() int
}
type residentSeam interface {
	// residentsInit wires observers before the actor accepts commands.
	residentsInit()
	residentsRunStarted()
	residentsRunEnded()
	residentsRunning() bool
	residentSummaries() []ResidentSummary
}
type compactSeam interface {
	maybeAutoCompact(Input) bool
	// compacting reports an in-flight compaction (busy for Submit/updateInfo).
	compacting() bool
	// abortCompaction cancels cleanly: EvCompaction{failed}, transcript unchanged.
	abortCompaction()
	// rescueTurn asks for a fallback model off the actor, then retries retry.
	rescueTurn(cause error, retry Input)
}
type settingsSeam interface{ replaceAgent(*core.Agent) }
type interSeam interface{ deliverInterSession(Sender, string) error }
