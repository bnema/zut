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
	applyAgentEvent(core.AgentEvent)
	isBusy() bool
	turnOrigin() InputOrigin
	// turnInfo projects turn-owned status and accounting into Info.
	turnInfo() turnInfoProjection
	cancelTurn()
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
	residentsRunStarted()
	residentsRunEnded()
	// residentsInit wires observers before the actor accepts commands.
	residentsInit()
	residentsRunning() bool
	residentSummaries() []ResidentSummary
}
type compactSeam interface{ maybeAutoCompact(Input) bool }
type settingsSeam interface{ replaceAgent(*core.Agent) }
type interSeam interface{ deliverInterSession(Sender, string) error }
