package session

import (
	"encoding/json"
	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"time"
)

// ID identifies a session contract value.
type ID string

// AttachState identifies a session contract value.
type AttachState string

// RunState identifies a session contract value.
type RunState string

// InputOrigin identifies a session contract value.
type InputOrigin string

// SubmitMode identifies a session contract value.
type SubmitMode string

// Info is a terminal-free session contract payload.
type Info struct {
	ID              string            `json:"id"`
	Title           string            `json:"title"`
	CWD             string            `json:"cwd"`
	RepoRoot        string            `json:"repo_root"`
	Provider        string            `json:"provider"`
	Model           string            `json:"model"`
	Reasoning       string            `json:"reasoning"`
	FastMode        bool              `json:"fast_mode"`
	Attach          AttachState       `json:"attach"`
	Run             RunState          `json:"run"`
	CurrentOrigin   InputOrigin       `json:"current_origin"`
	Activity        Activity          `json:"activity"`
	Goal            *GoalSummary      `json:"goal"`
	PlanStep        *PlanStepSummary  `json:"plan_step"`
	Residents       []ResidentSummary `json:"residents"`
	Schedules       int               `json:"schedules"`
	Deferred        int               `json:"deferred"`
	Usage           provider.Usage    `json:"usage"`
	ContextUsed     int               `json:"context_used"`
	ContextMax      int               `json:"context_max"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	LastUserInputAt time.Time         `json:"last_user_input_at"`
	SessionPath     string            `json:"session_path"`
}

// Activity is a terminal-free session contract payload.
type Activity struct {
	Kind  string    `json:"kind"`
	Tool  string    `json:"tool"`
	Since time.Time `json:"since"`
}

// GoalSummary is a terminal-free session contract payload.
type GoalSummary struct {
	Objective string `json:"objective"`
	Status    string `json:"status"`
}

// PlanStepSummary is a terminal-free session contract payload.
type PlanStepSummary struct {
	Index int    `json:"index"`
	Total int    `json:"total"`
	Text  string `json:"text"`
}

// ResidentSummary is a terminal-free session contract payload.
type ResidentSummary struct {
	ID        string    `json:"id"`
	Profile   string    `json:"profile"`
	State     string    `json:"state"`
	Workspace string    `json:"workspace"`
	Task      string    `json:"task"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Snapshot is a terminal-free session contract payload.
type Snapshot struct {
	Info        Info                       `json:"info"`
	Seq         uint64                     `json:"seq"`
	Messages    []provider.Message         `json:"messages"`
	Live        LiveTurn                   `json:"live"`
	Queue       []QueuedInput              `json:"queue"`
	Plan        map[string]core.PlanUpdate `json:"plan"`
	GoalHistory []core.SessionGoal         `json:"goal_history"`
	Pending     []PendingRequest           `json:"pending"`
	Deferred    []DeferredAction           `json:"deferred"`
	ExtChrome   ExtChrome                  `json:"ext_chrome"`
	Notes       []Note                     `json:"notes"`
}

// LiveTurn is a terminal-free session contract payload.
type LiveTurn struct {
	TurnID        string      `json:"turn_id"`
	AssistantText string      `json:"assistant_text"`
	Tools         []LiveTool  `json:"tools"`
	Origin        InputOrigin `json:"origin"`
}

// LiveTool is a terminal-free session contract payload.
type LiveTool struct {
	ID       string           `json:"id"`
	Name     string           `json:"name"`
	Args     json.RawMessage  `json:"args"`
	State    string           `json:"state"`
	Progress string           `json:"progress"`
	Result   *core.ToolResult `json:"result"`
}

// Input is a terminal-free session contract payload.
type Input struct {
	// Hidden contains extra hidden messages appended before the prompt by the turn cluster, carrying Meta.
	Hidden []provider.Message `json:"hidden,omitempty"`
	// HostEvent is queued as core.QueuedMessage.HostEvent by the turn cluster.
	HostEvent bool                  `json:"host_event,omitempty"`
	Text      string                `json:"text"`
	Images    []provider.ImageBlock `json:"images"`
	Mode      SubmitMode            `json:"mode"`
	Origin    InputOrigin           `json:"origin"`
	From      *Sender               `json:"from"`
}

// Sender is a terminal-free session contract payload.
type Sender struct {
	SessionID ID     `json:"session_id"`
	Title     string `json:"title"`
	CWD       string `json:"cwd"`
}

// QueuedInput is a terminal-free session contract payload.
type QueuedInput struct {
	ID    string `json:"id"`
	Input Input  `json:"input"`
}

// PendingRequest is a terminal-free session contract payload.
type PendingRequest struct {
	ID        string                     `json:"id"`
	Kind      string                     `json:"kind"`
	Rescue    *RescueOptions             `json:"rescue"`
	Confirm   *core.ToolCallConfirmation `json:"confirm"`
	Questions []tools.Question           `json:"questions"`
	CreatedAt time.Time                  `json:"created_at"`
}

// RescueOptions is a terminal-free session contract payload.
type RescueOptions struct {
	Error      string   `json:"error"`
	Candidates []string `json:"candidates"`
}

// DeferredAction is a terminal-free session contract payload.
type DeferredAction struct {
	ID        string           `json:"id"`
	Kind      string           `json:"kind"`
	Tool      string           `json:"tool"`
	Summary   string           `json:"summary"`
	Questions []tools.Question `json:"questions"`
	TurnID    string           `json:"turn_id"`
	At        time.Time        `json:"at"`
}

// ExtChrome is a terminal-free session contract payload.
type ExtChrome struct {
	Statuses map[string]map[string]ExtStatus `json:"statuses"`
	Widgets  map[string]map[string]ExtWidget `json:"widgets"`
	Panels   []ExtPanel                      `json:"panels"`
	Alert    *extproto.AlertRequest          `json:"alert"`
}

// ExtStatus is a terminal-free session contract payload.
type ExtStatus struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

// ExtWidget is a terminal-free session contract payload.
type ExtWidget struct {
	Position string   `json:"position"`
	Title    string   `json:"title"`
	Lines    []string `json:"lines"`
}

// ExtPanel associates an extension with its existing extproto.PanelSpec.
type ExtPanel struct {
	Ext  string             `json:"ext"`
	Spec extproto.PanelSpec `json:"spec"`
}

// Note mirrors the level and text of interactive status and extension notes.
type Note struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

// SettingsPatch changes runtime domain settings. Orchestration is launch-only
// (CreateSpec.Args), not a runtime setting.
type SettingsPatch struct {
	AutoSubagentsEnabled *bool   `json:"auto_subagents_enabled"`
	WebSearchEnabled     *bool   `json:"web_search_enabled"`
	LSPEnabled           *bool   `json:"lsp_enabled"`
	SubagentLSPEnabled   *bool   `json:"subagent_lsp_enabled"`
	AutoCompactThreshold *int    `json:"auto_compact_threshold"`
	JailByDefault        *bool   `json:"jail_by_default"`
	FastMode             *bool   `json:"fast_mode"`
	GoalMaxTokenBudget   *uint64 `json:"goal_max_token_budget"`
}

// GoalCommand is a terminal-free session contract payload.
type GoalCommand struct {
	Op        string `json:"op"`
	Objective string `json:"objective"`
}

// ResidentCommand is a terminal-free session contract payload.
type ResidentCommand struct {
	Op      string `json:"op"`
	ChildID string `json:"child_id"`
	Prompt  string `json:"prompt"`
}

// Tree is a terminal-free session contract payload.
type Tree struct {
	Workspaces []WorkspaceNode `json:"workspaces"`
}

// WorkspaceNode is a terminal-free session contract payload.
type WorkspaceNode struct {
	RepoRoot string        `json:"repo_root"`
	Sessions []SessionNode `json:"sessions"`
}

// SessionNode is a terminal-free session contract payload.
type SessionNode struct {
	Info      Info              `json:"info"`
	Residents []ResidentSummary `json:"residents"`
}

// CreateSpec is a terminal-free session contract payload.
type CreateSpec struct {
	CWD        string    `json:"cwd"`
	Env        []string  `json:"env"`
	Args       []string  `json:"args"`
	ResumePath string    `json:"resume_path"`
	Fork       *ForkSpec `json:"fork"`
	Headless   bool      `json:"headless"`
}

// RestoreSpec is a terminal-free session contract payload.
type RestoreSpec struct {
	ID              ID       `json:"id"`
	SessionPath     string   `json:"session_path"`
	CWD             string   `json:"cwd"`
	Env             []string `json:"env"`
	Args            []string `json:"args"`
	InterruptedTurn bool     `json:"interrupted_turn"`
}

// ForkSpec mirrors core.BranchSession and BranchSessionHidden. UpToMessage is
// the number of message rows copied; root, cwd and version come from CreateSpec/Deps.
type ForkSpec struct {
	ParentPath  string `json:"parent_path"`
	UpToMessage int    `json:"up_to_message"`
	Hidden      bool   `json:"hidden"`
}

const (
	// AttachStateAttached denotes attached.
	AttachStateAttached AttachState = "attached"
	// AttachStateDetached denotes detached.
	AttachStateDetached AttachState = "detached"
)
const (
	// RunStateIdle denotes idle.
	RunStateIdle RunState = "idle"
	// RunStateRunning denotes running.
	RunStateRunning RunState = "running"
	// RunStateCompacting denotes compacting.
	RunStateCompacting RunState = "compacting"
	// RunStateClosing denotes closing.
	RunStateClosing RunState = "closing"
	// RunStateClosed denotes closed.
	RunStateClosed RunState = "closed"
)
const (
	// InputOriginUser denotes user.
	InputOriginUser InputOrigin = "user"
	// InputOriginScheduler denotes scheduler.
	InputOriginScheduler InputOrigin = "scheduler"
	// InputOriginGoal denotes goal.
	InputOriginGoal InputOrigin = "goal"
	// InputOriginExtension denotes extension.
	InputOriginExtension InputOrigin = "extension"
	// InputOriginResident denotes resident.
	InputOriginResident InputOrigin = "resident"
	// InputOriginInterSession denotes inter_session.
	InputOriginInterSession InputOrigin = "inter_session"
	// InputOriginTelegram denotes telegram.
	InputOriginTelegram InputOrigin = "telegram"
)
const (
	// SubmitModeAuto denotes auto.
	SubmitModeAuto SubmitMode = "auto"
	// SubmitModeSteer denotes steer.
	SubmitModeSteer SubmitMode = "steer"
	// SubmitModeQueue denotes queue.
	SubmitModeQueue SubmitMode = "queue"
)
