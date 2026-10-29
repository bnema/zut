package session

import (
	"context"
	"encoding/json"
	"github.com/bnema/zut/packages/agent/extensions"
	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/scheduler"
	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider/auth"
	"log/slog"
	"time"
)

// AgentBuilder preserves the corresponding InteractiveConfig callback signatures.
type AgentBuilder interface {
	BuildAgent() (*core.Agent, string, string, error)
	BuildAgentFor(providerOverride, modelOverride string) (*core.Agent, string, string, error)
	BuildAgentForRescue(providerOverride, modelOverride string) (*core.Agent, string, string, error)
}

// SessionStore preserves the corresponding InteractiveConfig callback signatures.
type SessionStore interface {
	PersistModel(providerName, model string)
	PersistTitle(title string) error
	CurrentGoal() *core.SessionGoal
	CurrentGoalHistory() []core.SessionGoal
	EnsureMission(objective string) error
	PersistGoal(goal *core.SessionGoal) error
	PersistGoalRuntime(goal *core.SessionGoal) error
	// InitialPlanSnapshots reconstructs checklist snapshots from the full, untrimmed transcript.
	InitialPlanSnapshots() map[string]core.PlanUpdate
	CurrentPlan() []core.PlanStep
	PersistPlan(plan *core.SessionPlan) error
	PersistedPlan() *core.SessionPlan
	SessionTransition(func())
	FlushSession()
	CurrentSessionPath() string
	LoadSession(path string) error
	ChangeCWD(path string) error
}

// ResidentBuilder preserves the corresponding InteractiveConfig callback signatures.
type ResidentBuilder interface {
	SpawnResident(context.Context, tools.ResidentSpawnRequest) (string, error)
	BuildResidentSpec(context.Context, tools.ResidentSpawnRequest) (subagents.ResidentChildSpec, error)
	ResolveSubagent(name string) (*subagents.Profile, error)
}

// SkillStore preserves the corresponding InteractiveConfig callback signatures.
type SkillStore interface {
	SkillSnapshot() []*skills.Skill
	LoadSkillPins(cwd string) (skills.Pins, error)
	ToggleSkillPin(cwd, name string, global bool) error
}

// ToolRefresher preserves the corresponding InteractiveConfig callback signatures.
type ToolRefresher interface {
	RefreshTools() error
	SetWebSearchAvailable(bool)
}

// ExtensionHost is the domain-facing subset of the existing Manager.
type ExtensionHost interface {
	// EmitEvent forwards host lifecycle events to extensions.
	EmitEvent(ev extproto.EventFromHost)
	// Invoke executes an extension command with the existing manager contract.
	Invoke(ctx context.Context, name, args string, timeout time.Duration) (extproto.CommandResponseFromExt, error)
	HasCommand(name string) bool
	CommandOwner(name string) string
	InvokeTool(ctx context.Context, name string, args json.RawMessage, timeout time.Duration) (extproto.ToolResultFromExt, error)
	Reload(ctx context.Context, grace time.Duration) extensions.ReloadStats
	SendPanelClose(extName, panelID string) error
	SendPanelKey(extName, panelID, key, text string) error
	Commands() []extensions.CommandInfo
}

// ResidentHost is the domain-facing subset of the existing ResidentManager.
type ResidentHost interface {
	// SetAcceptedObserver observes accepted resident input.
	SetAcceptedObserver(observer func(subagents.ResidentChildSpec, string, string))
	// SetCompletionObserver installs the completion delivery observer.
	SetCompletionObserver(observer func(subagents.ResidentCompletion) bool)
	// SetUpdateObserver observes resident projection changes.
	SetUpdateObserver(observer func(string))
	Snapshot() []subagents.ResidentSnapshot
	RecentSnapshotPage(offset, limit int) ([]subagents.ResidentSnapshot, int)
	Resume(ctx context.Context, childID, prompt string) error
	ResumeFollowUp(ctx context.Context, childID, prompt string, mode subagents.ResumeMode, turnID string, onSteered func(turnID string)) (subagents.ResumeOutcome, error)
	Result(childID string) (subagents.ResidentResult, error)
	Stop(ctx context.Context, childID string) error
	Interrupt(ctx context.Context, childID string) (bool, error)
}

// SettingsStore mirrors the existing persisted settings boundary.
type SettingsStore interface {
	SetQuickModelShortcut(slot int, providerName, model string) error
	SetInlineImages(enabled bool) error
	SetAutoSubagents(enabled bool) error
	SetJailByDefault(enabled bool) error
	SetRecursiveFileSuggest(enabled bool) error
	SetRespectGitignore(enabled bool) error
	SetCompactMode(enabled bool) error
	SetTUIInputStyle(style string) error
	SetTUIStatusPosition(position string) error
	SetTUIWorkingPosition(position string) error
	SetReasoning(level string) error
	SetTheme(name string) error
}

// ConfirmGate is the existing confirmation gate boundary.
type ConfirmGate interface {
	Check(string, string) (bool, string, json.RawMessage)
	CheckToolCall(core.ToolCallConfirmation) (bool, string, json.RawMessage)
	Reset()
	AllowAll()
	SetConfirmer(core.Confirmer)
}

// ScheduleHost exposes scheduler operations scoped to this session by assembly.
type ScheduleHost interface {
	Add(scheduler.NewTaskInput) (scheduler.Task, error)
	Cancel(string) bool
	List() []scheduler.Task
}

// DeferredStore persists reviewable denied actions outside the core transcript.
type DeferredStore interface {
	Append(DeferredAction) error
	List() []DeferredAction
	Remove(string) error
}

// QuickModelShortcut mirrors the existing model profile slot.
type QuickModelShortcut struct {
	Provider, Model, Reasoning string
	FastMode                   bool
}

// Deps contains session-owned domain resources, never terminal resources.
// Replaceable callbacks are grouped into interfaces using their original method
// names and types; Clock is the explicitly approved time source.
type Deps struct {
	AgentBuilder                                                                                    AgentBuilder
	SessionStore                                                                                    SessionStore
	ResidentBuilder                                                                                 ResidentBuilder
	SkillStore                                                                                      SkillStore
	ToolRefresher                                                                                   ToolRefresher
	Extensions                                                                                      ExtensionHost
	ResidentManager                                                                                 ResidentHost
	ConfirmGate                                                                                     ConfirmGate
	SettingsStore                                                                                   SettingsStore
	AuthManager                                                                                     *auth.Manager
	Directory                                                                                       Directory
	Scheduler                                                                                       ScheduleHost
	DeferredStore                                                                                   DeferredStore
	Clock                                                                                           func() time.Time
	Logger                                                                                          *slog.Logger
	Env                                                                                             []string
	QuickModelShortcuts                                                                             []QuickModelShortcut
	Model, Provider, Reasoning, CWD, SystemPrompt, WritingGuidance                                  string
	MaxSteps                                                                                        int
	Version, ZutHome, SessionsRoot                                                                  string
	NoYolo                                                                                          bool
	AutoCompactThreshold                                                                            *int
	GoalMaxTokenBudget                                                                              *uint64
	AutoSubagentsEnabled, WebSearchEnabled, LSPEnabled, SubagentLSPEnabled                          *bool
	WebSearchToolAllowed, AutoSubagentsToolAllowed, AutoSubagentsStatusToolAllowed                  *bool
	AutoSubagentsStopToolAllowed, AutoSubagentsResumeToolAllowed, AutoSubagentsInterruptToolAllowed *bool
	FastMode, JailByDefault                                                                         *bool
	AutoSubagentsSystemAddendum, OnDemandSubagentsSystemAddendum, SubagentsSystemAddendum           string
}
