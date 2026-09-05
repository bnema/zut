package modes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bnema/zut/packages/agent/extensions"
	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/internal/orchestration"
	"github.com/bnema/zut/packages/agent/modes/telegram"
	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/auth"
	"github.com/bnema/zut/packages/tui"
)

// InteractiveConfig configures the interactive loop.
type InteractiveConfig struct {
	Terminal        tui.Terminal
	Theme           tui.Theme
	Model           string
	Provider        string
	AuthMethod      string // "apikey" | "oauth" — distinguishes API charges from subscription estimates
	BaseURL         string
	Reasoning       string
	SystemPrompt    string
	WritingGuidance string
	Tools           core.Registry
	MaxSteps        int
	CWD             string

	// FetchCodexWeeklyUsage resolves current subscription credentials and reads
	// the account allowance. It must honor cancellation; nil disables polling.
	FetchCodexWeeklyUsage func(context.Context) (*provider.CodexWeeklyUsage, error)

	// Startup resource fields are loaded inputs available to list before the
	// transcript. They are never sent to the provider or persisted by the view.
	StartupAgentName      string
	StartupContextPaths   []string
	StartupExtensionNames []string
	StartupSkillNames     []string

	// StartupExtensionErrors are non-fatal extension load failures collected
	// before the TUI starts. They are display-only and never enter the transcript.
	StartupExtensionErrors []string

	// ShowInstructionsAtStartup controls whether startup resources are visible.
	// nil means the default, off.
	ShowInstructionsAtStartup *bool

	// InlineImagesEnabled overrides terminal image rendering. nil means
	// auto-detect and render when supported; false disables; true uses
	// the detected protocol when available.
	InlineImagesEnabled *bool

	// TerminalAlertsEnabled controls interactive alerts from the main
	// agent and extensions. nil means enabled; false disables them.
	TerminalAlertsEnabled *bool

	// TerminalTitleEnabled controls the hidden title request and terminal
	// title updates. nil means enabled; false disables both.
	TerminalTitleEnabled *bool

	// AutoSubagentsEnabled mirrors the persisted config flag at startup so
	// the /settings dialog can render the current state without
	// re-reading config.json on every open.
	AutoSubagentsEnabled *bool

	// PonytailEnabled mirrors the persisted default-on coding-guidance
	// setting at startup. nil/missing means enabled.
	PonytailEnabled *bool

	// WebSearchEnabled mirrors the persisted default-on web-search setting.
	// nil/missing means enabled for normal interactive sessions.
	WebSearchEnabled *bool

	// WebSearchToolAllowed is the immutable invocation-level capability ceiling.
	// false means --no-tools, --tools, or a packaged PermissionSet excludes it.
	WebSearchToolAllowed *bool

	// WebSearchInvocationOverride is true when an explicit --tools list includes
	// web_search. That invocation-level opt-in takes precedence over the
	// persisted setting, so the settings row is informational for this session.
	WebSearchInvocationOverride bool

	// AutoSubagentsToolAllowed is nil for normal sessions and false when the
	// launch-time --no-tools/--tools policy excludes subagent spawning.
	AutoSubagentsToolAllowed *bool

	// AutoSubagentsStatusToolAllowed is nil for normal sessions and false when
	// the launch-time --no-tools/--tools policy excludes status queries.
	AutoSubagentsStatusToolAllowed *bool

	// AutoSubagentsStopToolAllowed and AutoSubagentsResumeToolAllowed are nil
	// for normal sessions and false when the launch-time tool policy excludes
	// manager lifecycle actions.
	AutoSubagentsStopToolAllowed   *bool
	AutoSubagentsResumeToolAllowed *bool

	// FastMode mirrors the persisted OpenAI fast-mode flag at startup.
	// nil/missing means disabled. Unsupported providers reject attempts
	// to enable it and reject requests when it remains enabled.
	FastMode *bool

	// LSPEnabled controls the main session's built-in lsp tool. nil means
	// enabled by default.
	LSPEnabled *bool

	// SubagentLSPEnabled controls whether newly spawned subagent children
	// receive the lsp tool. nil means enabled by default.
	SubagentLSPEnabled *bool

	// AutoCompactThreshold is the context-window percentage that triggers
	// automatic compaction. nil means 85; zero disables percentage-based
	// triggers while preserving payload-too-large recovery.
	AutoCompactThreshold *int

	// JailByDefault mirrors the persisted jail_by_default preference.
	// The current sandbox may differ after a session-scoped /jail or /unjail.
	JailByDefault *bool

	// RecursiveFileSuggest mirrors the persisted recursive_file_suggest
	// flag at startup. When true the @-mention picker fuzzy-searches the
	// whole project tree instead of browsing one directory at a time.
	RecursiveFileSuggest *bool

	// RespectGitignore mirrors the persisted respect_gitignore flag at
	// startup. nil means the default (on); when false the @-mention
	// picker shows files matched by the project's root .gitignore.
	RespectGitignore *bool

	// ThemeName mirrors the persisted config theme value. Empty means auto.
	ThemeName string
	// EffectiveThemeName is the process-local selection after ZUT_THEME
	// precedence. ThemeForced keeps settings persistent without changing a
	// dark/light environment override during this run.
	EffectiveThemeName string
	ThemeForced        bool
	// TerminalProfile is distinct from the applied Theme because an explicit
	// or custom theme is not evidence about the controlling terminal.
	TerminalProfile tui.TerminalProfile
	// ThemeWatchInterval is test-only when non-zero; production uses 500ms.
	ThemeWatchInterval time.Duration

	// FlatTools renders tool calls without the bordered panel (a quiet
	// header line plus indented, frameless output). Mirrors the
	// resolved tool_render config / ZUT_FLAT_TOOLS env at startup.
	FlatTools bool

	// CompactUser renders sent user messages as a single quiet gutter
	// line instead of a padded, tinted bubble. Mirrors the resolved
	// compact_input config / ZUT_COMPACT_INPUT env at startup.
	CompactUser bool

	// CompactMode mirrors the persisted compact_mode flag at startup.
	// When true, it enables both flat tool calls and compact user turns.
	CompactMode *bool

	// TUIInputStyle controls the main input rendering: plain or lines.
	TUIInputStyle string

	// TUIStatusPosition controls whether the status block renders above
	// or below the main input.
	TUIStatusPosition string

	// TUIWorkingPosition controls whether the busy/working spinner renders
	// above or below the main input.
	TUIWorkingPosition string

	// QuickModelShortcuts maps slots 1-9 to provider/model pairs. The
	// shortcuts are Ctrl+1..9. Cmd+1..9 may also work when the terminal
	// forwards Command/Super keypresses, but Ctrl is the displayed chord.
	QuickModelShortcuts []QuickModelShortcut

	// ExtensionThemes returns themes bundled with loaded extensions.
	ExtensionThemes func() []tui.ThemeOption

	// AutoSubagentsSystemAddendum is the proactive collaboration block that
	// gets appended when the user enables auto-subagents at runtime. Plumbed in
	// from the cli so this package doesn't have to import agent (cycle).
	AutoSubagentsSystemAddendum string

	// OnDemandSubagentsSystemAddendum limits delegation to explicit user
	// requests while auto-subagents is disabled.
	OnDemandSubagentsSystemAddendum string

	// SubagentsSystemAddendum is the metadata-only [subagents_list]
	// block available whenever the primary agent can spawn subagents.
	SubagentsSystemAddendum string
	SettingsStore           SettingsStore

	// Agent is optional. If nil, zut opens without credentials; the
	// user must /login before they can prompt.
	Agent *core.Agent

	// InitialCompactHandoff is the opaque, persisted handoff snapshot for a
	// resumed interactive session. Invalid data is ignored by this package.
	InitialCompactHandoff json.RawMessage
	// PersistCompactHandoff records the current opaque handoff snapshot in the
	// active session. It must not re-enter Interactive.
	PersistCompactHandoff func(json.RawMessage) error
	// CurrentCompactHandoff returns the opaque handoff snapshot for the session
	// currently owned by the host after a session switch. Invalid data is ignored.
	CurrentCompactHandoff func() json.RawMessage

	InitialInput string

	// StartupPre is auto-submitted once when the interactive session
	// opens, before InitialInput is applied. Uses the same path as a
	// typed Enter submit ("!" → shell escape, otherwise a model turn).
	StartupPre string

	// OnStartupPreDone runs after StartupPre finishes (shell escape or
	// model turn), before deferred InitialInput is applied. Hosts use
	// it to rediscover skills / reload extensions installed by pre.
	OnStartupPreDone func()

	// RefreshTools re-resolves the live agent registry after a setting
	// changes the main session's tool availability. Optional for embedders.
	RefreshTools func() error

	// SetWebSearchAvailable updates session-wide execution and generic-child
	// ceilings after a live registry commit or fail-closed revocation. It can
	// run while agentMu and i.mu are held, so callbacks must not re-enter
	// Interactive or wait on either lock. The CLI callback only updates its
	// web-search guard and resident child policy.
	SetWebSearchAvailable func(bool)

	// RefreshPrompt re-resolves the complete live prompt and tool registry
	// after a prompt-affecting setting changes. Optional for embedders.
	RefreshPrompt func() error

	// AutoSubmitInitial, when true, auto-submits InitialInput after
	// StartupPre completes (or immediately when StartupPre is empty).
	// When false, InitialInput only pre-fills the editor.
	AutoSubmitInitial bool

	// Auth is required. When the user runs /login, Interactive talks to
	// AuthManager to open a browser and wait for the callback.
	AuthManager *auth.Manager

	// LlamaCPPConfig resolves the router URL and optional API key from env or
	// the credential saved by /login.
	LlamaCPPConfig func() (baseURL, apiKey string, err error)

	// RefreshLlamaCPPModels synchronizes currently loaded router models into
	// the catalog before /model opens.
	RefreshLlamaCPPModels func(context.Context) error

	// BuildAgent is called after a successful login to (re)construct the
	// agent with the fresh credential. It returns the new agent and
	// the concrete provider/model in use.
	BuildAgent func() (*core.Agent, string, string, error)

	// SetKimiCLIFallbackDisabled controls whether zut may fall back to
	// the official Kimi Code CLI token when zut has no stored Kimi token.
	SetKimiCLIFallbackDisabled func(disabled bool) error

	// BuildAgentFor rebuilds the agent with an explicit provider/model
	// override (used by the /model picker when switching providers).
	// If providerOverride is empty, the current provider is kept.
	BuildAgentFor func(providerOverride, modelOverride string) (*core.Agent, string, string, error)

	// BuildAgentForRescue rebuilds the agent for the rescue picker that
	// opens after a recoverable provider failure. Unlike BuildAgentFor,
	// this builder drops launch-time --api-key and --base-url overrides
	// because those are usually the reason rescue triggered. Re-resolves
	// credentials from env vars / auth.json / provider defaults so the
	// retry has a real chance of succeeding. Falls back to BuildAgentFor
	// when nil so embedders that don't wire it keep working.
	BuildAgentForRescue func(providerOverride, modelOverride string) (*core.Agent, string, string, error)

	// LoggedInProviders returns the list of provider names that
	// currently have credentials. Used by /model to filter the
	// picker to only show reachable models.
	LoggedInProviders func() []string

	// ZutHome is zut's global state directory, used by authentication,
	// themes, extensions, and other shared configuration.
	ZutHome string

	// SessionsRoot is the root passed to core session operations. It differs
	// from ZutHome for Zutfile agents, whose sessions are isolated by agent
	// name. Empty falls back to ZutHome for embedders and tests.
	SessionsRoot string
	// SessionsDisabled is true for --no-session invocations. The picker must
	// fail closed instead of offering stale or non-persisted state.
	SessionsDisabled bool

	// Version is the binary's current version (from main.version).
	// Used only for display; the update check itself is done outside
	// this package to avoid an import cycle.
	Version string

	// UpdateInfoChan is an optional channel that delivers the result
	// of the github-release update check. Interactive reads at most
	// one value, drops it if the check reported nothing, and otherwise
	// surfaces a yellow "update available" banner at the top of the
	// chat. Nil channel = no banner, no startup cost.
	UpdateInfoChan <-chan UpdateInfo

	// Sandbox is the shared sandbox pointer. Toggled by /jail and /unjail.
	Sandbox *tools.Sandbox

	// LoadSession swaps the current session for the one at path. The
	// callback returns the new agent message slice so the TUI can invalidate.
	LoadSession func(path string) error
	// LoadSessionContext is the cancellation-aware picker path. Embedders that
	// only implement LoadSession retain the existing synchronous contract.
	LoadSessionContext func(context.Context, string) error

	// ChangeCWD switches the running zut session's working directory
	// to path. The host closes the current session, rebuilds the
	// agent so tools / AGENTS.md / sandbox bind to the new cwd, and
	// opens a fresh session there. Returns an error if path doesn't
	// exist, isn't a directory, or the host can't rebuild the agent.
	//
	// Optional: not wired by every embedder. When nil the hidden /cd
	// command surfaces a clear error rather than no-oping.
	ChangeCWD func(path string) error

	// CurrentSessionPath returns the path of the live session file
	// on disk (the one every AppendMessage writes to). Used by
	// /session export so the exporter ships the exact bytes on
	// disk. Returns an empty string when --no-session is set or
	// no session is open.
	CurrentSessionPath func() string

	// FlushSession writes any in-memory agent messages to the
	// session file that haven't been persisted yet. Called by
	// /session export right before reading the file so the
	// exported bytes reflect the full current conversation, not
	// just the rows the agent happened to write synchronously.
	// The default WriteNewTranscript-at-exit strategy means most
	// of a running session lives only in memory until the tui
	// closes; without a flush hook, /session export writes a
	// file that only has the meta row.
	FlushSession func()

	// SessionTransition serializes a state-changing session/model
	// operation with the host's session load. It is optional for embedders;
	// the CLI uses it to prevent a slow provider rebuild from being replaced
	// by a concurrent resume (or vice versa).
	SessionTransition func(func())

	// PersistModel is called whenever the user switches model or provider.
	// It should update config.json and (if there's an active session)
	// write a new meta row so resume picks up the same model.
	PersistModel func(providerName, model string)
	// OnReasoningChanged updates host-owned runtime defaults after the setting
	// has been persisted successfully.
	OnReasoningChanged func(level string)

	// InitialSessionTitle is the persisted title of the session loaded before
	// the TUI starts. It is shown in the terminal without another model call.
	InitialSessionTitle string
	// InitialSessionTitlePending is true for a newly forked branch that only
	// contains its copied prefix. The copied user messages must not prevent a
	// new title after the branch's first real prompt.
	InitialSessionTitlePending bool

	// CurrentSessionTitle returns the title of the session currently owned by
	// the host. It is used after /sessions and import switches.
	CurrentSessionTitle func() string
	// CurrentSessionTitlePending reports whether the loaded session is a new
	// branch that has not received a post-fork prompt yet.
	CurrentSessionTitlePending func() bool

	// PersistTitle stores an automatically generated title without adding a
	// user or assistant message to the transcript. It returns persistence
	// errors without affecting the main turn.
	PersistTitle func(title string) error
	// OnSessionTitleChanged updates host-side in-memory title bookkeeping
	// after a manual rename has already been written to disk.
	OnSessionTitleChanged func(title string)

	// CurrentGoal and PersistGoal expose the autonomous goal attached to the
	// current session. CurrentGoal must return a copy, not the live session
	// pointer, so failed persistence cannot mutate in-memory state. The callbacks
	// follow session switches without coupling the TUI to the session file owner.
	CurrentGoal        func() *core.SessionGoal
	CurrentGoalHistory func() []core.SessionGoal
	EnsureMission      func(objective string) error
	PersistGoal        func(goal *core.SessionGoal) error
	// PersistGoalRuntime records execution bookkeeping without adding a mission
	// transition to goal history.
	PersistGoalRuntime func(goal *core.SessionGoal) error
	// GoalMaxTokenBudget is optional. Nil means autonomous goals are unlimited.
	GoalMaxTokenBudget *uint64

	OnAssistant func(m provider.Message)

	// Extensions, if non-nil, lets users invoke extension-registered
	// slash commands. Commands declared by extensions are looked up
	// AFTER the built-in catalog so a built-in name always wins.
	Extensions *extensions.Manager

	// ResidentManager owns the /subagents runtime and provides bounded journal
	// pages plus live projections to the child-session UI.
	ResidentManager *subagents.ResidentManager
	// SpawnResident resolves the host's live registry/configuration and commits
	// a new resident child. Keeping that construction callback in agent avoids
	// leaking provider or credential assembly into modes.
	SpawnResident     func(context.Context, tools.ResidentSpawnRequest) (string, error)
	BuildResidentSpec func(context.Context, tools.ResidentSpawnRequest) (subagents.ResidentChildSpec, error)

	// ResolveSubagent validates a named markdown profile for direct
	// /subagents commands. Auto-subagents uses the equivalent callback on its
	// tool; keeping this optional preserves lightweight embedders.
	ResolveSubagent func(name string) (*subagents.Profile, error)

	// SkillSnapshot, if non-nil, returns the current list of
	// discovered SKILL.md files. Re-invoked each time /skills opens
	// so the picker reflects edits made during the session.
	SkillSnapshot func() []*skills.Skill

	// ChangelogChan, if non-nil, delivers release-notes for the
	// current binary version once at startup. Interactive opens a
	// dismissible overlay when the channel produces a non-empty
	// body. Receiver fires at most once.
	ChangelogChan <-chan ChangelogPayload

	// OnChangelogDismiss, if non-nil, is called once the user
	// closes the changelog overlay. The cli wires this to a
	// MarkChangelogShown call so the same version doesn't show
	// again on the next launch.
	OnChangelogDismiss func()

	// NoYolo is true when --no-yolo was passed. Interactive opens
	// a confirmation dialog before every tool call and blocks the
	// tool until the user picks yes / always-this-tool /
	// always-all / no. When false (default), tools run freely.
	NoYolo bool

	// ConfirmGate is the session-scoped gate wrapping this
	// interactive's Confirmer. When non-nil, /yolo can call
	// AllowAll() on it to disable confirmation for the rest of the
	// session. When nil (yolo mode), /yolo reports that there's
	// nothing to disable.
	ConfirmGate *core.ConfirmGate
}

// ChangelogPayload mirrors agent.ChangelogInfo without the import
// cycle. The cli builds one from the http response, the tui opens
// the overlay when one arrives.
type ChangelogPayload struct {
	Version string
	Body    string
	URL     string
}

type modelRefreshResult struct{ err error }

// Interactive is the TUI chat loop.
type chatCacheKey struct {
	cols                 int
	agentRev             uint64
	statusOK             string
	statusErr            string
	help                 string
	sessionInfo          string
	extNotes             string
	extStatuses          string
	extWidgets           string
	reloadErrors         string
	updateAvailable      bool
	updateCurrent        string
	updateLatest         string
	updateURL            string
	welcomeShowVer       bool
	expandAll            bool
	tailLimit            int
	renderedMessageCount int
	viewCacheRev         uint64
}

// QuickModelShortcut is one configured quick model switch slot.
type QuickModelShortcut struct {
	Provider string
	Model    string
}

type extensionStatus struct {
	Level string
	Text  string
}

type extensionWidget struct {
	Position string
	Title    string
	Lines    []string
}

// SettingsStore persists user-toggleable settings surfaced by /settings.
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

type terminalAlertsSettingsStore interface {
	SetTerminalAlertsEnabled(enabled bool) error
}

type terminalTitleSettingsStore interface {
	SetTerminalTitleEnabled(enabled bool) error
}

type lspSettingsStore interface {
	SetLSPEnabled(enabled bool) error
	SetSubagentLSPEnabled(enabled bool) error
}

type showInstructionsSettingsStore interface {
	SetShowInstructionsAtStartup(enabled bool) error
}

type autoCompactThresholdSettingsStore interface {
	SetAutoCompactThreshold(percent int) error
}

type fastModeSettingsStore interface {
	SetFastMode(enabled bool) error
}

type ponytailSettingsStore interface {
	SetPonytailEnabled(enabled bool) error
}

type webSearchSettingsStore interface {
	SetWebSearchEnabled(enabled bool) error
}

type compactContinuationOrigin uint8

type compactContinuationRequest struct {
	origin    compactContinuationOrigin
	force     bool
	lastStop  provider.StopReason
	turnError error
}

const (
	compactOriginManual compactContinuationOrigin = iota
	compactOriginPreTurnThreshold
	compactOriginAfterTurnThreshold
	compactOriginRecovery
)

type compactContinuationReason uint8

const (
	compactContinuationNone compactContinuationReason = iota
	compactContinuationStructuralTail
	compactContinuationForcedLength
	compactContinuationStatusRescue
)

const maxStatusRescueContinuations = 2

const compactHandoffVersion = 1

type compactContinuationState struct {
	reason         compactContinuationReason
	rescueAttempts int
}

type persistedCompactHandoff struct {
	Version        int    `json:"version"`
	Reason         string `json:"reason"`
	RescueAttempts int    `json:"rescue_attempts,omitempty"`
}

func decodeCompactHandoff(raw json.RawMessage) compactContinuationState {
	var persisted persistedCompactHandoff
	if len(raw) == 0 || json.Unmarshal(raw, &persisted) != nil || persisted.Version != compactHandoffVersion {
		return compactContinuationState{}
	}
	switch persisted.Reason {
	case "structural_tail":
		if persisted.RescueAttempts == 0 {
			return compactContinuationState{reason: compactContinuationStructuralTail}
		}
	case "forced_length":
		if persisted.RescueAttempts == 0 {
			return compactContinuationState{reason: compactContinuationForcedLength}
		}
	case "status_rescue":
		if persisted.RescueAttempts >= 1 && persisted.RescueAttempts <= maxStatusRescueContinuations {
			return compactContinuationState{reason: compactContinuationStatusRescue, rescueAttempts: persisted.RescueAttempts}
		}
	}
	return compactContinuationState{}
}

func encodeCompactHandoff(state compactContinuationState) json.RawMessage {
	var reason string
	switch state.reason {
	case compactContinuationStructuralTail:
		reason = "structural_tail"
	case compactContinuationForcedLength:
		reason = "forced_length"
	case compactContinuationStatusRescue:
		reason = "status_rescue"
	default:
		return nil
	}
	encoded, err := json.Marshal(persistedCompactHandoff{
		Version:        compactHandoffVersion,
		Reason:         reason,
		RescueAttempts: state.rescueAttempts,
	})
	if err != nil {
		return nil
	}
	return encoded
}

type scheduledFollowUp struct {
	text     string
	accepted chan error
}

type Interactive struct {
	cfg  InteractiveConfig
	view *tui.View

	terminalProfile   tui.TerminalProfile
	activeThemeSource *tui.ThemeSource
	themeWatchCancel  context.CancelFunc
	themeWatchEvents  chan themeWatchEvent
	themeWatchWarning string
	ed                *tui.Editor
	rend              *tui.Renderer

	mu    sync.Mutex
	agent *core.Agent
	// agentMu serializes live registry replacement with side-dialog
	// snapshots, whose copied-agent constructor reads the registry field.
	agentMu sync.Mutex
	// webSearchPolicyGeneration invalidates a resolve that began before a
	// session policy transition. The final registry commit compares its captured
	// value while agentMu and mu serialize it with live-agent replacement.
	webSearchPolicyGeneration atomic.Uint64
	// managedAutoSubagentsAddenda records exact prompt blocks appended by
	// auto-subagents. Disable only removes these owned occurrences, leaving
	// identical text that came from the user's base prompt untouched.
	managedAutoSubagentsAddenda []string
	streaming                   strings.Builder // what's currently painted on screen
	streamOn                    bool
	pendingAlert                *extproto.AlertRequest

	// streamPending is the runes buffered after each EvTextDelta that
	// haven't yet been promoted into `streaming` for rendering. It
	// exists because some provider paths (notably Anthropic via the
	// oauth/subscription channel) coalesce the model's output into a
	// few fat chunks instead of drip-streaming. Painting those fat
	// chunks verbatim looks like the summary "just appears". The
	// paintPace goroutine drains a handful of runes per tick from
	// this buffer into `streaming`, giving every path the same
	// typewriter feel regardless of upstream chunk size.
	streamPending []rune
	// streamFlushPending is set when EvAssistantMessage fires while
	// streamPending still has unrendered runes. The ticker flushes
	// them, then closes out the stream (clearing flags, resetting
	// buffers) so the final paint matches the on-disk message.
	streamFlushPending bool
	toolCalls          map[string]*tui.ToolCallView
	toolOrder          []string
	// toolRenderRevision is monotonic for the lifetime of the interactive
	// view. Per-call counters would let unrelated tools alias the same rendered
	// cache frame when they reach the same local revision.
	toolRenderRevision uint64
	// toolGate records, per tool-call id, how many runes of paced
	// assistant text must have drained into `streaming` before that
	// tool block may appear. It exists to make stream ordering
	// deterministic: a tool call can arrive from the provider while
	// the prose that logically precedes it is still being typed out
	// by the pacer. Without gating, the tool block would render
	// immediately while the intro paragraph keeps filling in below
	// it. We snapshot the total expected stream length (already
	// streamed + still pending) at the moment the tool starts, and
	// hold the block back until the pacer reaches it.
	toolGate          map[string]int
	statusErr         string
	statusOK          string
	goalStatus        core.GoalStatus
	goalRun           *goalContinuationRun
	reloadStatusSeq   uint64
	extStatuses       map[string]map[string]extensionStatus
	extWidgets        map[string]map[string]extensionWidget
	liveBlock         []string // live streaming/tool progress rendered outside scrollback
	helpBlock         []string // rendered above the chat when /help was typed
	sessionInfoBlocks []sessionInfoBlock
	cumUsage          provider.Usage
	codexUsage        codexUsageState
	lastCtxInput      int // input_tokens of the most recent turn — approximates current context size
	busy              bool
	ctrlCExit         bool
	activity          agentActivity
	pendingIdleWork   []func()
	dirty             chan struct{}
	renderScheduler   atomic.Pointer[latestFrameScheduler]
	renderRevision    atomic.Uint64
	renderOutsideLock bool
	modelRefresh      chan modelRefreshResult
	modelRefreshing   bool
	startupPreDone    chan startupPreResult
	cancelTurn        context.CancelFunc
	scrollOffset      int // rows from the bottom; 0 = pinned to latest
	prevScrollOffset  int // last value redraw snapped against; tracks intent

	// prevChatLen and prevChatCols track the chat buffer's size at the
	// last redraw so that when content grows below the user's viewport
	// while they're scrolled up reading history, we can bump
	// scrollOffset by exactly the growth and keep the visible content
	// pinned. Without this, every streamed line shifts the visible
	// window down through the buffer (because scrollOffset is measured
	// from the bottom) and the user's reading position drifts upward
	// and off the top.
	prevChatLen     int
	prevChatCols    int
	prevChatRows    int
	prevOverlayOpen bool
	cursorDimmed    bool

	// chatCache stores the built transcript/status-note rows for idle
	// frames. Editor typing changes only the bottom input region, so
	// reusing this cache avoids copying/walking/reassembling a long
	// session on every keypress.
	chatCache      []string
	chatCacheKey   chatCacheKey
	chatCacheValid bool

	// stableChatCache contains only transcript/startup rows. Live streaming,
	// tool overlays, and errors are appended separately so spinner/progress
	// frames can reuse finalized chat while a turn is busy.
	stableChatCache      []string
	stableChatCacheKey   chatCacheKey
	stableChatCacheValid bool

	// Messages typed while a turn is in flight. Each is delivered as
	// its own follow-up turn once the current one finishes. Rendered
	// above the status bar as "sliding in: ..." chips.
	queued []core.QueuedMessage

	// scheduled holds prompt-only follow-ups injected by the in-process
	// scheduler. Unlike agent.QueueMessage, these always wait until the
	// current turn has completed before starting their own turn.
	scheduled []scheduledFollowUp

	// runCtx is the top-level context passed to Run(). Follow-up turns
	// drained from `queued` are started against this context so they
	// survive past the ctx of the key event that enqueued them.
	runCtx context.Context

	// pendingPostCompactNote is a status_ok message to surface after
	// a successful auto-compact pass triggered by a 413 or by the
	// pre-turn fraction guard. Cleared by runCompact once shown.
	pendingPostCompactNote string

	// A pre-turn compaction must preserve the complete pending request,
	// including images. Overflow recovery instead continues the user message
	// already appended to the agent transcript, avoiding a duplicate message.
	pendingCompactPrompt    string
	pendingCompactImages    []provider.ImageBlock
	hasPendingCompactPrompt bool
	continueAfterCompact    bool

	// compacting is true for both manual and automatic compaction. Prompts
	// submitted while it is set stay in the host queue because Compact does
	// not run the agent loop that drains the agent-owned queue.
	compacting bool

	// autoCompacting is true while a model-triggered compaction is in
	// flight. Surfaced in the status bar so the user can tell a
	// condense pass from a regular assistant turn.
	autoCompacting bool

	// compactContinuation records the private reason and bounded rescue
	// budget for the live compaction handoff. It is never persisted or
	// inferred from rendered transcript rows.
	compactContinuation compactContinuationState

	// updateInfo is the result of the async update check. Zero value
	// while the check hasn't completed or nothing is available.
	updateInfo UpdateInfo

	dialog                  *loginDialog
	modelDialog             *modelDialog
	llamaDialog             *llamaDialog
	rescueDialog            *rescueDialog
	sessionDialog           *sessionDialog
	residentSubagentsDialog *residentSubagentsDialog
	residentChildSession    *residentChildSession
	residentChildMouse      bool
	jumpDialog              *jumpDialog
	btwDialog               *btwDialog
	skillsDialog            *skillsDialog
	changelogDialog         *changelogDialog
	confirmDialog           *confirmDialog
	logoutDialog            *logoutDialog
	telegramDialog          *telegramDialog
	settingsDialog          *settingsDialog
	floatingPane            tui.FloatingPane
	quickModelAssign        int
	telegramBridge          *telegram.Bridge
	sessionOpsDialog        *sessionOpsDialog
	sessionTreeDialog       *sessionTreeDialog
	extPanel                *extPanelDialog
	llamaConfigured         bool

	// completionTracker collects resident child terminal turns for delivery to
	// the parent orchestration wave.
	// turnCoordinator seals worker waves at manager-turn boundaries and decides
	// the next wake without relying on timing or polling.
	completionTracker         *subagents.CompletionTracker
	turnCoordinator           *orchestration.Coordinator
	coordinatorWorkerIDs      map[string][]string
	coordinatorWorkerSeq      uint64
	completionDeliveryMu      sync.Mutex
	completionDeliveryRunning bool
	completionDeliveryRequest bool
	completionDeliveryHolds   int

	// pendingFork is true when the user ran /session fork: the next
	// jump-picker selection should branch off that message instead
	// of scrolling. Flag resets after the action fires or the dialog
	// is dismissed, so repeated /jump calls don't turn into forks.
	pendingFork       bool
	suggest           *slashSuggester
	fileSuggest       *fileSuggester
	spin              *spinner
	residentAnimating atomic.Bool

	// parkedTurn is the 1-based turn number the viewport is currently
	// scrolled to by /jump. 0 = not parked, showing the tail as usual.
	// Rendered as a muted footer at the bottom of the chat so users
	// don't forget they're looking at history.
	parkedTurn  int
	parkedTotal int

	// inputHistoryIndex is -1 when not browsing history. When the
	// editor is empty, Up/Down can walk previous user prompts without
	// stealing normal vertical cursor movement in non-empty input.
	inputHistoryIndex int

	// lastCtrlC is when the user last pressed ctrl+c. The first press
	// clears the editor / cancels a turn / shows a hint; a second press
	// within ctrlCExitWindow exits. Mirrors the python-repl convention.
	lastCtrlC time.Time

	// doubleEscape tracks the optional bare-Escape session-tree gesture.
	// clock is injectable so the gesture can be tested without sleeping.
	doubleEscape doubleEscapeTracker
	clock        func() time.Time

	// welcomeStart is when the interactive run began. The welcome
	// banner shows the binary version for welcomeVersionDuration
	// after this point and reverts to plain text after.
	welcomeStart time.Time

	// interactiveStarted gates session-title generation so tests and
	// embedders that call Submit before Run do not issue a hidden request.
	interactiveStarted bool
	// titleRealPromptSeen prevents a fresh session from generating more than
	// one title. It is reset only when a fork or a new cwd starts a new branch.
	titleRealPromptSeen    bool
	titleGenerationStarted bool
	firstRealPrompt        string
	titleVersion           uint64
	titleCancel            context.CancelFunc
	sessionTitle           string

	// extNotes are one-shot styled lines pushed by extensions via
	// Notify / Display. They live above the editor (just below the
	// transcript) until cleared by /clear or another reset.
	extNotes []string

	// reloadErrors are host-only extension reload failures. They stay in
	// the scrolling chat until /clear without entering the agent transcript.
	reloadErrors []string

	// shellRunning is true while a !command is executing. It shares
	// i.busy/i.cancelTurn so esc cancels it and no turn or other shell
	// escape can start while one is in flight.
	shellRunning bool
	// shellLive is the accumulating stdout/stderr of the in-flight
	// shell escape, updated via BashTool progress for live rendering.
	shellLive string

	// awaitingStartupPre is true while the zutfile entry.pre auto-submit
	// is in flight. When it clears, deferredInitialInput is applied.
	awaitingStartupPre   bool
	deferredInitialInput string
	autoSubmitDeferred   bool

	// sessionLoading is true while a /sessions selection is being read
	// on a background goroutine. Keeping this off the input goroutine
	// lets ctrl+c/exit remain responsive for very large JSONL sessions.
	sessionLoading bool

	// sessionLoads carries per-entry results for the /sessions picker. The
	// picker owns the load lifecycle; the interactive loop applies results so
	// rendering never races with background file reads.
	sessionLoads <-chan sessionLoadEvent
	// sessionSearches carries the one-time corpus read and query-match results.
	// It stays separate from summary loading so a query edit never reopens JSONL.
	sessionSearches <-chan sessionSearchEvent

	// sessionTreeLoads delivers asynchronously-read tree rows. Like the flat
	// picker, the main loop is the sole owner of dialog mutation and rendering.
	sessionTreeLoads <-chan sessionTreeLoadEvent

	// pendingRescuePrompt / pendingRescueImages stash the prompt and
	// images that should be re-run after the user picks a model in
	// the rescue dialog. Cleared once applyRescueSelection consumes
	// them (or when the dialog is dismissed via esc).
	pendingRescuePrompt string
	pendingRescueImages []provider.ImageBlock

	clipboardImages []clipboardImageAttachment
}

// welcomeVersionDuration is how long the welcome banner shows the
// version suffix before reverting to the plain headline. 1.5s is
// enough to read at a glance and keeps the splash short.
const welcomeVersionDuration = 1500 * time.Millisecond

// initialResumeTailLimit caps how many messages from a freshly-resumed
// transcript we render on the first paint. The full transcript is
// still in memory; older messages are rendered (and their cached
// lines kept for the lifetime of the View) as soon as the user
// scrolls past the rendered tail. Picked to comfortably fill the
// largest realistic terminal viewport while keeping first paint
// snappy on multi-thousand-message sessions where markdown / syntax
// highlighting dominates the redraw cost.
const initialResumeTailLimit = 80

// resumeTailExpandStep is how many additional messages the tail
// limit grows by each time the user scrolls past the currently
// rendered top. Pre-rendering this many messages on a single tick
// keeps scroll-up smooth without falling back to a one-by-one
// reveal that would feel jerky.
const resumeTailExpandStep = 80

type startupPreResult struct {
	deferred   string
	autoSubmit bool
}

// NewInteractive constructs an Interactive from cfg.
func NewInteractive(cfg InteractiveConfig) *Interactive {
	renderer := tui.NewRenderer(cfg.Terminal)
	renderer.SetTheme(cfg.Theme)
	startupAgentName := ""
	startupContextPaths := []string(nil)
	startupExtensionNames := []string(nil)
	startupSkillNames := []string(nil)
	initialGoalStatus := core.GoalStatus("")
	if cfg.CurrentGoal != nil {
		if goal := cfg.CurrentGoal(); goal != nil {
			initialGoalStatus = goal.Status
		}
	}
	if cfg.ShowInstructionsAtStartup != nil && *cfg.ShowInstructionsAtStartup {
		startupAgentName = cfg.StartupAgentName
		startupContextPaths = append(startupContextPaths, cfg.StartupContextPaths...)
		startupExtensionNames = append(startupExtensionNames, cfg.StartupExtensionNames...)
		startupSkillNames = append(startupSkillNames, cfg.StartupSkillNames...)
	}
	profile := cfg.TerminalProfile
	if profile == (tui.TerminalProfile{}) {
		profile = cfg.Theme.Terminal
	}
	preference := cfg.EffectiveThemeName
	if preference == "" {
		preference = cfg.ThemeName
	}
	if preference == "" {
		preference = "auto"
	}
	source, sourceErr := tui.LoadThemeSource(cfg.ZutHome, preference)
	// Embedders historically supplied a ready-made Theme without a selection.
	// Preserve that explicit construction; CLI startup always supplies an
	// explicit effective preference and therefore uses the pure resolver.
	implicitReadyTheme := cfg.ThemeName == "" && cfg.EffectiveThemeName == "" && len(cfg.Theme.SpinnerFrames) != 0
	// Source loading is only a startup optimization. Invalid selected files are
	// handled by the existing invalid-selection path in the main loop.
	if sourceErr == nil && !implicitReadyTheme {
		cfg.Theme = tui.ResolveTheme(preference, source, profile).Theme
	}
	cfg.TerminalProfile = profile
	cfg.EffectiveThemeName = preference
	i := &Interactive{
		cfg:               cfg,
		terminalProfile:   profile,
		activeThemeSource: source,
		themeWatchEvents:  make(chan themeWatchEvent, 8),
		view: &tui.View{
			Theme:                 cfg.Theme,
			ImageProto:            effectiveImageProtocol(cfg.InlineImagesEnabled),
			FlatTools:             cfg.FlatTools,
			CompactUser:           cfg.CompactUser,
			CompactMode:           cfg.CompactMode != nil && *cfg.CompactMode,
			StartupAgentName:      startupAgentName,
			StartupContextPaths:   startupContextPaths,
			StartupExtensionNames: startupExtensionNames,
			StartupSkillNames:     startupSkillNames,
		},
		// Prompt is the standard half-block accent bar used by chat
		// speaker labels too, so the input gutter matches the rest
		// of the UI.
		ed:                      tui.NewEditor(cfg.Theme.AccentBar(cfg.Theme.Accent)),
		rend:                    renderer,
		toolCalls:               map[string]*tui.ToolCallView{},
		toolGate:                map[string]int{},
		dirty:                   make(chan struct{}, 8),
		modelRefresh:            make(chan modelRefreshResult, 1),
		startupPreDone:          make(chan startupPreResult, 1),
		dialog:                  newLoginDialog(),
		modelDialog:             newModelDialog(),
		llamaDialog:             newLlamaDialog(),
		rescueDialog:            newRescueDialog(),
		sessionDialog:           newSessionDialog(),
		residentSubagentsDialog: newResidentSubagentsDialog(),
		jumpDialog:              newJumpDialog(),
		btwDialog:               newBtwDialog(),
		skillsDialog:            newSkillsDialog(),
		changelogDialog:         newChangelogDialog(),
		confirmDialog:           newConfirmDialog(),
		logoutDialog:            newLogoutDialog(),
		telegramDialog:          newTelegramDialog(),
		settingsDialog:          newSettingsDialog(),
		sessionOpsDialog:        newSessionOpsDialog(),
		sessionTreeDialog:       newSessionTreeDialog(),
		extPanel:                newExtPanelDialog(),
		extStatuses:             map[string]map[string]extensionStatus{},
		extWidgets:              map[string]map[string]extensionWidget{},
		suggest:                 newSlashSuggester(),
		fileSuggest:             newFileSuggester(),
		spin:                    newSpinner(cfg.Theme),
		inputHistoryIndex:       -1,
		clock:                   time.Now,
		goalStatus:              initialGoalStatus,
		reloadErrors:            append([]string(nil), cfg.StartupExtensionErrors...),
		compactContinuation:     decodeCompactHandoff(cfg.InitialCompactHandoff),
	}
	i.btwDialog.setCloseHook(func() {
		i.confirmDialog.CancelChildConfirmations("side chat closed")
	})
	i.fileSuggest.SetRecursive(cfg.RecursiveFileSuggest != nil && *cfg.RecursiveFileSuggest)
	i.fileSuggest.SetRespectGitignore(cfg.RespectGitignore == nil || *cfg.RespectGitignore)
	if cfg.LlamaCPPConfig != nil {
		baseURL, _, err := cfg.LlamaCPPConfig()
		i.llamaConfigured = err == nil && baseURL != ""
	}
	i.managedAutoSubagentsAddenda = autoSubagentsAddenda(cfg, cfg.AutoSubagentsEnabled != nil && *cfg.AutoSubagentsEnabled)
	if cfg.ResidentManager != nil {
		// Resident updates arrive from the child control goroutine. The
		// renderer reads its immutable live projection on the next throttled
		// frame, so this callback must only request that frame.
		cfg.ResidentManager.SetUpdateObserver(func(string) { i.invalidate() })
		cfg.ResidentManager.SetHistoryUpdateObserver(func(childID string) {
			go i.reloadOpenResidentChildSession(childID)
		})
		cfg.ResidentManager.SetActivityObserver(func(active bool) {
			i.residentAnimating.Store(active)
		})
	}
	if cfg.Agent != nil {
		i.agent = cfg.Agent
		i.view.Messages = filterHiddenTranscriptMessages(cfg.Agent.Messages())
		i.cumUsage = cfg.Agent.Cost()
		// Rehydrate the "context used" gauge from the last persisted
		// turn. Without this the status bar reads 0.0% after a resume
		// until the next turn lands a usage event.
		if last := cfg.Agent.LastTurnUsage(); last.InputTokens > 0 || last.CacheReadTokens > 0 || last.CacheWriteTokens > 0 {
			i.lastCtxInput = last.InputTokens + last.CacheReadTokens + last.CacheWriteTokens
		}
		// Cap the first paint at the tail of the transcript so
		// resuming a multi-thousand-message session doesn't block
		// on rendering every prior turn before showing anything.
		if len(i.view.Messages) > initialResumeTailLimit {
			i.view.TailLimit = initialResumeTailLimit
		}
	}
	i.sessionTitle = core.NormalizeSessionTitle(cfg.InitialSessionTitle)
	if cfg.InitialSessionTitlePending {
		i.sessionTitle = ""
		i.titleRealPromptSeen = false
		i.titleGenerationStarted = false
	} else {
		i.titleRealPromptSeen = i.sessionTitle != "" || hasRealUserPrompt(i.view.Messages)
		i.titleGenerationStarted = i.titleRealPromptSeen
	}
	i.applyAutoSubagentsTool()
	i.recoverGoalRun()
	return i
}

// ExitedViaCtrlC reports whether the last Run returned because the user
// deliberately exited with Ctrl+C.
func (i *Interactive) ExitedViaCtrlC() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.ctrlCExit
}

// markCtrlCExit records that the current Run is ending via Ctrl+C.
func (i *Interactive) markCtrlCExit() {
	i.mu.Lock()
	i.ctrlCExit = true
	i.mu.Unlock()
}

// setResidentChildMouseReporting enables mouse reporting only while the
// transcript overlay owns scrolling. Normal chat keeps native terminal
// selection and wheel scrolling intact.
func (i *Interactive) setResidentChildMouseReporting(enabled bool) {
	i.mu.Lock()
	if i.residentChildMouse == enabled {
		i.mu.Unlock()
		return
	}
	i.residentChildMouse = enabled
	term, running := i.cfg.Terminal, i.runCtx != nil
	i.mu.Unlock()
	if term == nil || !running {
		return
	}
	seq := tui.SeqMouseOff
	if enabled {
		seq = tui.SeqMouseOn
	}
	_, _ = term.Write([]byte(seq))
}

// Run blocks until the user quits.
func (i *Interactive) Run(ctx context.Context) error {
	i.mu.Lock()
	i.runCtx = ctx
	i.mu.Unlock()
	i.restartThemeWatch()
	defer i.stopThemeWatch()
	term := i.cfg.Terminal
	restore, err := term.EnterRaw()
	if err != nil {
		return err
	}
	defer restore()
	i.markInteractiveStarted()
	defer i.cancelSessionTitle()
	defer i.sessionDialog.Close()
	defer func() {
		if i.telegramBridge != nil {
			i.telegramBridge.Stop()
		}
	}()

	// Enabling mouse reporting steals click-drag selection from the
	// host terminal (VS Code, Ghostty, iTerm). The user prefers native
	// selection over the wheel-speed boost, so we no longer turn it
	// on automatically. Wheel events fall through to the terminal's
	// own scrollback handler.
	// Keep zut on the terminal's main screen. We intentionally do not
	// enter the alternate-screen buffer (CSI ?1049h). The renderer emits
	// chat as normal terminal flow/scrollback and redraws only the live
	// input/status block on normal typing.
	_, _ = term.Write([]byte(tui.SeqBracketedPasteOn + tui.SeqEnhancedKeyboardOn + tui.SeqResetScrollRegion + tui.SeqDeleteKittyImages + tui.SeqClearScreenNoHome + tui.SeqClearScrollback + tui.MoveTo(1, 1)))
	// Tell the terminal our working directory (OSC 7) so "new tab /
	// split here" opens in the launch cwd instead of inheriting a stale
	// directory from an extension subprocess (see issue #38). Harmless
	// on terminals that ignore it.
	if seq := tui.ReportCWD(i.cfg.CWD); seq != "" {
		_, _ = term.Write([]byte(seq))
	}
	// Erase the live frame and place the cursor deterministically before any
	// exit summary or shell prompt is written. Do not erase scrollback: users
	// should still be able to review the session after closing zut.
	mode2031Owned := false
	defer func() {
		cleanup := tui.SeqResetScrollRegion + tui.SeqDeleteKittyImages + tui.SeqMouseOff + tui.SeqEnhancedKeyboardOff + tui.SeqBracketedPasteOff + tui.ResetCursorColor() + tui.ResetCursorShape() + tui.SeqClearScreenNoHome + tui.MoveTo(1, 1) + tui.SeqShowCursor
		if mode2031Owned {
			cleanup = tui.SeqDisableMode2031 + cleanup
		}
		_, _ = term.Write([]byte(cleanup))
	}()
	i.applyInputCursorColor()

	// Streaming pacer: drains buffered text deltas at a steady rate
	// so typewriter feel is identical across providers regardless of
	// upstream chunk size. Starts here so it lives for the whole
	// session and exits with ctx.
	go i.runStreamPacer(ctx)

	cols, rows := term.Size()
	i.rend.Resize(cols, rows)
	renderScheduler := newLatestFrameScheduler()
	i.renderScheduler.Store(renderScheduler)
	go renderScheduler.run(func(req renderRequest) {
		c, r := term.Size()
		i.rend.Resize(c, r)
		if req.theme != nil {
			i.rend.SetTheme(*req.theme)
		}
		if req.clear {
			i.rend.Clear()
		}
		if req.invalidate {
			i.rend.Invalidate()
		}
		i.redraw()
	})
	defer renderScheduler.stop()
	term.OnResize(func() {
		// Resize and redraw share the renderer owner. The owner reads the
		// latest terminal size just before its next frame.
		i.renderRevision.Add(1)
		renderScheduler.request(false, false)
	})

	if i.cfg.StartupPre != "" {
		i.deferredInitialInput = i.cfg.InitialInput
		i.autoSubmitDeferred = i.cfg.AutoSubmitInitial
		i.awaitingStartupPre = true
		i.Submit(i.cfg.StartupPre)
	} else if i.cfg.AutoSubmitInitial && i.cfg.InitialInput != "" {
		i.Submit(i.cfg.InitialInput)
	} else {
		if i.cfg.InitialInput != "" {
			i.ed.SetValue(i.cfg.InitialInput)
		}
		i.startRestoredCompactHandoff(ctx)
	}

	// Stamp the welcome time and schedule a one-shot redraw at the
	// expiry so the version suffix disappears on its own even if the
	// user hasn't typed anything yet.
	i.welcomeStart = time.Now()
	time.AfterFunc(welcomeVersionDuration, i.invalidate)

	// If the agent was constructed with a pre-loaded transcript
	// (--continue, --resume, --session) pin the viewport at the
	// bottom so the most recent reply (and any prompt the user just
	// typed) is fully visible. Earlier behaviour parked the view at
	// the last user turn, which could leave the latest message clipped
	// off the bottom of the page on long sessions.
	if i.agent != nil {
		if msgs := i.agent.Messages(); len(msgs) > 0 {
			i.scrollToBottom()
		}
	}

	// No credential at startup? Auto-open the login dialog, and mark
	// the status line. The user can Esc out of the dialog if they
	// want to dismiss it (e.g. to check /help or /exit first).
	if i.agent == nil {
		i.statusErr = "not logged in. pick a login method below or press esc to dismiss."
		i.dialog.Open(i.cfg.ZutHome)
	}

	// The appearance-aware source is the only stdin reader. It consumes only
	// replies for queries currently in flight, keeps bracketed-paste payload
	// opaque, and sends every other byte through the established key parser.
	keys := make(chan tui.Key, 256)
	appearanceEvents := make(chan tui.InputEvent, 256)
	appearanceParser := &tui.AppearanceParser{}
	appearanceParser.SetPendingColors(true)
	appearanceParser.SetPendingScheme(true)
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		source := tui.NewAppearanceSource(term.ReadByte, term.PeekByteTimeout, appearanceParser, func(event tui.InputEvent) {
			select {
			case appearanceEvents <- event:
			case <-ctx.Done():
			}
		})
		reader := tui.NewReader(source.ReadByte)
		for {
			k, err := reader.Read()
			if err != nil {
				return
			}
			select {
			case keys <- k:
			case <-ctx.Done():
				return
			}
		}
	}()
	// Probe before mutating mode 2031. Replies are handled in the main loop so
	// ownership and profile publication stay serialized with rendering.
	_, _ = term.Write([]byte(tui.SeqQueryMode2031 + tui.AppearanceQuery()))
	defer func() {
		// Test terminals and normal EOF join immediately. A physical tty read
		// may be uncancellable on some platforms, so shutdown never waits
		// indefinitely for a kernel read that cannot be interrupted.
		select {
		case <-inputDone:
		case <-time.After(50 * time.Millisecond):
		}
	}()

	// Subscribe to auth events.
	var authEvents <-chan auth.Event
	if i.cfg.AuthManager != nil {
		authEvents = i.cfg.AuthManager.Events()
	}

	// Animation ticker: drives spinner and dialog-related redraws when
	// nothing else changed. 120ms is slow enough that highlighting a huge
	// transcript doesn't spin the cpu.
	tick := time.NewTicker(120 * time.Millisecond)
	defer tick.Stop()
	i.refreshCodexUsage(ctx, time.Now())
	defer i.resetCodexUsage()

	// Redraw throttle: coalesce bursts of invalidate() calls so we paint
	// at most once every redrawMinInterval. Huge tool-result dumps can
	// fire hundreds of invalidations while the user is typing; without
	// this, the input goroutine never gets CPU and keystrokes lag.
	const redrawMinInterval = 16 * time.Millisecond
	var lastRedraw time.Time
	var pendingRedraw bool
	var pendingTimer *time.Timer

	drainPending := func() {
		if pendingTimer != nil {
			pendingTimer.Stop()
			pendingTimer = nil
		}
		if pendingRedraw {
			pendingRedraw = false
			lastRedraw = time.Now()
			renderScheduler.request(false, false)
		}
	}

	requestRedraw := func() {
		since := time.Since(lastRedraw)
		if since >= redrawMinInterval {
			// Redrawing right now subsumes any pending redraw, so clear
			// the throttle state. Without this, a pending flag stays
			// stuck at true and subsequent invalidate() calls within
			// redrawMinInterval get dropped — which is exactly how the
			// final "turn finished" frame went missing until the user
			// nudged the ui by typing or scrolling.
			if pendingTimer != nil {
				pendingTimer.Stop()
			}
			pendingRedraw = false
			lastRedraw = time.Now()
			renderScheduler.request(false, false)
			return
		}
		if pendingRedraw {
			return // already scheduled
		}
		pendingRedraw = true
		wait := redrawMinInterval - since
		if pendingTimer == nil {
			pendingTimer = time.AfterFunc(wait, func() {
				// Poke the dirty channel so the main loop wakes and
				// drains the pending redraw on its own goroutine. We
				// can't call drainPending here directly — it touches
				// closure state shared with the main loop.
				i.invalidate()
			})
		} else {
			pendingTimer.Reset(wait)
		}
	}

	i.invalidate()

	updates := i.cfg.UpdateInfoChan  // nil-safe; nil channel blocks forever in select
	changelog := i.cfg.ChangelogChan // single-shot, see case below
	profileCandidate := i.terminalProfile
	profileDirty := false
	appearanceDeadline := time.Now().Add(500 * time.Millisecond)
	lastAppearancePoll := time.Now()
	modeEnableRequested := false
	notificationsSupported := false

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-i.themeWatchEvents:
			if i.activeThemeSource == nil || event.path != i.activeThemeSource.Path {
				break
			}
			switch {
			case event.source != nil:
				i.activeThemeSource = event.source
				i.themeWatchWarning = ""
				i.installResolvedTheme(tui.ResolveTheme(i.cfg.EffectiveThemeName, event.source, i.terminalProfile))
				i.mu.Lock()
				i.statusOK = "theme reloaded"
				i.statusErr = ""
				i.mu.Unlock()
			case event.deleted:
				i.activeThemeSource = nil
				i.stopThemeWatch()
				i.cfg.ThemeName = ""
				i.cfg.EffectiveThemeName = "auto"
				if i.cfg.SettingsStore != nil {
					_ = i.cfg.SettingsStore.SetTheme("auto")
				}
				i.installResolvedTheme(tui.ResolveTheme("auto", nil, i.terminalProfile))
				i.mu.Lock()
				i.statusOK = "theme removed; using auto"
				i.statusErr = ""
				i.mu.Unlock()
			case event.err != nil && event.err.Error() != i.themeWatchWarning:
				i.themeWatchWarning = event.err.Error()
				i.mu.Lock()
				i.statusErr = "theme reload: " + event.err.Error()
				i.mu.Unlock()
			}
			i.invalidate()
		case event := <-appearanceEvents:
			switch {
			case event.Color != nil:
				switch event.Color.Kind {
				case 10:
					profileCandidate.Foreground = event.Color.Color
					profileCandidate.HasForeground = true
				case 11:
					profileCandidate.Background = event.Color.Color
					profileCandidate.HasBackground = true
				case 4:
					profileCandidate.Palette[event.Color.Slot] = event.Color.Color
					profileCandidate.PaletteKnown |= uint16(1) << event.Color.Slot
				}
				profileDirty = true
			case event.Scheme != nil:
				profileCandidate.Light = event.Scheme.Light
				profileCandidate.SchemeKnown = true
				profileDirty = true
				if notificationsSupported && !event.Scheme.Solicited {
					// A scheme notification means the terminal defaults and palette
					// may have changed too. Refresh the complete profile atomically.
					appearanceParser.SetPendingColors(true)
					appearanceParser.SetPendingScheme(true)
					appearanceDeadline = time.Now().Add(500 * time.Millisecond)
					_, _ = term.Write([]byte(tui.AppearanceQuery()))
				}
			case event.Mode != nil && event.Mode.Mode == 2031:
				switch event.Mode.Status {
				case 1, 3:
					notificationsSupported = true
					appearanceParser.SetNotifications(true)
					if modeEnableRequested {
						mode2031Owned = true
					}
				case 2:
					modeEnableRequested = true
					_, _ = term.Write([]byte(tui.SeqEnableMode2031 + tui.SeqQueryMode2031))
				default:
					appearanceParser.SetNotifications(false)
				}
			}
			// The parser gate closes after a bounded collection window; accepted
			// notifications remain enabled separately once mode 2031 is known.
			if profileDirty {
				i.invalidate()
			}
		case k := <-keys:
			if done := i.handleKey(ctx, k); done {
				return nil
			}
			// Drain any keystrokes that arrived during this iteration.
			// VS Code (and other terminals that don't bracket drops as
			// paste) deliver a path one rune at a time — without this
			// loop the editor would render between every rune and a
			// long path on a heavy transcript would visibly type in.
		drain:
			for {
				select {
				case k2 := <-keys:
					if done := i.handleKey(ctx, k2); done {
						return nil
					}
				default:
					break drain
				}
			}
			i.invalidate()
		case ev := <-authEvents:
			i.handleAuthEvent(ev)
			if ev.Provider == "openai" || ev.Provider == "openai-codex" {
				i.resetCodexUsage()
			}
			i.invalidate()
		case result := <-i.modelRefresh:
			i.mu.Lock()
			i.modelRefreshing = false
			i.mu.Unlock()
			i.openModelPickerAfterRefresh(result.err)
			i.invalidate()
		case result := <-i.startupPreDone:
			i.applyStartupPreResult(result)
			i.invalidate()
		case info, ok := <-updates:
			if ok && info.Available {
				i.mu.Lock()
				i.updateInfo = info
				i.mu.Unlock()
				i.invalidate()
			}
			updates = nil // single-shot; subsequent iterations skip this case
		case cl, ok := <-changelog:
			if ok && cl.Body != "" {
				i.changelogDialog.Open(cl.Version, cl.URL, cl.Body)
				i.invalidate()
			}
			changelog = nil // single-shot
		case event, ok := <-i.sessionLoads:
			i.mu.Lock()
			if !ok {
				i.sessionLoads = nil
				i.sessionDialog.ApplyLoadClosed()
				i.mu.Unlock()
				i.invalidate()
				break
			}
			i.sessionDialog.ApplyLoad(event)
			i.mu.Unlock()
			i.invalidate()
		case event, ok := <-i.sessionSearches:
			i.mu.Lock()
			if !ok {
				i.sessionSearches = nil
				i.mu.Unlock()
				break
			}
			i.sessionDialog.ApplySearch(event)
			i.mu.Unlock()
			i.invalidate()
		case event, ok := <-i.sessionTreeLoads:
			i.mu.Lock()
			if !ok {
				i.sessionTreeLoads = nil
				i.sessionTreeDialog.ApplyLoadClosed()
				i.mu.Unlock()
				i.invalidate()
				break
			}
			if err := i.sessionTreeDialog.ApplyLoad(event); err != nil {
				i.sessionTreeLoads = nil
				i.statusErr = "tree: no readable session family"
				i.statusOK = ""
			}
			i.mu.Unlock()
			i.invalidate()
		case <-i.dirty:
			requestRedraw()
		case <-tick.C:
			now := time.Now()
			i.refreshCodexUsage(ctx, now)
			if !appearanceDeadline.IsZero() && !now.Before(appearanceDeadline) {
				appearanceParser.SetPendingColors(false)
				appearanceParser.SetPendingScheme(false)
				appearanceDeadline = time.Time{}
			}
			if profileDirty {
				i.applyTerminalProfile(profileCandidate)
				profileDirty = false
			}
			if !notificationsSupported && now.Sub(lastAppearancePoll) >= 2*time.Second {
				appearanceParser.SetPendingColors(true)
				appearanceParser.SetPendingScheme(true)
				appearanceDeadline = now.Add(500 * time.Millisecond)
				lastAppearancePoll = now
				_, _ = term.Write([]byte("\x1b]11;?\a" + tui.SeqQueryColorScheme))
			}
			// Always drain a pending redraw on the tick. This is the
			// safety net that catches the case where the dirty channel
			// was saturated when the final "turn finished" invalidate
			// fired, or where the throttle scheduled a deferred redraw
			// and the AfterFunc-driven invalidate got dropped on a
			// full channel.
			drainPending()
			// Only force a periodic redraw when something is actually
			// animating (the main spinner during a busy turn, or the
			// btw side-chat spinner while it's awaiting a response).
			// Static pickers (model, completed session, jump, etc.) don't
			// need the tick and firing it cancels the terminal's cursor
			// blink inside dialogs that host their own editor (btw),
			// because each frame re-emits hide-cursor + show-cursor.
			// A session picker that is still loading is an exception so
			// its dialog-local spinner can advance.
			//
			i.mu.Lock()
			busy := i.busy
			sessionLoading := i.sessionDialog.Loading()
			sessionSearching := i.sessionDialog.SearchLoading()
			i.mu.Unlock()
			if busy || sessionLoading || sessionSearching || i.btwDialog.Loading() || i.residentAnimating.Load() {
				requestRedraw()
			}
		}
	}
}

const maxExtensionWidgetRows = 12
const maxExtensionStatusRows = 6

// sessionsRoot returns the session namespace for this run.

// lastCols returns the current terminal width in columns.

// chatPage returns the number of chat rows currently visible, used
// as the page size for PageUp/PageDown.

// scrollBy adjusts the scroll offset. Positive = up (into history).
// Clearing the parked-turn label when we're back at the bottom means
// the "viewing turn N" footer goes away automatically as soon as you
// scroll back to the live tail.

// anchorScrollOffset keeps the user's reading position pinned when the
// chat buffer grows/shrinks or the viewport height changes between two
// redraws while they're scrolled up.
//
// scrollOffset is measured from the bottom of the chat buffer, so the
// top visible row is start = chatLen - scrollOffset - chatRows. To hold
// `start` constant we adjust the offset by the buffer-length delta minus
// the viewport-height delta. The result is clamped to [0, newLen] so a
// shrinking buffer can't push it negative.

// scrollToBottom pins the view to the latest content.

// snapViewportStartToImageBlock treats inline images as atomic blocks for
// scrolling. Terminal image protocols draw from a single escape row into a
// separate graphics layer; the following blank rows are only zut's reserved
// footprint. If the viewport starts on one of those blank rows, there is no
// correct partial-image state to render. Snap back to the escape row instead
// so the image is either shown from its beginning or skipped entirely.

const (
	hiddenOpenAIImageMirrorPrefix = "Tool output included the following image content:"
	autoCompactContinueMetaKey    = "auto_compact_continue"
	shellEscapeMetaKey            = "shell_escape"
)

// isBoxBlankLine reports whether line is visually empty after
// stripping ANSI escape sequences, surrounding whitespace, and the
// vertical box edges drawn by the tool-box renderer. Used by
// clipBottomClippedImages so an image's reservation rows still count
// as blank when those rows are wrapped in "│  ...  │" inside a tool box.

// stripANSIBytes removes ANSI CSI escape sequences (ESC '[' ... final
// byte) from s without pulling in the regexp package. Mirrors the
// internal helper in package tui; the duplicated copy avoids exporting
// it just for one caller.

// truncateLine shortens s so it fits within n display cells, with an
// ellipsis if trimmed. Used by the "sliding in" chips so a pasted
// novel doesn't blow past the status line.

// ctrlCExitWindow is how long after a ctrl+c press a *second* press
// will exit instead of just clearing input. Long enough to be
// deliberate (rules out accidental key chord), short enough that the
// hint stays meaningful.
const ctrlCExitWindow = 2 * time.Second

// armCtrlCExit records the timestamp of the current ctrl+c so the next
// one within ctrlCExitWindow exits.

// ctrlCExitArmed reports whether a previous ctrl+c was recent enough
// that another press should now exit.

// clearFileSuggestQuery strips the filter the user typed after the
// last "@", leaving the bare "@" so the picker stays open. Called when
// navigating between directory levels (Right/Left): the filter applied
// to the level the user was on, not the one being entered, so carrying
// it forward would wrongly hide the new directory's contents.

// setToolExpansion updates long tool result expansion and replays the
// transcript so already-emitted rows reflect the new state.

// confirmChildActive reports whether an interaction opened from slash input
// currently owns the keyboard while a tool confirmation remains pending.

// restoreConfirmFocus returns input to confirmation after slash input is
// cleared or a child interaction closes. A non-empty editor keeps command
// input focused, and an active child continues to own its keys.

// invokeExtensionCommand fires an extension-registered slash command
// in a background goroutine, awaits the response, and applies the
// requested action (prompt / insert / display / noop). Errors and
// timeouts surface as a status_err line.

// appendExtensionNote renders an extension-originated note in the
// chat. Levels: "info" (muted), "warn" (warning), "error" (error),
// "success" (tool/ok green).

// HostHooks implementation for the extension manager. The manager
// holds an interface, not a concrete *Interactive, so these methods
// are the only thing the manager sees.

// ApplySessionAgent swaps the live agent after a validated session resume.
// Unlike ApplyChangedCWD it does not mutate cwd-scoped resources or startup
// context; the session loader is changing the transcript/provider only.

// SetSubagentSessionScope redraws after a host session transition. Resident
// children retain their explicit parent session in their durable journal.

// ApplyChangedCWD is called by hosts after a successful /cd hook that do
// not provide startup context metadata.

// ApplyChangedCWDWithStartupContext swaps a rebuilt agent and its display-only
// startup context into the running TUI after a successful /cd hook.

// SubmitSlash runs text as a slash command in the TUI as if the user
// had typed it. text must start with '/' — callers that hand it
// plain prose silently get a no-op so a misbehaving extension can't
// run a stray prompt through this path. Read-only commands run in
// place; commands that would mutate the transcript or replace the
// agent cancel the active turn first via the same path the editor
// uses for typed slash commands.

// SubmitOrQueue runs a text-and-image prompt immediately if the agent is idle,
// or appends it to the pending queue if a turn is already in flight. Used by
// the Telegram bridge and editor submit path so both input sources share the
// same queue semantics.

// CancelTurn aborts the active turn if one is running. Used by the
// telegram bridge when the paired user sends /stop.
// ChangelogVersion returns the version string of the changelog
// currently shown (or last shown). Used by the dismiss callback
// to store the correct version for dev builds.

// Insert places text at the cursor in the editor.

// Display appends a styled note from extName to the chat without a
// model call.

// ReportError surfaces a host-side error in the interactive status area.

// SetStatus replaces one persistent status item owned by an extension.

// SetWidget replaces one persistent widget owned by an extension.

// ClearWidget removes one persistent widget owned by an extension.

// ClearExtensionChrome removes every persistent UI item owned by an
// extension that has exited. This keeps crashes, disable/reload operations,
// and startup failures from leaving stale widgets beside the transcript.

const modalBackdropDimPercent = 50

// setInputCursorDimmed updates the terminal-owned cursor only when its layer
// changes. Reapplying cursor controls on every redraw can reset its blink
// timing before the terminal completes a normal blink cycle.

// stripWebSearchTool removes the callable web_search entry from the live
// registry without asking the resolver to run again. It is the fail-closed
// fallback when persistence rollback fails after a refresh failure.

// buildStudyPrompt returns the canned prompt the /study command
// submits to the agent.
//
// With no argument, /study targets the current directory — the
// historical behaviour. With an argument, /study targets that path
// instead; either a directory ("read every file in here") or a
// single file ("read this file"). The argument can be:
//
//   - a relative path (resolved against cwd)
//   - an absolute path
//   - an @-picker chip, which has already been expanded to an
//     absolute path by expandFileChips before runSlash sees it
//
// The path is stat'd to pick the right wording ("directory" vs
// "file"). If the path doesn't exist, we still build a sensible
// prompt rather than erroring — the agent will surface the
// missing-file failure itself when it tries to read it, which is
// more useful than a refusal here.

// tryPathTabCompleteEditor looks at ed's current value, finds the
// path-like token immediately before the cursor (the cursor is always
// at the end of the buffer after a keystroke, so "before the cursor"
// is the trailing non-whitespace run), and rewrites it to its shell-
// style completion against the filesystem.
//
// Returns true when it consumed the Tab keystroke (token recognised,
// completion attempted — even if no candidates matched, the keystroke
// is still consumed so it doesn't insert a literal tab character).
// Returns false when the token doesn't look like a path; callers then
// let Tab fall through to its normal no-op.
//
// Recognised path shapes:
//   - ~ or ~/foo                  expanded via os.UserHomeDir()
//   - /abs/path or /abs/path/foo  absolute
//   - ./foo, ../foo, foo/bar      relative to cwd
//
// A bare word like "hello" is not treated as a path so plain text
// keeps Tab as a literal no-op.
//
// Free function (not a method) so the same logic runs against the
// editor instances owned by local dialogs without each
// dialog needing its own copy.

// tryPathTabComplete is the Interactive-bound convenience wrapper.
// It calls the free helper against the main editor and invalidates
// the frame on a successful rewrite.

// looksLikePathToken reports whether tok is shaped like a filesystem
// path. Paths must either start with ~, /, ./, ../ or contain a /.
// Plain words are excluded so Tab on "hello" stays a no-op.

// resolvePathTabToken splits tok into (absolute parent dir, basename
// prefix to match, display-form parent the user typed). ok is false
// when the parent dir can't be resolved (e.g. ~ with no $HOME).

// splitDirBase is like filepath.Split but preserves the trailing
// slash convention: "foo" => (".", "foo"); "foo/" => ("foo", "");
// "a/b" => ("a/", "b"); "/" => ("/", ""). Returned dir always has
// the trailing separator when non-empty so callers can rebuild paths
// by concatenation.

// openLogoutDialog shows the provider picker for `/logout` with no
// argument. Only providers the user is currently logged into are
// listed, plus an "all" entry when more than one is present. If
// nothing's logged in, writes a status line instead of opening an
// empty dialog.

// doLogout clears credentials for the given provider (or all providers)
// from auth.json. If the active agent was using those credentials, it
// is torn down so the user is forced through /login before their next
// prompt.
//
// target: "anthropic" | "openai" | "kimi" | "github-copilot" | "all"

// applyModelSelection switches the active model (and provider, if the
// new model belongs to a different one). It rebuilds the underlying
// client when needed so the provider wire-protocol matches.
// cancelAndWaitForIdle cancels the active turn (if any) and blocks
// briefly until the turn goroutine has updated i.busy = false. Used
// before destructive slash commands so transcript-mutating work
// (/clear, /compact, /logout, /login completion, cross-provider
// /model swap) doesn't race with the still-running stream.
//
// The wait is bounded; if the turn doesn't release within the timeout
// we proceed anyway. Worst case is a brief overlap that the agent's
// own mutex protects against.

// openBtwDialog opens the side-chat overlay with a frozen snapshot
// of the current main session. The optional argument is auto-
// submitted as the first question, so '/btw does X work?' fires the
// model call immediately instead of just opening an empty dialog.

// submitOrQueuePrompt submits a slash command's expanded prompt immediately,
// or queues it behind the active turn.

// openSkillsDialog opens the skill inspector. The picker reflects
// whatever SkillSnapshot returns at call time, so edits to a
// SKILL.md made during a session show up on the next /skills.

// openJumpDialog builds a /jump picker from the current transcript.
// If the user typed "/jump foo" with a filter and it matches exactly
// one turn, jump there directly without showing the dialog.

// applyJumpSelection scrolls the chat viewport so the user message at
// msgIdx is visible at (or near) the top of the chat area. Uses the
// anchor slice returned by view.BuildWithAnchors so the mapping from
// message index to row is exact, regardless of variable-height tool
// blocks above the target.

// totalTurnsLocked counts user messages in the transcript. Caller is
// assumed to hold i.mu (the name is a mild reminder; this function
// itself doesn't touch shared state beyond the slice it's handed).

// applySessionSelection loads the given session via the cli-provided
// callback and snaps the viewport to the bottom (the latest message)
// so the user lands at the live tail of the resumed conversation.

// applyRescueModelSelection is like applyModelSelection but routes
// through BuildAgentForRescue so launch-time --api-key / --base-url
// overrides are dropped before the new agent is built. Falls back to
// the regular builder when the host doesn't wire a rescue builder.

// swapModel applies a /model selection (or a rescue selection) using
// the supplied builder. rescue=true tags the success message so the
// user can see that launch-time overrides were ignored.

// clearPendingCompactTurnLocked drops work that only makes sense after the
// current compaction. The caller must hold i.mu.

// compactHandoffLocked returns a self-contained persistence snapshot. The
// caller must hold i.mu.

// setCompactContinuationLocked updates the private handoff state and returns
// its persistence snapshot plus whether persistence is needed. The caller must
// hold i.mu.

// resetCompactContinuationLocked clears the private handoff state and returns
// its persistence snapshot plus whether persistence is needed. The caller must
// hold i.mu.

// autoCompactContinuationPrompt is an interactive-only compact-handoff
// instruction. Its completion criteria must stay separate from Ponytail so
// toggling the optional coding-style addendum never changes recovery behavior.
const autoCompactContinuationPrompt = `Context was compacted while work was in progress. Continue the user's most recent active request now; do not wait for them to type "continue". Inspect the context summary and kept recent messages, treating active constraints and preferences there as still in force, including delegation/subagent instructions. If a newer request supersedes earlier plans in the summary, follow the newer request. A progress report, plan update, or statement of future work is not completion: if required work remains, take the next concrete action now. Only finish when the request is actually complete or when a specific user decision is required. If nothing remains, give a brief truthful completion; do not invent work or force a tool call.`

type compactHandoffResume uint8

const (
	compactHandoffDiscard compactHandoffResume = iota
	compactHandoffContinueExisting
	compactHandoffAppendPrompt
)

// classifyCompactHandoffResume validates that the persisted checkpoint still
// describes the effective transcript. A normal assistant reply or a newer
// explicit user message supersedes the checkpoint; tool activity remains an
// unfinished existing turn. No hidden prompt means the process stopped after
// recording the handoff but before appending its context message.

// startRestoredCompactHandoff resumes a handoff checkpoint restored from the
// active session. When its hidden context message was already persisted, the
// agent continues that existing turn rather than appending it again.

// startAutoCompactContinuation adds an internal user turn that tells the
// model to resume the task after threshold compaction. Provider requests
// cannot continue from an assistant tail on every supported API, so the
// continuation must be represented as a user message. It is hidden from
// the rendered transcript but remains in the persisted/model context.

// runCompact invokes core.Agent.Compact and reflects the progress in
// the tui. It runs in a goroutine so the ui stays responsive; esc/ctrl+c
// cancel via the same cancelTurn channel used for normal turns.
//
// When auto is true the spinner message is pinned to "condensing
// history" and the status bar surfaces "(auto)" next to the context
// percentage so it's obvious the system triggered this, not the user.
// request.force tells the post-compact hand-off to resume even if the
// transcript tail has visible assistant text, for example after StopLength.

// shellEscapeCommand reports whether text is a "!command" shell
// escape and, if so, returns the command with the leading '!' (and
// surrounding whitespace) stripped. A bare "!" with no command is
// treated as not an escape so it falls through to the normal prompt
// path rather than running an empty shell.

// ShellEscapeCommand reports whether text is a "!command" shell
// escape and, if so, returns the command with the leading '!' (and
// surrounding whitespace) stripped. A bare "!" with no command is
// treated as not an escape so it falls through to the normal prompt
// path rather than running an empty shell.
const ShellEscapeTimeoutSeconds = 600

// startShellEscape runs a "!command" in the same shell the bash tool
// uses, in the session working directory, honoring the /jail sandbox.
// It shares the busy/cancel state with the agent: esc cancels it, and
// it refuses to start while a turn or another shell escape is already
// in flight. Its terminal-log output is appended to the transcript as
// user context without automatically starting a model turn.

// startTurnRequest starts a new prompt or continues an existing user turn.
// continueExisting selects Agent.Continue so callers can retry a transcript
// message without appending it twice. overflowRecoveryAttempted suppresses
// the pre-turn threshold guard and the one-shot overflow recovery path; a
// normal post-compaction continuation leaves it false so a still-too-large
// rebuilt context can recover once more.

// openRescueDialog surfaces the rescue model picker after a recoverable
// provider failure. The pending prompt + images are stashed on the
// Interactive so a later applyRescueSelection can re-run the same turn
// against the freshly-picked model. activeProvider/failedProvider are
// usually the same, but some clients embed different prefixes in their
// errors than the configured provider id, so we accept both.

// applyRescueSelection switches model (cross-provider if needed) and
// re-runs the same prompt+images that just failed. Mirrors
// applyModelSelection's transcript-carry logic so the user keeps full
// session continuity across the swap.

// autoCompactNoteLine returns a styled chat-area note for the
// inline auto-compact heads-up. Lives in extNotes so it survives
// the busy-spinner overwrite of the status row.

// NormalizeAutoCompactThreshold applies the persisted auto-compaction
// setting's supported values and default. It is exported so non-interactive
// hosts can share the interactive mode's exact configuration semantics.

// ShouldAutoCompact reports whether the latest request context reached the
// configured percentage of the model's advertised context window.

// classifyCompactionContinuation chooses a private handoff reason from the
// successful turn tail. Structural unfinished-tail handling is independent of
// the status matcher. A status rescue is admitted only for an after-turn
// threshold compaction or an already-active status handoff, and only after a
// normal successful end.

// structuralCompactionContinuation preserves the pre-existing unfinished-tail
// behavior without consulting the status matcher.

// likelyForwardWorkStatus is deliberately narrow. It recognizes only clear
// first-person future-work commitments and never treats generic plan prose as
// a host control signal.

// shouldAutoCompactLocked reports whether the last turn pushed context
// usage past the auto-compact threshold. Must be called with i.mu
// held; it reads lastCtxInput and the current model's context window.

// WebSearchPolicyGeneration returns the generation a resolver must present at
// its final interactive commit. Policy transitions advance it before changing
// the gate or generic-child ceiling.

// ApplyAgentPromptConfig commits a resolved prompt and tool registry only if
// ag is still the live interactive agent. Callers without a long-running
// resolve use the current policy generation.

// ApplyAgentPromptConfigAtWebSearchGeneration additionally rejects a registry
// resolved before a web-search policy transition. This final check is made in
// the same critical section as live-agent replacement and the core prompt swap.

// prepareReplacementAgentLocked applies session-wide capability policy before
// ag becomes visible to prompt submission. The caller holds agentMu and i.mu.

// DeferUntilIdle runs work immediately when the interactive agent is idle,
// or after the current turn/compaction/shell operation releases the busy
// state. Callbacks run outside i.mu.

// Agent returns the current agent, if any. Used by cli.go to flush the
// final transcript to the session file.

// silence unused import in some build configs
var _ = fmt.Sprintf

// runReloadExt triggers a live reload of every extension (discovered
// + explicit). Runs on a goroutine so the TUI stays responsive; the
// Manager.Reload takes a couple of hundred ms to shut down subprocs
// and respawn them. Shows a temporary status line throughout.

// Confirm implements core.Confirmer. The agent goroutine calls
// this synchronously before every tool invocation when --no-yolo is
// active. We push the request onto the confirmDialog queue, trigger
// a redraw, and block the caller until the user answers.
//
// If the session is cancelled or the TUI exits mid-prompt, any
// pending request is refused via CancelAll so the agent doesn't
// deadlock.

// ConfirmToolCall attaches a side-effect-free preview to the matching live
// tool panel, then blocks until the user approves or refuses the call.

// openTelegramDialog shows the picker for `/telegram` with no arg.
// Items depend on current state: disconnect + status when running,
// connect + status when stopped.

// telegramMenuItems builds the dialog entries for the current
// bridge state. Returns empty when no bot.json exists so the
// caller can show a helpful status line instead of an empty menu.

// doTelegram dispatches one of the three explicit actions. Called
// from /telegram <action> or after the picker selects a row.

// telegramConnect starts the bridge. Refuses if it's already
// running or if the on-disk bot.json is missing a token.

// telegramDisconnect stops the bridge. No-op when already stopped.

// refreshToolsAfterTelegram restores normal tools only through a successful
// resolver refresh. Failure removes Telegram-only tools but leaves web_search
// revoked in the live registry, stale snapshots, and generic-child policy.

// telegramSenderAdapter wraps the bridge so the tools package can
// drive it without importing telegram directly. The Active() check
// is forwarded to the bridge so the tool can fail clearly with a
// model-readable error when the user disconnected mid-turn.
type telegramSenderAdapter struct {
	bridge *telegram.Bridge
}

// TrackResidentSubagent reserves completion delivery after the manager has
// durably accepted a resident child prompt and before it is scheduled.

// ReportResidentSubagent delivers a typed resident terminal outcome through
// the same coordinator path used by legacy worker completions.

// registerCoordinatorWorker associates a tracker registration with the open
// manager wave. Direct embedders that register a worker outside startTurn get
// a one-worker sealed wave, retaining the exported tracker API's old behavior.

// requestCompletionDelivery starts at most one waiter for the current active
// set. A request arriving while that waiter is formatting or submitting is
// picked up before the waiter exits, so a later worker cannot be lost in the
// handoff between batches.

// beginCompletionDeliveryHold keeps all completions observed during one
// parent model turn together. Tool calls are executed sequentially inside the
// core agent, so the release point is the owning registration boundary for
// same-parent-turn batching rather than a timing-based debounce.

// autoSubagentsAddenda returns the prompt blocks owned by subagent prompting,
// in the same order Resolve appends them to a new agent. The profile manifest
// is independent of strict orchestration and remains available in on-demand
// mode whenever the primary agent can spawn a worker.

// removeLastAutoSubagentsAddendum removes one occurrence of a block known to
// have been appended by auto-subagents. Resolve appends owned blocks at the
// end, so removing the last occurrence preserves an identical base block.

// applyAutoSubagentsSystemPrompt swaps the prompt blocks owned by subagent
// prompting on the running agent. Toggling strict orchestration changes the
// delegation guidance while retaining the profile manifest when usable.

// applyAutoSubagentsTool registers the canonical subagent tools whenever
// launch-time policy permits them. Auto-subagents controls prompt behavior,
// while the tools remain available for explicit user-requested delegation.
// Mirrors applyTelegramTools' snapshot+mutate pattern so extension tools and
// /reload-ext additions survive a settings change.

// applyTelegramTools registers (active=true) or removes (active=false)
// the telegram_send_image and telegram_send_file tools on the running
// agent so the model only sees them while the bridge is connected.
// Snapshots and mutates the live tool registry so any extension or
// /reload-ext additions made while Telegram is connected survive a
// later /telegram disconnect. The two Telegram entries are always replaced;
// web_search is additionally stripped only while the external bridge is active.

// telegramStatus writes a one-liner describing the bridge state.
// Reports on both the in-tui bridge and the background daemon so
// the user isn't confused when the daemon owns the poll loop.

// telegramHost adapts *Interactive to telegram.Host so the bridge
// can call back into the TUI without importing modes directly.
type telegramHost struct{ iv *Interactive }

// openSessionOpsDialog shows the picker for `/session` with no arg.
// Always offers export, import, fork, tree; the handlers bail with
// a clear status message when the precondition isn't met (empty
// transcript on fork; no parent/siblings on tree).

// doSessionOp dispatches export, import, fork, or tree. arg is the
// optional positional argument from e.g. /session export <path>
// or /session import <path>; fork and tree ignore it.

// doSessionExport writes the live session file to destination path
// dst. When dst is empty we default to ~/Downloads (falling back to
// the user's home directory if it doesn't exist). The helper
// expands a leading `~` and creates any missing parent directories.

// doSessionImport copies the .zutsession file at src into the
// running cwd's sessions directory and loads it as the active
// session, same as `/sessions` -> pick. When src is empty we ask
// the user to pass a path (no usable default here).

// defaultExportDir returns ~/Downloads when it exists, or ~ as a
// fallback, or /tmp on exotic machines with no home dir.

// expandTilde turns a leading ~ into the user's home directory.
// Returns the input unchanged when there's no tilde or no home.

// unquotePath strips a matching pair of surrounding single or
// double quotes. Drag-drop paste in the tui auto-quotes dropped
// file paths so the shell-like `/session import 'foo bar.zs'`
// stays well-formed; when the TUI's own slash handler consumes
// the arg, we want the raw path back.

// friendlyPath collapses the user's home directory to a leading ~
// so status messages read cleanly. Falls back to the raw path when
// the home dir is unknown.

// doSessionFork opens the /jump turn picker in "fork mode". The
// next selection branches the current session at that user turn
// instead of scrolling the viewport to it.

// doSessionTree opens the current session family after the shared
// fail-closed gate succeeds. It intentionally does not fall back to the
// in-memory transcript: a tree that cannot be persisted and reloaded cannot
// safely create a navigation branch.

// applySessionTreeMessageSelection keeps the old scalar integration
// source-compatible for package consumers. New dialog selections use the
// structured sessionTreeTarget path below.

// applySessionTreeTarget checks out a structured dialog target. The target's
// source and effective indices come from the same preflight snapshot the
// dialog rendered, while the live read below preserves compatibility with
// the existing LoadSession(path) error callback.

// applyForkSelection branches the current session at msgIdx+1 (so
// the selected user message and everything before it is included
// in the new branch), then switches the running agent to the new
// file. Called from the jump-dialog handler when pendingFork=true.

// formatInt is a tiny strconv.Itoa shim; keeps the handler above
// from needing a strconv import just for one call.

// assistantText returns the concatenated text of every TextBlock in
// m. Used by the streaming-view dedupe guard to tell when a live
// streamed reply has already been promoted into the transcript.

// resetTranscriptRenderLocked invalidates every render cache that assumes
// the previous transcript remains structurally intact. Compaction is a
// replacement, not an append: the flow renderer must repaint from the new
// transcript rather than diff it against scrollback rows from the old one.
// Must be called with i.mu held.

// resetStreamingStateLocked clears every piece of streaming state
// in one shot. Used by abort paths (turn cancel, compact hand-off,
// queue drain) so the pacer doesn't keep draining stale runes from
// a prior turn. Must be called with i.mu held.

// openAllToolGatesLocked drops every pending tool gate so that any
// tool registered during this turn renders unconditionally from now
// on. Called when streaming finalizes (the paced text has fully
// drained and `streaming` is about to reset to length 0): without
// this, the gate comparison against a freshly-reset streaming buffer
// would wrongly re-hide tools that had already cleared their gate.
// Must be called with i.mu held.

// gateToolLocked records the stream position at which a tool call may
// become visible. The gate is the total length the streaming buffer
// will reach once the pacer has drained everything currently queued
// (already painted + still pending). Holding the tool block back
// until the pacer crosses that mark guarantees the prose emitted
// before the tool call finishes typing out above it, instead of the
// tool block snapping in while the paragraph is still filling in.
//
// We only gate while text is actively streaming. If no stream is in
// flight (gate 0), the tool shows immediately, which is the correct
// behaviour for tool-only turns and replayed sessions. First
// registration wins so a later EvToolCall can't move an existing
// gate. Must be called with i.mu held.

// toolGateOpenLocked reports whether the gated tool block may render
// yet, i.e. the pacer has drained enough text to reach the position
// recorded when the tool call arrived. Must be called with i.mu held.

// assistantMessageSideEffects runs the non-visual hooks attached to
// EvAssistantMessage: the host-provided OnAssistant callback and the
// telegram-bridge mirror. Called with i.mu held.
//
// Factored out of handleEvent because the streaming pacer may defer
// visual reset until after the last buffered rune has painted, but
// the callbacks themselves must fire on message arrival so
// downstream observers (session persistence, telegram, cost panels)
// don't wait on a UI animation to catch up.

// paintPaceRate is how many runes the streaming pacer releases per
// tick. With a 16ms tick, 6 runes/tick is ~375 runes/s — fast enough
// that a 500-rune summary finishes in ~1.3s, slow enough to look
// like a human typing. Empirically matches the feel of provider
// paths that already drip-stream natively.
const paintPaceRate = 6

// paintPaceInterval is the tick interval for the streaming pacer.
// 16ms lines up with the redraw throttle so we never paint faster
// than the terminal can keep up.
const paintPaceInterval = 16 * time.Millisecond

// runStreamPacer drains buffered deltas from streamPending into
// streaming a small batch per tick, invalidating after each move.
// It stops when the context cancels (tui shutdown).
//
// Why a pacer: providers differ wildly in how they chunk their
// text_delta events. The API-key path on Anthropic emits ~30 drips
// for a 400-token summary; the OAuth path can coalesce the same
// summary into 3 fat chunks, visually indistinguishable from "the
// whole reply just appeared". The pacer normalizes that so every
// path looks the same on screen.
