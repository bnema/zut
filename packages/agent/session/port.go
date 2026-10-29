package session

import (
	"context"
	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

// Port is the session API shared by local and remote clients.
// Subscribe replays events after afterSeq, or returns ErrSeqTooOld.
// Slow subscribers receive a final detached/slow_consumer AttachEvent and close.
// ExtensionPanelInput uses the existing PanelKeyFromHost wire key.
type Port interface {
	Info() Info
	Snapshot(ctx context.Context) (Snapshot, error)
	Subscribe(ctx context.Context, afterSeq uint64) (<-chan Event, error)
	Submit(ctx context.Context, in Input) error
	Abort(ctx context.Context) error
	Compact(ctx context.Context) error
	CancelQueued(ctx context.Context, id string) error
	SlideBackQueued(ctx context.Context) (Input, error)
	AnswerConfirmation(ctx context.Context, reqID string, decision core.ConfirmDecision) error
	AnswerQuestion(ctx context.Context, reqID string, answers []tools.QuestionAnswer) error
	ResolveDeferred(ctx context.Context, id, action string) error
	SetModel(ctx context.Context, providerName, model string) error
	SetReasoning(ctx context.Context, level string) error
	SetFastMode(ctx context.Context, enabled bool) error
	ApplyProfile(ctx context.Context, n int) error
	ApplySettings(ctx context.Context, patch SettingsPatch) error
	SetGoal(ctx context.Context, command GoalCommand) error
	ChangeCWD(ctx context.Context, path string) error
	SetJail(ctx context.Context, enabled bool) error
	SetTitle(ctx context.Context, title string) error
	ToggleSkillPin(ctx context.Context, name string, global bool) error
	ReloadExtensions(ctx context.Context) error
	InvokeExtensionCommand(ctx context.Context, name, args string) error
	ExtensionPanelInput(ctx context.Context, ext, panelID string, key extproto.PanelKeyFromHost) error
	ReloadCredentials(ctx context.Context) error
	ResidentAction(ctx context.Context, command ResidentCommand) error
	SessionsQuery(ctx context.Context) (Tree, error)
}

// Directory provides daemon-owned discovery and guarded inter-session delivery.
type Directory interface {
	List(context.Context) []Info
	Tree(context.Context) Tree
	Send(ctx context.Context, from, to ID, text string) error
}

// Factory constructs and restores daemon session handles.
type Factory interface {
	Create(context.Context, CreateSpec) (Handle, error)
	Restore(context.Context, RestoreSpec) (Handle, error)
}

// Handle extends the client port with daemon lifecycle operations.
type Handle interface {
	Port
	SetAttach(state AttachState, reason string)
	Close(ctx context.Context, reason string) error
	Idle() bool
	Done() <-chan struct{}
}
