package session

import (
	"github.com/bnema/zut/packages/agent/scheduler"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

// These actor-only seams preserve cluster ownership and permit generated mocks.
type turnSeam interface {
	startTurn(Input) error
	enqueue(Input)
	applyAgentEvent(core.AgentEvent)
	isBusy() bool
	turnOrigin() InputOrigin
	cancelTurn()
}
type confirmSeam interface {
	confirmer() core.ToolCallConfirmer
	questionAsker() tools.QuestionAsker
	denyAllPending(string)
}
type goalSeam interface {
	goalAfterTurn(core.EvTurnEnd)
	goalIdle() bool
}
type schedSeam interface {
	deliverScheduled(scheduler.Task)
	scheduleCount() int
}
type residentSeam interface {
	residentsRunning() bool
	residentSummaries() []ResidentSummary
}
type compactSeam interface{ maybeAutoCompact(Input) bool }
type settingsSeam interface{ replaceAgent(*core.Agent) }
type interSeam interface{ deliverInterSession(Sender, string) error }
