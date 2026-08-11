package modes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

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
	Terminal     tui.Terminal
	Theme        tui.Theme
	Model        string
	Provider     string
	AuthMethod   string // "apikey" | "oauth" — used to tag cost as (sub) in status bar
	BaseURL      string
	Reasoning    string
	SystemPrompt string
	Tools        core.Registry
	MaxSteps     int
	CWD          string

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

	// TUISubagentPosition controls whether live subagent activity renders
	// above or below the main input. Empty defaults below the input.
	TUISubagentPosition string

	// QuickModelShortcuts maps slots 1-9 to provider/model pairs. The
	// shortcuts are Ctrl+1..9. Cmd+1..9 may also work when the terminal
	// forwards Command/Super keypresses, but Ctrl is the displayed chord.
	QuickModelShortcuts []QuickModelShortcut

	// ExtensionThemes returns themes bundled with loaded extensions.
	ExtensionThemes func() []tui.ThemeOption

	// AutoSubagentsSystemAddendum is the strict orchestrator block that gets
	// appended when the user enables auto-subagents at runtime. Plumbed in from
	// the cli so this package doesn't have to import agent (cycle).
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
	// web-search guard and supervisor policy.
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

	OnAssistant func(m provider.Message)

	// Extensions, if non-nil, lets users invoke extension-registered
	// slash commands. Commands declared by extensions are looked up
	// AFTER the built-in catalog so a built-in name always wins.
	Extensions *extensions.Manager

	// Supervisor, if non-nil, enables the /subagents slash command and the
	// dashboard dialog. The cli constructs the Supervisor once per
	// interactive run and tears it down on exit. nil disables the
	// feature entirely (used by embedders / tests that don't want
	// subprocesses).
	Supervisor *subagents.Supervisor

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
	SetTUISubagentPosition(position string) error
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

type Interactive struct {
	cfg  InteractiveConfig
	view *tui.View
	ed   *tui.Editor
	rend *tui.Renderer

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
	toolGate               map[string]int
	statusErr              string
	statusOK               string
	goalStatus             core.GoalStatus
	reloadStatusSeq        uint64
	extStatuses            map[string]map[string]extensionStatus
	extWidgets             map[string]map[string]extensionWidget
	rightBarHidden         bool     // session-only Ctrl+B toggle; zero value keeps the rail visible
	liveBlock              []string // live streaming/tool progress rendered outside scrollback
	helpBlock              []string // rendered above the chat when /help was typed
	cumUsage               provider.Usage
	lastCtxInput           int // input_tokens of the most recent turn — approximates current context size
	busy                   bool
	ctrlCExit              bool
	activity               agentActivity
	subagentActivityActive bool
	pendingIdleWork        []func()
	dirty                  chan struct{}
	renderScheduler        atomic.Pointer[latestFrameScheduler]
	renderRevision         atomic.Uint64
	renderOutsideLock      bool
	modelRefresh           chan modelRefreshResult
	modelRefreshing        bool
	startupPreDone         chan startupPreResult
	cancelTurn             context.CancelFunc
	scrollOffset           int // rows from the bottom; 0 = pinned to latest
	prevScrollOffset       int // last value redraw snapped against; tracks intent

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
	queued []string

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

	dialog            *loginDialog
	modelDialog       *modelDialog
	llamaDialog       *llamaDialog
	rescueDialog      *rescueDialog
	sessionDialog     *sessionDialog
	subagentsDialog   *subagentsDialog
	jumpDialog        *jumpDialog
	btwDialog         *btwDialog
	skillsDialog      *skillsDialog
	changelogDialog   *changelogDialog
	confirmDialog     *confirmDialog
	logoutDialog      *logoutDialog
	telegramDialog    *telegramDialog
	settingsDialog    *settingsDialog
	quickModelAssign  int
	telegramBridge    *telegram.Bridge
	sessionOpsDialog  *sessionOpsDialog
	sessionTreeDialog *sessionTreeDialog
	extPanel          *extPanelDialog
	llamaConfigured   bool

	// completionTracker observes auto-subagent turn and process completion.
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

	// subagentsWaitWatcherDone is an optional test hook invoked when a
	// /subagents wait watcher exits.
	subagentsWaitWatcherDone func()

	// pendingFork is true when the user ran /session fork: the next
	// jump-picker selection should branch off that message instead
	// of scrolling. Flag resets after the action fires or the dialog
	// is dismissed, so repeated /jump calls don't turn into forks.
	pendingFork bool
	suggest     *slashSuggester
	fileSuggest *fileSuggester
	spin        *spinner

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
	i := &Interactive{
		cfg: cfg,
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
		ed:                  tui.NewEditor(cfg.Theme.AccentBar(cfg.Theme.Accent)),
		rend:                renderer,
		toolCalls:           map[string]*tui.ToolCallView{},
		toolGate:            map[string]int{},
		dirty:               make(chan struct{}, 8),
		modelRefresh:        make(chan modelRefreshResult, 1),
		startupPreDone:      make(chan startupPreResult, 1),
		dialog:              newLoginDialog(),
		modelDialog:         newModelDialog(),
		llamaDialog:         newLlamaDialog(),
		rescueDialog:        newRescueDialog(),
		sessionDialog:       newSessionDialog(),
		subagentsDialog:     newSubagentsDialog(),
		jumpDialog:          newJumpDialog(),
		btwDialog:           newBtwDialog(),
		skillsDialog:        newSkillsDialog(),
		changelogDialog:     newChangelogDialog(),
		confirmDialog:       newConfirmDialog(),
		logoutDialog:        newLogoutDialog(),
		telegramDialog:      newTelegramDialog(),
		settingsDialog:      newSettingsDialog(),
		sessionOpsDialog:    newSessionOpsDialog(),
		sessionTreeDialog:   newSessionTreeDialog(),
		extPanel:            newExtPanelDialog(),
		extStatuses:         map[string]map[string]extensionStatus{},
		extWidgets:          map[string]map[string]extensionWidget{},
		suggest:             newSlashSuggester(),
		fileSuggest:         newFileSuggester(),
		spin:                newSpinner(cfg.Theme),
		inputHistoryIndex:   -1,
		clock:               time.Now,
		goalStatus:          initialGoalStatus,
		reloadErrors:        append([]string(nil), cfg.StartupExtensionErrors...),
		compactContinuation: decodeCompactHandoff(cfg.InitialCompactHandoff),
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

// Run blocks until the user quits.
func (i *Interactive) Run(ctx context.Context) error {
	i.mu.Lock()
	i.runCtx = ctx
	i.mu.Unlock()
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
	defer term.Write([]byte(tui.SeqResetScrollRegion + tui.SeqDeleteKittyImages + tui.SeqEnhancedKeyboardOff + tui.SeqBracketedPasteOff + tui.ResetCursorColor() + tui.ResetCursorShape() + tui.SeqShowCursor))
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

	// Input goroutine. Buffered generously so a drag-drop that the
	// terminal delivers as a burst of single-character key events
	// (no bracketed paste) can be drained in one main-loop pass
	// instead of triggering a redraw per character.
	keys := make(chan tui.Key, 256)
	go func() {
		reader := tui.NewReaderWithPeek(term.ReadByte, term.PeekByteTimeout)
		for {
			k, err := reader.Read()
			if err != nil {
				return
			}
			keys <- k
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

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
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
			// The subagent dashboard is also animated: its rows reflect
			// background subprocesses whose activity / age change
			// without any user input. Without the tick redraw the
			// dashboard freezes on the snapshot taken when the user
			// last pressed a key. We exclude the dashboard when one
			// of its inline editors (spawn task or prompt composer)
			// is active so the cursor blink in those editors works
			// the same way it does inside btw.
			i.mu.Lock()
			busy := i.busy
			subagentActivityActive := i.subagentActivityActive
			sessionLoading := i.sessionDialog.Loading()
			i.mu.Unlock()
			if busy || subagentActivityActive || sessionLoading || i.btwDialog.Loading() || i.subagentsDialog.NeedsTickRefresh() {
				requestRedraw()
			}
		}
	}
}

func (i *Interactive) requestRendererClear() {
	if scheduler := i.renderScheduler.Load(); scheduler != nil {
		scheduler.request(true, false)
		return
	}
	if i.rend != nil {
		i.rend.Clear()
	}
}

func (i *Interactive) requestRendererInvalidate() {
	if scheduler := i.renderScheduler.Load(); scheduler != nil {
		scheduler.request(false, true)
		return
	}
	if i.rend != nil {
		i.rend.Invalidate()
	}
}

func (i *Interactive) requestRendererTheme(theme tui.Theme) {
	if scheduler := i.renderScheduler.Load(); scheduler != nil {
		scheduler.requestTheme(theme)
		return
	}
	if i.rend != nil {
		i.rend.SetTheme(theme)
	}
}

func (i *Interactive) invalidate() {
	i.renderRevision.Add(1)
	// Keep ordinary state changes on the main-loop throttle. The scheduler
	// owns terminal output, but scheduling it here would bypass
	// redrawMinInterval and paint every intermediate tool-event frame.
	select {
	case i.dirty <- struct{}{}:
	default:
	}
}

func (i *Interactive) cachedChatLocked(cols int) []string {
	// A busy frame contains mutable streaming/tool state. Keep the stable
	// transcript cache, but never return a full-frame cache entry that would
	// hide a newly arrived delta or completed result.
	if i.busy || i.streamOn || i.streamFlushPending {
		return i.buildChatLocked(cols)
	}
	key := i.chatCacheKeyLocked(cols)
	if i.chatCacheValid && i.chatCacheKey == key {
		return append([]string(nil), i.chatCache...)
	}
	chat := i.buildChatLocked(cols)
	key = i.chatCacheKeyLocked(cols)
	i.chatCache = append(i.chatCache[:0], chat...)
	i.chatCacheKey = key
	i.chatCacheValid = true
	return chat
}

func (i *Interactive) chatCacheKeyLocked(cols int) chatCacheKey {
	var rev uint64
	if i.agent != nil {
		rev = i.agent.Revision()
	}
	showVer := len(i.view.Messages) == 0 && !i.streamOn && len(i.toolOrder) == 0 && !i.welcomeStart.IsZero() && time.Since(i.welcomeStart) < welcomeVersionDuration
	return chatCacheKey{
		cols:                 cols,
		agentRev:             rev,
		statusOK:             i.statusOK,
		statusErr:            i.statusErr,
		help:                 strings.Join(i.helpBlock, "\n"),
		extNotes:             strings.Join(i.extNotes, "\n"),
		extStatuses:          i.extensionStatusesKeyLocked(),
		extWidgets:           i.extensionWidgetsKeyLocked(),
		reloadErrors:         strings.Join(i.reloadErrors, "\n"),
		updateAvailable:      i.updateInfo.Available,
		updateCurrent:        i.updateInfo.Current,
		updateLatest:         i.updateInfo.Latest,
		updateURL:            i.updateInfo.URL,
		welcomeShowVer:       showVer,
		expandAll:            i.view.ExpandAll,
		tailLimit:            i.view.TailLimit,
		renderedMessageCount: len(i.view.Messages),
		viewCacheRev:         i.view.RenderCacheRevision,
	}
}

func sortedNestedOuterKeys[T any](groups map[string]map[string]T) []string {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedNestedInnerKeys[T any](groups map[string]map[string]T, outer string) []string {
	items := groups[outer]
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (i *Interactive) extensionStatusesKeyLocked() string {
	var sb strings.Builder
	extNames := sortedNestedOuterKeys(i.extStatuses)
	for _, extName := range extNames {
		keys := sortedNestedInnerKeys(i.extStatuses, extName)
		for _, key := range keys {
			status := i.extStatuses[extName][key]
			fmt.Fprintf(&sb, "%s/%s/%s/%s\n", extName, key, status.Level, status.Text)
		}
	}
	return sb.String()
}

func (i *Interactive) extensionWidgetsKeyLocked() string {
	var sb strings.Builder
	extNames := sortedNestedOuterKeys(i.extWidgets)
	for _, extName := range extNames {
		ids := sortedNestedInnerKeys(i.extWidgets, extName)
		for _, id := range ids {
			widget := i.extWidgets[extName][id]
			fmt.Fprintf(&sb, "%s/%s/%s/%s\n", extName, id, widget.Position, widget.Title)
			for _, line := range widget.Lines {
				sb.WriteString(line)
				sb.WriteByte('\n')
			}
		}
	}
	return sb.String()
}

const maxExtensionWidgetRows = 12
const maxExtensionStatusRows = 6

// maxNarrowExtensionChromeRows bounds right-bar widgets when they fall back
// above the input, leaving one row for the truncation marker.
const maxNarrowExtensionChromeRows = 6

func (i *Interactive) rightBarWidgetsLocked() []tui.RightBarWidget {
	var out []tui.RightBarWidget
	for _, extName := range sortedNestedOuterKeys(i.extWidgets) {
		for _, id := range sortedNestedInnerKeys(i.extWidgets, extName) {
			widget := i.extWidgets[extName][id]
			if extproto.NormalizeWidgetPosition(widget.Position) != extproto.WidgetPositionRightBar {
				continue
			}
			out = append(out, tui.RightBarWidget{
				Extension: extName,
				ID:        id,
				Title:     widget.Title,
				Lines:     append([]string(nil), widget.Lines...),
			})
		}
	}
	return out
}

func (i *Interactive) extensionChromeLinesLocked(cols int) []string {
	return i.extensionChromeLinesAtLocked(cols, false)
}

func (i *Interactive) extensionChromeLinesForLayoutLocked(cols int, rightBarActive, rightBarFallback bool) []string {
	lines := i.extensionChromeLinesAtLocked(cols, rightBarActive)
	if !rightBarFallback || len(lines) <= maxNarrowExtensionChromeRows {
		return lines
	}
	limit := maxNarrowExtensionChromeRows
	trimmed := append([]string(nil), lines[:limit-1]...)
	trimmed = append(trimmed, i.cfg.Theme.FGColor(i.cfg.Theme.Muted, truncateLine("  ... extension chrome truncated ...", cols)))
	return trimmed
}

func (i *Interactive) extensionChromeLinesAtLocked(cols int, rightBarActive bool) []string {
	var out []string
	bodyWidth := cols - 2
	if bodyWidth < 1 {
		bodyWidth = 1
	}
	widgetRows := 0
	widgetTruncated := false
	appendWidgetRow := func(line string) bool {
		if widgetRows >= maxExtensionWidgetRows-1 {
			widgetTruncated = true
			return false
		}
		out = append(out, line)
		widgetRows++
		return true
	}

	stopWidgets := false
	for _, extName := range sortedNestedOuterKeys(i.extWidgets) {
		if stopWidgets {
			break
		}
		for _, id := range sortedNestedInnerKeys(i.extWidgets, extName) {
			widget := i.extWidgets[extName][id]
			if rightBarActive && extproto.NormalizeWidgetPosition(widget.Position) == extproto.WidgetPositionRightBar {
				continue
			}
			label := "  [" + extName + "]"
			if widget.Title != "" {
				label += " " + widget.Title
			}
			if !appendWidgetRow(i.cfg.Theme.FGColor(i.cfg.Theme.Accent, truncateLine(label, cols))) {
				stopWidgets = true
				break
			}
			for _, line := range widget.Lines {
				if !appendWidgetRow("  " + truncateLine(line, bodyWidth)) {
					stopWidgets = true
					break
				}
			}
			if stopWidgets {
				break
			}
		}
	}
	if widgetTruncated {
		out = append(out, i.cfg.Theme.FGColor(i.cfg.Theme.Muted, truncateLine("  ... extension widgets truncated ...", cols)))
	}

	statusRows := 0
	statusTruncated := false
	for _, extName := range sortedNestedOuterKeys(i.extStatuses) {
		for _, key := range sortedNestedInnerKeys(i.extStatuses, extName) {
			if statusRows >= maxExtensionStatusRows-1 {
				statusTruncated = true
				break
			}
			status := i.extStatuses[extName][key]
			color := i.cfg.Theme.Muted
			switch status.Level {
			case "warn":
				color = i.cfg.Theme.Warning
			case "error":
				color = i.cfg.Theme.Error
			case "success":
				color = i.cfg.Theme.Tool
			}
			label := "  [" + extName + "] " + status.Text
			out = append(out, i.cfg.Theme.FGColor(color, truncateLine(label, cols)))
			statusRows++
		}
		if statusTruncated {
			break
		}
	}
	if statusTruncated {
		out = append(out, i.cfg.Theme.FGColor(i.cfg.Theme.Muted, truncateLine("  ... extension statuses truncated ...", cols)))
	}
	return out
}

func (i *Interactive) stableChatRowsLocked(cols int) []string {
	key := i.chatCacheKeyLocked(cols)
	if i.stableChatCacheValid && i.stableChatCacheKey == key {
		return append([]string(nil), i.stableChatCache...)
	}

	renderView := i.view.CloneForRender()
	renderView.Streaming = ""
	renderView.StreamingActive = false
	renderView.ToolCalls = nil
	renderView.Err = ""
	if !i.renderOutsideLock {
		rows := renderView.Build(cols)
		i.stableChatCache = append(i.stableChatCache[:0], rows...)
		i.stableChatCacheKey = key
		i.stableChatCacheValid = true
		return append([]string(nil), rows...)
	}
	// Markdown, syntax highlighting, and wrapping are the expensive part of
	// a frame. Build the immutable snapshot without holding the interactive
	// mutex so key processing can continue while a cold transcript renders.
	i.mu.Unlock()
	rows := renderView.Build(cols)
	i.mu.Lock()
	if i.view.MessagesRevision == renderView.MessagesRevision &&
		i.view.RenderCacheRevision == renderView.RenderCacheRevision {
		i.view.AdoptRenderCacheFrom(renderView)
	}

	i.stableChatCache = append(i.stableChatCache[:0], rows...)
	i.stableChatCacheKey = key
	i.stableChatCacheValid = true
	return append([]string(nil), rows...)
}

func (i *Interactive) buildChatLocked(cols int) []string {
	if i.agent != nil {
		i.view.Messages = filterHiddenTranscriptMessages(i.agent.Messages())
		i.view.MessagesRevision = i.agent.Revision()
	} else {
		i.view.Messages = nil
		i.view.MessagesRevision = 0
	}
	// Pacer flush: while the streaming pacer is still draining the
	// buffer (i.e. EvAssistantMessage already fired but more runes
	// are queued), the final assistant message is already in
	// i.agent.Messages() in full. Painting it in the transcript
	// AND the streaming block at the same time shows the user the
	// complete text immediately — which defeats the whole pacer.
	// Hide the last message until the pacer catches up; once the
	// flush-pending latch clears, the message is revealed (the
	// streaming block disappears the same frame because streamOn
	// flips off, so the transition is seamless).
	if i.streamFlushPending && len(i.view.Messages) > 0 {
		i.view.Messages = i.view.Messages[:len(i.view.Messages)-1]
	}
	i.view.Streaming = i.streaming.String()
	i.view.StreamingActive = i.streamOn
	// Guard against the narrow race where EvAssistantMessage has
	// just promoted a streaming reply into the transcript but a
	// render tick hasn't flipped streamOn off yet. Without the
	// guard, the same text would appear twice (once as the
	// in-flight streaming block, once as the last transcript
	// message). We detect the duplicate strictly: the last
	// assistant message's visible text must equal the streaming
	// buffer. Just matching on role is too broad — it also hides
	// the next round's typewriter streaming after a tool turn,
	// because the last transcript message is always assistant
	// (the tool-use block) until the follow-up summary lands.
	if i.streamOn && i.streaming.Len() > 0 {
		if n := len(i.view.Messages); n > 0 && i.view.Messages[n-1].Role == provider.RoleAssistant {
			if assistantText(i.view.Messages[n-1]) == i.streaming.String() {
				i.view.StreamingActive = false
			}
		}
	}
	// Live tool-call view: only shown while a turn is in flight. Once
	// the agent is idle, every tool call has already been folded into
	// the transcript (as assistant.ToolCallBlock + a tool-role message),
	// so showing v.ToolCalls a second time would duplicate them below
	// the final assistant text — which looks like the summary came
	// "before" the tools.
	i.view.ToolCalls = i.view.ToolCalls[:0]
	if i.busy {
		for _, id := range i.toolOrder {
			// Deterministic ordering: a tool block stays hidden until
			// the paced assistant text that preceded it has finished
			// typing out. toolOrder is append-only in arrival order,
			// so once one tool is still gated, every later tool is too,
			// so stop here to avoid showing a tool out of sequence.
			if !i.toolGateOpenLocked(id) {
				break
			}
			if tc, ok := i.toolCalls[id]; ok {
				i.view.ToolCalls = append(i.view.ToolCalls, *tc)
			}
		}
	}
	i.view.Err = i.statusErr
	// Live streaming/tool rows are appended to the chat buffer (not
	// hoisted into a separate live block above the editor). That keeps
	// the renderer's diff view append-only: when a tool finishes the
	// rows update in place at the end of the buffer, instead of the
	// whole bottom band shrinking and shifting chat lines around.
	i.liveBlock = nil
	stable := i.stableChatRowsLocked(cols)
	chat := append([]string(nil), stable...)
	chat = append(chat, i.view.BuildLive(cols)...)

	// Welcome banner: shown at the top of the chat area when there is
	// no transcript yet. Disappears after the first message is sent.
	// The version suffix is shown for welcomeVersionDuration after
	// startup, then drops off automatically.
	if len(i.view.Messages) == 0 && !i.streamOn && len(i.toolOrder) == 0 {
		showVer := !i.welcomeStart.IsZero() && time.Since(i.welcomeStart) < welcomeVersionDuration
		chat = append(welcomeBanner(i.cfg.Theme, i.cfg.Version, showVer), chat...)
	}

	// Update-available banner: prepended above everything else so it's
	// the first thing the user sees when opening a new zut session.
	// Once rendered, it stays until the user updates to a newer
	// version — we don't persist a "dismissed" flag because this is
	// cheap and re-showing it is how most users remember to update.
	if i.updateInfo.Available {
		banner := renderUpdateBanner(i.cfg.Theme, i.updateInfo, cols)
		chat = append(banner, chat...)
	}

	// /help block: appended to the transcript so it appears at the
	// bottom of the chat area (right above the status bar / editor).
	// Prepending it would push long conversations off the top of the
	// viewport, which users would miss entirely.
	if len(i.helpBlock) > 0 {
		chat = append(chat, i.helpBlock...)
	}

	if i.statusOK != "" {
		// Hard-truncate the OK line to the visible width so a long
		// session path ("resumed session: /Users/.../sessions/...")
		// doesn't overflow past the right edge and look broken on a
		// narrow terminal.
		line := "✓ " + i.statusOK
		if cols > 4 && len(line) > cols {
			line = line[:cols-3] + "..."
		}
		chat = append(chat, i.cfg.Theme.FGColor(i.cfg.Theme.Tool, line), "")
	}

	// Live shell-escape output (!command / entry.pre) streams into the
	// transcript area while the command runs, then is replaced by the
	// final user-context message when it finishes.
	if i.shellRunning && i.shellLive != "" {
		chat = append(chat, strings.Split(strings.TrimRight(i.shellLive, "\n"), "\n")...)
		chat = append(chat, "")
	}

	// Extension notes (notify / display) live just under the
	// transcript, above the dialog/editor band. Cleared by /clear.
	if len(i.extNotes) > 0 {
		chat = append(chat, i.extNotes...)
		chat = append(chat, "")
	}

	// Reload failures remain in the scrolling chat until /clear. They are
	// host-only display state, not provider messages or persisted context.
	// While the latest failure is still the temporary status error, hide its
	// stored copy to avoid painting the same message twice.
	reloadErrors := i.reloadErrors
	if n := len(reloadErrors); n > 0 && reloadErrors[n-1] == i.statusErr {
		reloadErrors = reloadErrors[:n-1]
	}
	if len(reloadErrors) > 0 {
		chat = append(chat, renderReloadErrors(i.cfg.Theme, reloadErrors, cols)...)
		chat = append(chat, "")
	}

	// Strip trailing blank rows so the chat content sits flush
	// against the new "blank above status bar" row added by the
	// bottom-region assembly. Build() ends every message with a
	// blank separator; without this trim, the final message in
	// the transcript would have its own trailing blank plus the
	// status block's leading blank, doubling the gap.
	for len(chat) > 0 && strings.TrimSpace(chat[len(chat)-1]) == "" {
		chat = chat[:len(chat)-1]
	}
	return chat
}

// sessionsRoot returns the session namespace for this run.
func (i *Interactive) sessionsRoot() string {
	if i.cfg.SessionsRoot != "" {
		return i.cfg.SessionsRoot
	}
	return i.cfg.ZutHome
}

// lastCols returns the current terminal width in columns.
func (i *Interactive) lastCols() int {
	cols, _ := i.cfg.Terminal.Size()
	return cols
}

// chatPage returns the number of chat rows currently visible, used
// as the page size for PageUp/PageDown.
func (i *Interactive) chatPage() int {
	_, rows := i.cfg.Terminal.Size()
	p := rows - 6 // rough reservation for status + editor + a dialog line
	if p < 4 {
		p = 4
	}
	return p
}

// scrollBy adjusts the scroll offset. Positive = up (into history).
// Clearing the parked-turn label when we're back at the bottom means
// the "viewing turn N" footer goes away automatically as soon as you
// scroll back to the live tail.
func (i *Interactive) scrollBy(delta int) {
	i.mu.Lock()
	i.scrollOffset += delta
	if i.scrollOffset < 0 {
		i.scrollOffset = 0
	}
	if i.scrollOffset == 0 {
		i.parkedTurn = 0
		i.parkedTotal = 0
	}
	if i.rend != nil {
		// VS Code's terminal is especially prone to leaving stray
		// wrapped-character fragments behind during scroll-driven
		// viewport changes. Force a full repaint on scroll, but
		// avoid a whole-screen clear because that visibly flickers.
		i.requestRendererInvalidate()
	}
	i.mu.Unlock()
	i.invalidate()
}

// anchorScrollOffset keeps the user's reading position pinned when the
// chat buffer grows/shrinks or the viewport height changes between two
// redraws while they're scrolled up.
//
// scrollOffset is measured from the bottom of the chat buffer, so the
// top visible row is start = chatLen - scrollOffset - chatRows. To hold
// `start` constant we adjust the offset by the buffer-length delta minus
// the viewport-height delta. The result is clamped to [0, newLen] so a
// shrinking buffer can't push it negative.
func anchorScrollOffset(offset, prevLen, newLen, prevRows, newRows int) int {
	adj := (newLen - prevLen) - (newRows - prevRows)
	offset += adj
	if offset < 0 {
		offset = 0
	}
	if offset > newLen {
		offset = newLen
	}
	return offset
}

// scrollToBottom pins the view to the latest content.
func (i *Interactive) scrollToBottom() {
	i.mu.Lock()
	i.scrollOffset = 0
	i.parkedTurn = 0
	i.parkedTotal = 0
	// Reset the auto-follow baseline. scrollToBottom is the resume /
	// session-swap snap point, where the chat buffer changes length
	// wholesale. Without zeroing these, the next render's follow guard
	// compares the fresh transcript's length against a stale baseline
	// and nudges scrollOffset, which reads as a viewport jump right
	// after resume. See commit 43da5e5 for the same fix on new turns.
	i.prevChatLen = 0
	i.prevChatCols = 0
	i.requestRendererInvalidate()
	i.mu.Unlock()
	i.invalidate()
}

func (i *Interactive) redraw() {
	i.mu.Lock()
	renderRevision := i.renderRevision.Load()

	cols, rows := i.cfg.Terminal.Size()
	mainCols := cols
	rightBarWidgets := i.rightBarWidgetsLocked()
	rightBarActive := false
	var rightBarWidth int
	if !i.rightBarHidden && len(rightBarWidgets) > 0 {
		if width, rail, ok := tui.RightBarColumns(cols); ok {
			mainCols = width
			rightBarWidth = rail
			rightBarActive = true
		}
	}
	rightBarFallback := len(rightBarWidgets) > 0 && !rightBarActive
	i.renderOutsideLock = true
	chat := i.cachedChatLocked(mainCols)
	i.renderOutsideLock = false
	if i.renderRevision.Load() != renderRevision {
		i.mu.Unlock()
		return
	}

	// Dialogs (login or model picker) render between chat and the editor.
	var dialog []string
	switch {
	case i.dialog.Active():
		dialog = i.dialog.Render(i.cfg.Theme, mainCols)
	case i.modelDialog.Active():
		dialog = i.modelDialog.Render(i.cfg.Theme, mainCols)
	case i.llamaDialog.Active():
		dialog = i.llamaDialog.Render(i.cfg.Theme, mainCols)
	case i.rescueDialog.Active():
		dialog = i.rescueDialog.Render(i.cfg.Theme, mainCols)
	case i.sessionDialog.Active():
		// Reserve rows for the editor (~3), status line (1-2),
		// dialog chrome (header + hint + rule + indicators, ~5),
		// and leave the remainder for session rows. Minimum of 3
		// rows so even a very small terminal shows something.
		_, rows := i.cfg.Terminal.Size()
		avail := rows - 12
		if avail < 3 {
			avail = 3
		}
		i.sessionDialog.MaxRows = avail
		dialog = i.sessionDialog.Render(i.cfg.Theme, mainCols)
	case i.subagentsDialog.Active():
		// The dashboard is composed into the bottom-sticky dialog band.
		// Reserve its leading gap, frame padding, one chat row, and the
		// renderer margin so a long list or expanded snapshot never pushes
		// its top rows out of the terminal viewport.
		avail := rows - 4
		if avail < 4 {
			avail = 4
		}
		i.subagentsDialog.SetMaxRows(avail)
		dialog = i.subagentsDialog.Render(i.cfg.Theme, mainCols)
	case i.jumpDialog.Active():
		dialog = i.jumpDialog.Render(i.cfg.Theme, mainCols)
	case i.extPanel.Active() && (!i.confirmDialog.Active() || !i.confirmDialog.Focused()):
		dialog = i.extPanel.Render(i.cfg.Theme, mainCols)
	case i.confirmDialog.Active():
		if i.btwDialog.Active() {
			// Keep the side-chat transcript and its live tool preview visible
			// above confirmation, matching how the main transcript remains
			// visible while --no-yolo waits for a decision.
			dialog = renderBtwConfirmation(i.cfg.Theme, mainCols, i.btwDialog, i.confirmDialog)
		} else {
			dialog = i.confirmDialog.Render(i.cfg.Theme, mainCols)
		}
	case i.btwDialog.Active():
		dialog = i.btwDialog.Render(i.cfg.Theme, mainCols)
	case i.skillsDialog.Active():
		dialog = i.skillsDialog.Render(i.cfg.Theme, mainCols)
	case i.changelogDialog.Active():
		dialog = i.changelogDialog.Render(i.cfg.Theme, mainCols)
	case i.logoutDialog.Active():
		dialog = i.logoutDialog.Render(i.cfg.Theme, mainCols)
	case i.telegramDialog.Active():
		dialog = i.telegramDialog.Render(i.cfg.Theme, mainCols)
	case i.settingsDialog.Active():
		dialog = i.settingsDialog.Render(i.cfg.Theme, mainCols)
	case i.sessionOpsDialog.Active():
		dialog = i.sessionOpsDialog.Render(i.cfg.Theme, mainCols)
	case i.sessionTreeDialog.Active():
		// Reserve rows for the editor, status, and tree chrome while using
		// the same budget for rendering and PageUp/PageDown movement.
		_, rows := i.cfg.Terminal.Size()
		avail := rows - 12
		if avail < 3 {
			avail = 3
		}
		i.sessionTreeDialog.MaxRows = avail
		dialog = i.sessionTreeDialog.Render(i.cfg.Theme, mainCols)
	}
	if len(dialog) > 0 {
		dialog = padDialogFrame(dialog)
	}
	modalBackdrop := len(dialog) > 0

	// Slash-command autocomplete: popup above the status line, only
	// when the editor starts with "/" and no dialog is already open.
	// Feed extension-registered commands into the suggester first so
	// they show up in tab-complete + the popup alongside the built-ins.
	i.suggest.SetJailed(i.cfg.Sandbox.Locked())
	i.suggest.SetLlamaConfigured(i.llamaConfigured)
	if i.cfg.Extensions != nil {
		catalog := i.cfg.Extensions.Commands()
		extra := make([]slashCommand, 0, len(catalog))
		for _, c := range catalog {
			// The popup renders extension commands under a dedicated
			// "── extensions ───" divider, so the description doesn't
			// need to repeat the source. If the description is empty,
			// fall back to the extension name so the row isn't blank.
			desc := c.Description
			if strings.TrimSpace(desc) == "" {
				desc = "(" + c.Extension + ")"
			}
			extra = append(extra, slashCommand{
				Name: "/" + c.Name,
				Desc: desc,
			})
		}
		i.suggest.SetExtra(extra)
	}
	var suggest []string
	currentInput := i.ed.Value()
	if i.suggest.SkillInputStarted(currentInput) {
		var list []*skills.Skill
		if i.cfg.SkillSnapshot != nil {
			list = i.cfg.SkillSnapshot()
		}
		i.suggest.SetSkills(list)
	}
	// Slash popup renders even while the agent is busy so the user
	// can queue a destructive command (/clear, /compact, /logout,
	// /model) or a read-only one (/help, /jump, /sessions, etc.)
	// without waiting for the current turn to finish. The dispatcher
	// in runSlash already handles the busy case per-command: safe
	// ones run immediately, destructive ones cancel the turn first.
	i.fileSuggest.SetCWD(i.cfg.CWD)
	mainInputFocused := len(dialog) == 0 || (i.confirmDialog.Active() && !i.confirmDialog.Focused() && !i.confirmChildActiveLocked())
	if mainInputFocused && i.suggest.Active(currentInput) {
		suggest = i.suggest.Render(currentInput, i.cfg.Theme, mainCols)
	} else if mainInputFocused && i.fileSuggest.Active(currentInput) {
		suggest = i.fileSuggest.Render(currentInput, i.cfg.Theme, mainCols)
	}

	// Detect overlay close (any dialog or slash/file suggestion popup
	// just transitioned from open to closed). Force a full redraw so
	// the rows the overlay occupied are guaranteed to be repainted
	// from the chat below, instead of the diff path leaving stale
	// dialog content behind. Equivalent to the user pressing ctrl+l.
	overlayOpen := len(dialog) > 0 || len(suggest) > 0
	if i.rend != nil && i.prevOverlayOpen && !overlayOpen {
		// An overlay (dialog or slash/file popup) just closed, so the
		// bottom band shrinks. On terminals where we can drop
		// scrollback, a full Clear is the simplest way to guarantee
		// the vacated rows are repainted from the chat below.
		//
		// On VS Code's terminal closing a dialog leaves the stale
		// overlay rows in the retained scrollback (we can't drop them
		// with the quiet in-place diff). Run the same full Clear() that
		// Ctrl+L uses so the scrollback is purged and the conversation
		// is repainted clean, matching what the user expects after
		// dismissing a picker. Clear() is keepScrollback-aware and
		// emits \x1b[3J there.
		i.rend.Clear()
	}
	i.prevOverlayOpen = overlayOpen
	if len(suggest) > 0 {
		// One blank row above the popup so it doesn't sit flush
		// against the chat / welcome content above.
		suggest = append([]string{""}, suggest...)
	}

	// Busy prefix shown at the far left of the status bar. The spinner owns
	// only its frame; the activity label reports the operation in progress.
	var busyPrefix string
	if i.busy {
		label := i.activity.label()
		if label == "" {
			label = "Preparing request"
		}
		busyPrefix = fmt.Sprintf("%s %s %s %s",
			i.cfg.Theme.FGColor(i.cfg.Theme.Assistant, i.spin.Frame()),
			i.cfg.Theme.FGColor(i.cfg.Theme.Assistant, label),
			i.cfg.Theme.FGColor(i.cfg.Theme.Muted, "-"),
			i.cfg.Theme.FGColor(i.cfg.Theme.Muted, i.spin.Elapsed().String()),
		)
	}

	ctxMax := 0
	if m, err := provider.FindModel(i.cfg.Provider, i.cfg.Model); err == nil {
		ctxMax = m.ContextWindow
	}
	statusPosition := tui.NormalizeStatusPosition(i.cfg.TUIStatusPosition)
	workingPosition := tui.NormalizeWorkingPosition(i.cfg.TUIWorkingPosition)
	workingWithStatus := statusPosition == workingPosition
	statusBusyPrefix := ""
	if workingWithStatus {
		statusBusyPrefix = busyPrefix
	}
	goalStatus := string(i.goalStatus)
	statusLines := tui.StatusBar(tui.StatusBarParams{
		Theme:          i.cfg.Theme,
		Provider:       i.cfg.Provider,
		Model:          i.cfg.Model,
		Reasoning:      i.cfg.Reasoning,
		FastMode:       i.cfg.FastMode != nil && *i.cfg.FastMode,
		Busy:           i.busy,
		BusyPrefix:     statusBusyPrefix,
		CWD:            i.cfg.CWD,
		Locked:         i.cfg.Sandbox.Locked(),
		NoYolo:         i.cfg.NoYolo,
		GoalStatus:     goalStatus,
		Usage:          i.cumUsage,
		Subscription:   i.cfg.AuthMethod == "oauth",
		ContextUsed:    i.lastCtxInput,
		ContextMax:     ctxMax,
		AutoCompacting: i.autoCompacting,
		Telegram:       i.telegramBridge != nil && i.telegramBridge.Active(),
		Cols:           mainCols,
	})
	inputStyle := tui.NormalizeInputStyle(i.cfg.TUIInputStyle)
	if inputStyle == tui.InputStyleLines || inputStyle == tui.InputStyleBlock {
		i.ed.Prompt = ""
	} else {
		i.ed.Prompt = i.cfg.Theme.AccentBar(i.cfg.Theme.Accent)
	}
	edLines, curR, curC := i.ed.Render(mainCols)
	dashboardActive := i.subagentsDialog.Active()
	var allSubagentLines []string
	if !dashboardActive {
		now := i.clock()
		allSubagentLines = renderSubagentActivityLines(
			i.cfg.Theme,
			i.spin.FrameAt(now),
			i.activeSubagentActivitySnapshots(),
			mainCols,
			now,
		)
	}
	var workingLines []string
	if busyPrefix != "" && !workingWithStatus {
		workingLines = []string{"  " + busyPrefix}
	}
	inputCursorOffset := 0
	inputCursorColOffset := 0
	switch inputStyle {
	case tui.InputStyleLines:
		edLines = tui.InputLines(i.cfg.Theme, edLines, mainCols)
		inputCursorOffset = 1
	case tui.InputStyleBlock:
		edLines = tui.InputBlock(i.cfg.Theme, edLines, mainCols)
		inputCursorOffset = 1
		inputCursorColOffset = 2
	}

	// "Sliding in" chips for messages the user typed while a turn is
	// in flight. Shown directly above the status bar so they're close
	// to the editor but don't push the chat around.
	var queue []string
	queued := append([]string(nil), i.queued...)
	if i.agent != nil {
		queued = append(queued, i.agent.PendingQueuedMessages()...)
	}
	if len(queued) > 0 {
		queue = append(queue, "")
		for _, q := range queued {
			label := i.cfg.Theme.FGColor(i.cfg.Theme.Accent, "  sliding in: ")
			text := truncateLine(q, mainCols-17)
			queue = append(queue, label+i.cfg.Theme.FGColor(i.cfg.Theme.Muted, text))
		}
		// Hint row, rendered in the same muted tone as the model
		// info on the status bar so it reads as ambient metadata
		// rather than a chip. Tells the user how to recover the
		// most recent queued message back into the editor.
		hint := "  Press " + slideBackChordHint() + " to slide back into input"
		queue = append(queue, i.cfg.Theme.FGColor(i.cfg.Theme.Muted, hint))
	}

	extensionLines := i.extensionChromeLinesForLayoutLocked(mainCols, rightBarActive, rightBarFallback)

	// Bottom-sticky sections (always visible, never scroll). Each
	// non-empty subsection (dialog, suggest popup, sliding-in queue)
	// is preceded by one blank row so it has air above the chat
	// content. The status block and editor get their own dedicated
	// blanks so spacing stays consistent whether or not a dialog or
	// popup is showing.
	composeBottom := func(subagentLines []string) (bottom []string, inputStartRow int) {
		bottom = make([]string, 0, len(dialog)+len(suggest)+len(queue)+len(extensionLines)+len(statusLines)+len(subagentLines)+len(edLines)+9)
		inputStartRow = -1
		if len(dialog) > 0 {
			bottom = append(bottom, "")
		}
		bottom = append(bottom, dialog...)
		// The subagent dashboard owns the bottom of the screen while it's
		// active: it has its own inline editors for spawn (`n`) and
		// prompt (`p`), so the main input would be a confusing second
		// caret. The suggest popup, sliding-in queue, status block, and
		// main editor are all hidden underneath it. Keystrokes still
		// reach handleKey — it routes them to subagentsDialog.HandleKey
		// before the editor ever sees them — so the only effect of this
		// branch is visual.
		if dashboardActive {
			return bottom, inputStartRow
		}

		bottom = append(bottom, suggest...)
		bottom = append(bottom, queue...)
		lineInput := inputStyle == tui.InputStyleLines
		statusBelow := statusPosition == tui.StatusPositionBelowInput
		workingBelow := workingPosition == tui.WorkingPositionBelowInput
		subagentBelow := tui.NormalizeSubagentPosition(i.cfg.TUISubagentPosition) == tui.SubagentPositionBelowInput

		var aboveInput []string
		aboveInput = append(aboveInput, extensionLines...)
		if !statusBelow {
			aboveInput = append(aboveInput, statusLines...)
		}
		if !workingBelow {
			aboveInput = append(aboveInput, workingLines...)
		}
		if !subagentBelow {
			aboveInput = append(aboveInput, subagentLines...)
		}
		var belowInput []string
		if subagentBelow {
			belowInput = append(belowInput, subagentLines...)
		}
		if workingBelow {
			belowInput = append(belowInput, workingLines...)
		}
		if statusBelow {
			belowInput = append(belowInput, statusLines...)
		}

		bottom = append(bottom, "")
		bottom = append(bottom, aboveInput...)
		needsGapAboveInput := len(aboveInput) > 0 && !lineInput
		if lineInput && statusBelow && !workingBelow && len(workingLines) > 0 {
			needsGapAboveInput = true
		}
		if needsGapAboveInput {
			bottom = append(bottom, "")
		}
		inputStartRow = len(bottom)
		bottom = append(bottom, edLines...)
		if len(belowInput) > 0 {
			// Running subagents sit immediately under the prompt by default.
			// Other below-input chrome keeps the existing breathing room.
			if !lineInput && (!subagentBelow || len(subagentLines) == 0) {
				bottom = append(bottom, "")
			}
			bottom = append(bottom, belowInput...)
		}
		return bottom, inputStartRow
	}

	// Preserve one chat row and the renderer-owned bottom margin. This keeps
	// a bounded set of live indicators from pushing the editor or its cursor
	// out of a fixed right-bar frame on short terminals.
	const rendererBottomMarginRows = 1
	maxBottomRows := rows - rendererBottomMarginRows - 1
	subagentLines := allSubagentLines
	bottom, inputStartRow := composeBottom(subagentLines)
	if !dashboardActive && len(allSubagentLines) > 0 {
		for maxRows := len(allSubagentLines); maxRows >= 0; maxRows-- {
			candidate := limitSubagentActivityLines(i.cfg.Theme, allSubagentLines, maxRows, mainCols)
			candidateBottom, candidateInputStartRow := composeBottom(candidate)
			if len(candidateBottom) <= maxBottomRows || maxRows == 0 {
				subagentLines = candidate
				bottom = candidateBottom
				inputStartRow = candidateInputStartRow
				break
			}
		}
	}
	i.subagentActivityActive = !dashboardActive && len(subagentLines) > 0

	chatRows := rows - len(bottom)
	if chatRows < 1 {
		chatRows = 1
	}

	// Auto-follow guard: when the user has scrolled up (scrollOffset
	// > 0) and the agent appends new content below the viewport while
	// they're reading, compensate so the visible content stays
	// anchored. scrollOffset is measured from the bottom of `chat`,
	// so without compensation a growing buffer pushes the window
	// downward through the content and the lines the user was
	// reading scroll off the top.
	//
	// Skip compensation when the terminal width changed (a resize
	// reflows the whole buffer and the line-count delta no longer
	// corresponds to appended content) and when scrollOffset is 0
	// (the user is following the tail and wants new content to push
	// the view down as usual).
	//
	// The window the user sees starts at row
	//   start = len(chat) - scrollOffset - chatRows
	// so to keep `start` fixed across a redraw we must offset by both
	// the buffer growth (len delta) AND the viewport-height change
	// (chatRows delta, e.g. the status band or sliding-in queue
	// appearing during a turn). Compensating only for the len delta
	// let a shrinking chatRows pull the window toward the tail, which
	// read as the viewport jumping to the bottom whenever the agent
	// streamed text or a tool call grew the bottom band.
	if i.scrollOffset > 0 && i.prevChatCols == mainCols && i.prevChatLen > 0 {
		i.scrollOffset = anchorScrollOffset(i.scrollOffset,
			i.prevChatLen, len(chat), i.prevChatRows, chatRows)
	}
	i.prevChatLen = len(chat)
	i.prevChatCols = mainCols
	i.prevChatRows = chatRows

	// Apply scroll offset to the chat slice.
	maxOffset := len(chat) - chatRows
	if maxOffset < 0 {
		maxOffset = 0
	}
	// Tail-render expansion: if the user has scrolled to (or above)
	// the top of the currently rendered tail and there are still
	// truncated messages above, widen view.TailLimit and rebuild.
	// The chat cache is keyed on tailLimit so the next cachedChatLocked
	// will re-issue Build instead of returning the stale slice. We
	// rebuild immediately so the same redraw shows the freshly-revealed
	// rows; otherwise the user would have to scroll again to see them.
	if i.scrollOffset >= maxOffset && i.view.TailLimit > 0 && i.view.TailLimit < len(i.view.Messages) {
		prevLen := len(chat)
		i.view.TailLimit += resumeTailExpandStep
		if i.view.TailLimit >= len(i.view.Messages) {
			i.view.TailLimit = 0 // unbounded
		}
		i.chatCacheValid = false
		chat = i.cachedChatLocked(mainCols)
		for len(chat) > 0 && strings.TrimSpace(chat[len(chat)-1]) == "" {
			chat = chat[:len(chat)-1]
		}
		// Newly-revealed rows are older messages prepended at the top.
		// scrollOffset counts rows from the bottom, so to keep the
		// viewport visually anchored on the same content the user was
		// looking at we shift it up by the number of rows added. Keep
		// the auto-follow baseline (prevChatLen) in sync with the
		// post-expansion length too, so the next render's follow guard
		// doesn't see this growth as a synthetic delta and yank the
		// viewport again.
		if grew := len(chat) - prevLen; grew > 0 {
			i.scrollOffset += grew
		}
		i.prevChatLen = len(chat)
		i.prevChatCols = mainCols
		maxOffset = len(chat) - chatRows
		if maxOffset < 0 {
			maxOffset = 0
		}
	}
	if i.scrollOffset > maxOffset {
		i.scrollOffset = maxOffset
	}
	if i.scrollOffset < 0 {
		i.scrollOffset = 0
	}

	var visibleChat []string
	if len(chat) <= chatRows {
		visibleChat = chat
	} else {
		end := len(chat) - i.scrollOffset
		rawStart := end - chatRows
		if rawStart < 0 {
			rawStart = 0
		}
		start := snapViewportStartToImageBlock(chat, rawStart)
		// If the snap pulled start upward (an image-block was atomic) while
		// the user is scrolling downward, the viewport would sit on the same
		// image until the user mashes down past every reserved row. Bump
		// scrollOffset past the image so one keypress always clears it.
		if start < rawStart && i.scrollOffset < i.prevScrollOffset {
			jump := rawStart - start
			i.scrollOffset -= jump
			if i.scrollOffset < 0 {
				i.scrollOffset = 0
			}
			end = len(chat) - i.scrollOffset
			rawStart = end - chatRows
			if rawStart < 0 {
				rawStart = 0
			}
			start = snapViewportStartToImageBlock(chat, rawStart)
		}
		end = start + chatRows
		if end > len(chat) {
			end = len(chat)
			start = end - chatRows
			if start < 0 {
				start = 0
			}
		}
		visibleChat = chat[start:end]
	}
	i.prevScrollOffset = i.scrollOffset
	visibleChat = clipBottomClippedImages(visibleChat)

	// A tiny "scrolled up" indicator in the top-right of the chat pane
	// so you know you're not at the bottom. When the viewport was
	// parked by /jump we include the turn number so the user remembers
	// they're reading history rather than the live conversation.
	if i.scrollOffset > 0 && len(visibleChat) > 0 {
		var text string
		if i.parkedTurn > 0 && i.parkedTotal > 0 {
			text = fmt.Sprintf("  ↑ viewing turn %d of %d - %d lines more below (pgdn / end)",
				i.parkedTurn, i.parkedTotal, i.scrollOffset)
		} else {
			text = fmt.Sprintf("  ↑ %d lines more below (end to jump)", i.scrollOffset)
		}
		note := i.cfg.Theme.FGColor(i.cfg.Theme.Muted, text)
		visibleChat = append([]string{note}, visibleChat...)
		if len(visibleChat) > chatRows {
			visibleChat = visibleChat[:chatRows]
		}
	}

	// A dialog is the foreground layer. Dim every other visible surface,
	// including the main input beneath it, without changing the dialog's
	// own styling. Keep the source slices untouched because chat is cached.
	if modalBackdrop {
		chat = tui.DimLines(chat)
		visibleChat = tui.DimLines(visibleChat)
		dimmedBottom := tui.DimLines(bottom)
		copy(dimmedBottom[1:1+len(dialog)], dialog)
		bottom = dimmedBottom
	}

	// Default: the real terminal cursor sits on the main editor's
	// input position. In main-screen log mode cursor rows are relative
	// to the fixed bottom band, not the chat transcript.
	// dialogLead is 1 when the bottom region prepends a blank above
	// the dialog block (whenever a dialog is showing) so popup-side
	// cursor positions still land in the right cell.
	dialogLead := 0
	if len(dialog) > 0 {
		dialogLead = 1
	}
	cursorRow := inputStartRow + inputCursorOffset + curR
	cursorCol := curC + inputCursorColOffset
	cursorInDialog := false
	if i.btwDialog.Active() {
		if r, c := i.btwDialog.CursorPos(mainCols); r >= 0 {
			cursorRow = dialogLead + r
			cursorCol = c
			cursorInDialog = true
		}
	}
	if i.dialog.Active() {
		if r, c := i.dialog.CursorPos(mainCols); r >= 0 {
			cursorRow = dialogLead + r
			cursorCol = c
			cursorInDialog = true
		}
	}
	if i.llamaDialog.Active() {
		if r, c := i.llamaDialog.CursorPos(); r >= 0 {
			cursorRow = dialogLead + r
			cursorCol = c
			cursorInDialog = true
		} else {
			cursorRow = -1
			cursorCol = 0
		}
	}
	if i.sessionDialog.Active() {
		if r, c := i.sessionDialog.CursorPos(); r >= 0 {
			cursorRow = dialogLead + r
			cursorCol = c
			cursorInDialog = true
		}
	}
	if i.sessionTreeDialog.Active() {
		if r, c := i.sessionTreeDialog.CursorPos(); r >= 0 {
			cursorRow = dialogLead + r
			cursorCol = c
			cursorInDialog = true
		}
	}
	if i.subagentsDialog.Active() {
		if r, c := i.subagentsDialog.CursorPos(mainCols); r >= 0 {
			cursorRow = dialogLead + r
			cursorCol = c
			cursorInDialog = true
		} else {
			// Dashboard list / transcript view has no caret. Without
			// this branch the default cursorRow points at the
			// (hidden) main editor row, so the terminal would draw
			// a stray block somewhere in the chat region.
			cursorRow = -1
			cursorCol = 0
		}
	}
	if i.extPanel.Active() {
		cursorRow = -1
		cursorCol = 0
	}
	i.setInputCursorDimmed(modalBackdrop && !cursorInDialog)
	if i.renderRevision.Load() != renderRevision {
		i.mu.Unlock()
		return
	}
	theme := i.cfg.Theme
	// State assembly above is synchronized; terminal output is owned by
	// this renderer goroutine and must not hold the global state mutex.
	i.mu.Unlock()
	if rightBarActive {
		rightBar := tui.RenderRightBar(theme, rightBarWidgets, rightBarWidth, rows)
		if modalBackdrop {
			i.rend.DrawRightBarDimmed(visibleChat, bottom, tui.DimLines(rightBar), cursorRow, cursorCol)
		} else {
			i.rend.DrawRightBar(visibleChat, bottom, rightBar, cursorRow, cursorCol)
		}
	} else {
		_ = visibleChat // maintained for legacy scroll state/indicators; DrawLog owns chat viewport.
		i.rend.DrawLog(chat, bottom, cursorRow, cursorCol)
	}
	i.mu.Lock()
	if i.pendingAlert != nil && !i.busy && !i.streamOn && !i.streamFlushPending && len(i.streamPending) == 0 {
		alert := *i.pendingAlert
		i.pendingAlert = nil
		i.emitAlertLocked(alert)
	}
	i.mu.Unlock()
}

func hasImageEscape(line string) bool {
	return strings.Contains(line, "\x1b]1337;File=") || strings.Contains(line, "\x1b_G")
}

// snapViewportStartToImageBlock treats inline images as atomic blocks for
// scrolling. Terminal image protocols draw from a single escape row into a
// separate graphics layer; the following blank rows are only zut's reserved
// footprint. If the viewport starts on one of those blank rows, there is no
// correct partial-image state to render. Snap back to the escape row instead
// so the image is either shown from its beginning or skipped entirely.
func snapViewportStartToImageBlock(chat []string, start int) int {
	if start <= 0 || start >= len(chat) {
		return start
	}
	if hasImageEscape(chat[start]) || !isBoxBlankLine(chat[start]) {
		return start
	}
	for k := start - 1; k >= 0; k-- {
		line := chat[k]
		if hasImageEscape(line) {
			return k
		}
		if !isBoxBlankLine(line) {
			break
		}
	}
	return start
}

const (
	hiddenOpenAIImageMirrorPrefix = "Tool output included the following image content:"
	autoCompactContinueMetaKey    = "auto_compact_continue"
	shellEscapeMetaKey            = "shell_escape"
)

func filterHiddenTranscriptMessages(msgs []provider.Message) []provider.Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]provider.Message, 0, len(msgs))
	for _, m := range msgs {
		if isHiddenTranscriptMessage(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

func isHiddenTranscriptMessage(m provider.Message) bool {
	if m.Meta[autoCompactContinueMetaKey] == "true" || m.Meta[goalContinueMetaKey] == "true" {
		return true
	}
	if m.Role != provider.RoleUser || len(m.Content) == 0 {
		return false
	}
	tb, ok := m.Content[0].(provider.TextBlock)
	if !ok {
		return false
	}
	return strings.TrimSpace(tb.Text) == hiddenOpenAIImageMirrorPrefix
}

func clipBottomClippedImages(lines []string) []string {
	if len(lines) == 0 {
		return lines
	}
	out := append([]string(nil), lines...)
	for i, line := range out {
		if !hasImageEscape(line) {
			continue
		}
		// Image blocks render as: image escape, zero or more blank
		// reservation rows, then the muted "image - ..." info line,
		// then one trailing blank. If the info line isn't visible in
		// the current chat slice, the image would paint down into the
		// fixed status bar area. Suppress that image for this frame.
		//
		// When the image lives inside a tool box, the reservation rows
		// are wrapped in vertical box edges ("│  ...  │"); those rows
		// look non-blank under a naive whitespace check but are still
		// reservation rows for this scan, so treat them as blank.
		foundInfo := false
		for j := i + 1; j < len(out); j++ {
			if strings.Contains(out[j], "image - ") {
				foundInfo = true
				break
			}
			if !isBoxBlankLine(out[j]) {
				break
			}
		}
		if !foundInfo {
			out[i] = ""
		}
	}
	return out
}

// isBoxBlankLine reports whether line is visually empty after
// stripping ANSI escape sequences, surrounding whitespace, and the
// vertical box edges drawn by the tool-box renderer. Used by
// clipBottomClippedImages so an image's reservation rows still count
// as blank when those rows are wrapped in "│  ...  │" inside a tool box.
func isBoxBlankLine(line string) bool {
	stripped := stripANSIBytes(line)
	stripped = strings.TrimSpace(stripped)
	stripped = strings.Trim(stripped, "│")
	stripped = strings.TrimSpace(stripped)
	return stripped == ""
}

// stripANSIBytes removes ANSI CSI escape sequences (ESC '[' ... final
// byte) from s without pulling in the regexp package. Mirrors the
// internal helper in package tui; the duplicated copy avoids exporting
// it just for one caller.
func stripANSIBytes(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			end := i + 2
			for end < len(s) {
				c := s[end]
				end++
				if c >= 0x40 && c <= 0x7e {
					break
				}
			}
			i = end
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// truncateLine shortens s so it fits within n display cells, with an
// ellipsis if trimmed. Used by the "sliding in" chips so a pasted
// novel doesn't blow past the status line.
func panelKeyName(k tui.Key) string {
	switch k.Kind {
	case tui.KeyUp:
		return "up"
	case tui.KeyDown:
		return "down"
	case tui.KeyLeft:
		return "left"
	case tui.KeyRight:
		return "right"
	case tui.KeyEnter:
		return "enter"
	case tui.KeyEsc:
		return "esc"
	case tui.KeyTab:
		return "tab"
	case tui.KeyBackspace:
		return "backspace"
	case tui.KeyDelete:
		return "delete"
	case tui.KeyHome:
		return "home"
	case tui.KeyEnd:
		return "end"
	case tui.KeyPageUp:
		return "pageup"
	case tui.KeyPageDown:
		return "pagedown"
	case tui.KeyRune:
		return "rune"
	default:
		return "unknown"
	}
}

func panelKeyText(k tui.Key) string {
	if k.Kind == tui.KeyRune {
		return string(k.Rune)
	}
	return ""
}

func truncateLine(s string, n int) string {
	if n <= 0 {
		return ""
	}
	// Collapse newlines — chips are single line.
	s = strings.ReplaceAll(s, "\n", " ↩ ")
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	if n <= 3 {
		return strings.Repeat(".", n)
	}
	return string(runes[:n-3]) + "..."
}

// ctrlCExitWindow is how long after a ctrl+c press a *second* press
// will exit instead of just clearing input. Long enough to be
// deliberate (rules out accidental key chord), short enough that the
// hint stays meaningful.
const ctrlCExitWindow = 2 * time.Second

// armCtrlCExit records the timestamp of the current ctrl+c so the next
// one within ctrlCExitWindow exits.
func (i *Interactive) armCtrlCExit() {
	i.mu.Lock()
	i.lastCtrlC = time.Now()
	i.mu.Unlock()
}

// ctrlCExitArmed reports whether a previous ctrl+c was recent enough
// that another press should now exit.
func (i *Interactive) ctrlCExitArmed() bool {
	i.mu.Lock()
	t := i.lastCtrlC
	i.mu.Unlock()
	return !t.IsZero() && time.Since(t) <= ctrlCExitWindow
}

// clearFileSuggestQuery strips the filter the user typed after the
// last "@", leaving the bare "@" so the picker stays open. Called when
// navigating between directory levels (Right/Left): the filter applied
// to the level the user was on, not the one being entered, so carrying
// it forward would wrongly hide the new directory's contents.
func (i *Interactive) clearFileSuggestQuery() {
	val := i.ed.Value()
	if idx := strings.LastIndex(val, "@"); idx >= 0 {
		i.ed.SetValue(val[:idx+1])
	}
}

// setToolExpansion updates long tool result expansion and replays the
// transcript so already-emitted rows reflect the new state.
func (i *Interactive) setToolExpansion(expanded bool) {
	i.mu.Lock()
	i.view.ExpandAll = expanded
	i.requestRendererClear()
	i.mu.Unlock()
	i.invalidate()
}

func (i *Interactive) toggleToolExpansion() {
	i.mu.Lock()
	expanded := !i.view.ExpandAll
	i.mu.Unlock()
	i.setToolExpansion(expanded)
}

func (i *Interactive) toggleBtwToolExpansion() {
	i.btwDialog.ToggleToolExpansion()
	i.mu.Lock()
	i.requestRendererClear()
	i.mu.Unlock()
	i.invalidate()
}

// confirmChildActive reports whether an interaction opened from slash input
// currently owns the keyboard while a tool confirmation remains pending.
func (i *Interactive) confirmChildActive() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.confirmChildActiveLocked()
}

func (i *Interactive) confirmChildActiveLocked() bool {
	return len(i.helpBlock) > 0 ||
		len(i.extNotes) > 0 ||
		i.dialog.Active() ||
		i.modelDialog.Active() ||
		i.llamaDialog.Active() ||
		i.rescueDialog.Active() ||
		i.sessionDialog.Active() ||
		i.subagentsDialog.Active() ||
		i.jumpDialog.Active() ||
		i.btwDialog.Active() ||
		i.skillsDialog.Active() ||
		i.changelogDialog.Active() ||
		i.logoutDialog.Active() ||
		i.telegramDialog.Active() ||
		i.settingsDialog.Active() ||
		i.sessionOpsDialog.Active() ||
		i.sessionTreeDialog.Active() ||
		i.extPanel.Active()
}

// restoreConfirmFocus returns input to confirmation after slash input is
// cleared or a child interaction closes. A non-empty editor keeps command
// input focused, and an active child continues to own its keys.
func (i *Interactive) restoreConfirmFocus() {
	if !i.confirmDialog.Active() || i.confirmDialog.Focused() {
		return
	}
	if !i.ed.IsEmpty() || i.confirmChildActive() {
		return
	}
	i.confirmDialog.Focus()
	i.invalidate()
}

func (i *Interactive) handleKey(ctx context.Context, k tui.Key) (done bool) {
	defer i.restoreConfirmFocus()

	// A bare Escape is eligible for the session-tree gesture only while the
	// main input owns the key. Any other key, modified Escape, or visible
	// child interaction resets the pending first tap before normal routing.
	if !isUnmodifiedEscape(k) || i.sessionTreeEscapeBlocked() {
		i.mu.Lock()
		i.doubleEscape.Reset()
		i.mu.Unlock()
	}

	// Dialogs route keys before the main clipboard handler below. Resolve text
	// here when a child interaction owns input so every editor and filter sees
	// the same KeyPaste event as terminal-native bracketed paste. Main chat is
	// left image-first to preserve macOS clipboard image attachments.
	if k.Kind == tui.KeyPasteClipboard && i.confirmChildActive() {
		resolved, pastedText, err := resolveClipboardText(k, tui.ReadClipboardText)
		if err != nil {
			i.mu.Lock()
			i.statusErr = "clipboard paste failed: " + err.Error()
			i.statusOK = ""
			i.mu.Unlock()
			return false
		}
		if pastedText {
			k = resolved
			i.mu.Lock()
			i.statusErr = ""
			i.statusOK = ""
			i.mu.Unlock()
		}
	}

	// Any key that isn't ctrl+c invalidates an armed ctrl+c-exit, so
	// pressing ctrl+c then typing then ctrl+c much later doesn't quit
	// unexpectedly. The hint message also goes stale; clear it.
	if k.Kind != tui.KeyCtrlC {
		i.mu.Lock()
		if !i.lastCtrlC.IsZero() {
			i.lastCtrlC = time.Time{}
			if strings.HasPrefix(i.statusOK, "input cleared") || strings.HasPrefix(i.statusOK, "press ctrl+c") {
				i.statusOK = ""
			}
		}
		i.mu.Unlock()
	}

	// Dialogs consume keys while open (except ctrl+c, which always closes them).

	// Confirmation owns input by default. Pressing / deliberately moves
	// focus to the main editor without answering the pending request, so
	// slash commands and the dialogs they open can run first.
	if i.confirmDialog.Focused() {
		if k.Kind == tui.KeyRune && k.Rune == '/' {
			i.confirmDialog.Blur()
			i.ed.Insert("/")
			i.suggest.Reset()
			i.invalidate()
			return false
		}
		if k.Kind == tui.KeyCtrlO {
			if i.btwDialog != nil && i.btwDialog.Active() {
				i.toggleBtwToolExpansion()
			} else {
				i.toggleToolExpansion()
			}
			return false
		}
		i.confirmDialog.HandleKey(k)
		i.invalidate()
		return false
	}
	if i.confirmDialog.Active() && !i.confirmChildActive() && k.Kind == tui.KeyEsc {
		i.ed.Clear()
		i.suggest.Reset()
		i.fileSuggest.Reset()
		i.confirmDialog.Focus()
		i.invalidate()
		return false
	}
	if i.dialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.dialog.Close()
			if i.cfg.AuthManager != nil {
				i.cfg.AuthManager.CancelOAuth()
			}
			return false
		}
		act := i.dialog.HandleKey(k)
		if act.StartAPIKey {
			i.startAPIKeyFlow(act.Provider)
		}
		if act.StartOAuth {
			i.startOAuthFlow(act.Provider)
		}
		if act.StartManual {
			i.startManualOAuthFlow(act.Provider)
		}
		if act.SubmitCode != "" {
			i.submitManualOAuthCode(act.SubmitCode)
		}
		if act.SaveLlama {
			i.saveLlamaCPPLogin(act.LlamaURL, act.LlamaAPIKey)
		}
		return false
	}
	if i.modelDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.modelDialog.Close()
			i.quickModelAssign = 0
			return false
		}
		act := i.modelDialog.HandleKey(k)
		if act.Close {
			i.quickModelAssign = 0
		}
		if act.ReasoningChanged {
			i.applyReasoningSetting(act.Reasoning)
		}
		if act.Select {
			if i.quickModelAssign > 0 {
				i.applyQuickModelSelection(i.quickModelAssign, act.Provider, act.Model)
				i.quickModelAssign = 0
			} else {
				i.applyModelSelection(act.Provider, act.Model)
			}
		}
		return false
	}
	if i.llamaDialog.Active() {
		i.llamaDialog.HandleKey(k)
		i.invalidate()
		return false
	}
	if i.rescueDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.rescueDialog.Close()
			i.invalidate()
			return false
		}
		act := i.rescueDialog.HandleKey(k)
		if act.Select {
			i.applyRescueSelection(act.Provider, act.Model, act.Prompt)
		}
		i.invalidate()
		return false
	}
	i.mu.Lock()
	sessionDialogActive := i.sessionDialog.Active()
	i.mu.Unlock()
	if sessionDialogActive {
		if k.Kind == tui.KeyCtrlC {
			i.mu.Lock()
			i.sessionDialog.Close()
			i.sessionLoads = nil
			i.mu.Unlock()
			i.invalidate()
			return false
		}
		manualRenamePath := ""
		i.mu.Lock()
		if k.Kind == tui.KeyEnter && i.sessionDialog.renaming && core.NormalizeSessionTitle(i.sessionDialog.rename) != "" && i.sessionDialog.cursor >= 0 && i.sessionDialog.cursor < len(i.sessionDialog.sessions) && i.cfg.CurrentSessionPath != nil {
			manualRenamePath = i.sessionDialog.sessions[i.sessionDialog.cursor].Path
		}
		i.mu.Unlock()
		manualRenameCurrent := manualRenamePath != "" && i.cfg.CurrentSessionPath() == manualRenamePath
		if manualRenameCurrent {
			i.markSessionTitleSwitching()
		}
		i.mu.Lock()
		act := i.sessionDialog.HandleKey(k)
		if act.Select || act.Close {
			i.sessionLoads = nil
		}
		i.mu.Unlock()
		if act.Select {
			i.applySessionSelection(act.Path)
		}
		if act.Err != nil {
			if manualRenameCurrent {
				i.restoreFailedSessionTitle()
			}
			i.mu.Lock()
			i.statusErr = "rename: " + act.Err.Error()
			i.statusOK = ""
			i.mu.Unlock()
		} else if act.Renamed && act.Path != "" && i.cfg.CurrentSessionPath != nil && i.cfg.CurrentSessionPath() == act.Path {
			i.setManualSessionTitle(act.RenameTitle)
		}
		// Always request a redraw after handling a key here: when esc
		// closes the picker, the overlay-close detection in the render
		// pass needs to run so the tall dialog rows get repainted from
		// the chat (otherwise VS Code's retained scrollback leaves a
		// duplicate frame on screen).
		i.invalidate()
		return false
	}
	if i.subagentsDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.subagentsDialog.Close()
			i.invalidate()
			return false
		}
		_, msg, errMsg := i.subagentsDialog.HandleKey(k)
		if msg != "" || errMsg != "" {
			i.mu.Lock()
			i.statusOK = msg
			i.statusErr = errMsg
			i.mu.Unlock()
		}
		i.invalidate()
		return false
	}
	if i.logoutDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.logoutDialog.Close()
			i.invalidate()
			return false
		}
		act := i.logoutDialog.HandleKey(k)
		if act.Select {
			i.doLogout(act.Target)
		}
		i.invalidate()
		return false
	}
	if i.telegramDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.telegramDialog.Close()
			i.invalidate()
			return false
		}
		act := i.telegramDialog.HandleKey(k)
		if act.Select {
			i.doTelegram(act.Action)
		}
		i.invalidate()
		return false
	}
	if i.settingsDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.settingsDialog.Close()
			i.invalidate()
			return false
		}
		act := i.settingsDialog.HandleKey(k)
		if act.ModelShortcutSlot > 0 {
			i.openQuickModelPicker(act.ModelShortcutSlot)
		}
		if act.Toggle {
			i.applySettingChange(act)
		}
		i.invalidate()
		return false
	}
	if i.sessionOpsDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.sessionOpsDialog.Close()
			i.invalidate()
			return false
		}
		act := i.sessionOpsDialog.HandleKey(k)
		if act.Select {
			i.doSessionOp(act.Action, "")
		}
		i.invalidate()
		return false
	}
	if i.sessionTreeDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.sessionTreeDialog.Close()
			i.invalidate()
			return false
		}
		act := i.sessionTreeDialog.HandleKey(k)
		if act.Select {
			if act.Target.Role != "" || act.Target.SourcePath != "" || act.Target.IsBoundary() || act.Target.UserDraft != "" {
				i.applySessionTreeTarget(act.Target, act.TurnNo)
			} else {
				// Keep package embedders that still return the scalar action
				// source-compatible while the dialog migrates to Target.
				i.applySessionTreeMessageSelection(act.Path, act.MessageIdx, act.TurnNo, act.Role, act.Prompt)
			}
		}
		i.invalidate()
		return false
	}
	if i.extPanel.Active() {
		if k.Kind == tui.KeyCtrlC || k.Kind == tui.KeyEsc {
			if i.cfg.Extensions != nil {
				_ = i.cfg.Extensions.SendPanelClose(i.extPanel.ext, i.extPanel.id)
			}
			i.extPanel.Close()
			i.invalidate()
			return false
		}
		if i.cfg.Extensions != nil {
			_ = i.cfg.Extensions.SendPanelKey(i.extPanel.ext, i.extPanel.id, panelKeyName(k), panelKeyText(k))
		}
		return false
	}
	if i.jumpDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.jumpDialog.Close()
			i.pendingFork = false
			return false
		}
		act := i.jumpDialog.HandleKey(k)
		if act.Select {
			if i.pendingFork {
				i.applyForkSelection(act.MessageIdx)
			} else {
				i.applyJumpSelection(act.MessageIdx, act.TurnNo)
			}
		}
		// If the user dismissed the dialog without selecting, also
		// clear the pending-fork flag so a later plain /jump isn't
		// hijacked.
		if act.Close {
			i.pendingFork = false
		}
		return false
	}
	if i.btwDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.btwDialog.Close()
			i.invalidate()
			return false
		}
		if k.Kind == tui.KeyCtrlO {
			i.toggleBtwToolExpansion()
			return false
		}
		i.btwDialog.HandleKey(k, i.invalidate)
		return false
	}
	if i.skillsDialog.Active() {
		if k.Kind == tui.KeyCtrlC {
			i.skillsDialog.Close()
			i.invalidate()
			return false
		}
		i.skillsDialog.HandleKey(k)
		i.invalidate()
		return false
	}
	if i.changelogDialog.Active() {
		if closed := i.changelogDialog.HandleKey(k); closed {
			// User dismissed; let the parent persist the
			// LastChangelogShown marker via the close callback.
			if i.cfg.OnChangelogDismiss != nil {
				i.cfg.OnChangelogDismiss()
			}
		}
		i.invalidate()
		return false
	}

	if slot := quickModelShortcutSlot(k); slot > 0 {
		i.applyQuickModelShortcut(slot)
		return false
	}

	// The first eligible bare Escape is deliberately left to the existing
	// Escape path (which clears idle input or cancels a turn). A second tap
	// within the gesture window only gets first refusal while the cheap,
	// local eligibility rules still hold. If a turn started, a message was
	// queued, or the input became ineligible between taps, reset the detector
	// and fall through so Escape retains its normal cancellation behavior.
	// Once those rules still hold, the full tree gate owns the second tap:
	// family/read failures are fail-closed but are nevertheless consumed so
	// they cannot fall through into ordinary editor handling.
	if isUnmodifiedEscape(k) {
		now := i.sessionTreeEscapeNow()
		i.mu.Lock()
		consumed := i.doubleEscape.Consume(now)
		i.mu.Unlock()
		if consumed {
			if i.canArmSessionTreeEscape() {
				i.openSessionTree()
				return false
			}
			i.mu.Lock()
			i.doubleEscape.Reset()
			i.mu.Unlock()
		} else if i.canArmSessionTreeEscape() {
			i.mu.Lock()
			i.doubleEscape.Arm(now)
			i.mu.Unlock()
		}
	}

	// Global keys.
	switch k.Kind {
	case tui.KeyCtrlC:
		i.mu.Lock()
		loadingSession := i.sessionLoading
		i.mu.Unlock()
		if loadingSession {
			i.markCtrlCExit()
			return true
		}
		// While busy: do NOT cancel the turn. ctrl+c during a
		// running turn is almost always reflex muscle memory
		// ("be quiet" in a shell) rather than a deliberate
		// decision to kill a multi-minute model call that's
		// already cost tokens. Use esc to interrupt a turn; use
		// a deliberate double-ctrl+c to exit zut entirely. First
		// press arms the exit hint, second press within
		// ctrlCExitWindow quits.
		if i.busy {
			if i.ctrlCExitArmed() {
				i.markCtrlCExit()
				return true
			}
			i.mu.Lock()
			i.statusOK = "press ctrl+c again to exit, esc to cancel the turn"
			i.statusErr = ""
			i.mu.Unlock()
			i.armCtrlCExit()
			return false
		}
		// Idle: first press clears the editor (and any queued
		// follow-up messages); a second press within ctrlCExitWindow
		// exits. With both an empty editor and no queue the first
		// press still just arms — require a deliberate double-tap.
		ag := i.agent
		pending := 0
		if ag != nil {
			pending = ag.QueuedMessageCount()
		}
		hadInput := !i.ed.IsEmpty() || len(i.queued) > 0 || pending > 0
		if hadInput {
			i.ed.Clear()
			i.clipboardImages = nil
			i.suggest.Reset()
			if ag != nil {
				ag.DrainQueuedMessages()
			}
			i.mu.Lock()
			i.queued = nil
			i.statusOK = "input cleared"
			i.statusErr = ""
			i.mu.Unlock()
			i.armCtrlCExit()
			return false
		}
		if i.ctrlCExitArmed() {
			i.markCtrlCExit()
			return true
		}
		i.mu.Lock()
		i.statusOK = "press ctrl+c again to exit"
		i.statusErr = ""
		i.mu.Unlock()
		i.armCtrlCExit()
		return false
	case tui.KeyEsc:
		// Esc interrupts a running turn — but only when nothing
		// else on screen wants to consume the key first. The slash
		// popup has its own Esc behaviour (close + clear editor),
		// and transient overlays like the /help block and extension
		// notes should dismiss on Esc before we even consider the
		// turn. Without these guards, a casual Esc press after
		// running /help on a busy turn rips the turn away.
		if i.suggest.Active(i.ed.Value()) || i.fileSuggest.Active(i.ed.Value()) {
			break
		}
		i.mu.Lock()
		hadHelp := len(i.helpBlock) > 0
		hadNotes := len(i.extNotes) > 0
		if hadHelp {
			i.helpBlock = nil
		}
		if hadNotes {
			i.extNotes = nil
		}
		i.mu.Unlock()
		if hadHelp || hadNotes {
			i.invalidate()
			return false
		}
		i.mu.Lock()
		busyCancel := i.busy && i.cancelTurn != nil
		cancelTurn := i.cancelTurn
		var handoff json.RawMessage
		var persistHandoff bool
		if busyCancel {
			handoff, persistHandoff = i.resetCompactContinuationLocked()
		}
		i.mu.Unlock()
		if busyCancel {
			i.updateActiveGoal(core.GoalPaused, "interrupted by user")
			cancelTurn()
			// If a confirm dialog is pending, refuse it so the agent
			// goroutine unblocks and the context cancellation can
			// actually take effect.
			i.confirmDialog.CancelAll("turn cancelled")
			if persistHandoff {
				i.persistCompactHandoff(handoff)
			}
			return false
		}
	case tui.KeyCtrlD:
		if i.ed.IsEmpty() && !i.busy {
			return true
		}
	case tui.KeyCtrlB:
		i.mu.Lock()
		i.rightBarHidden = !i.rightBarHidden
		i.mu.Unlock()
		i.requestRendererInvalidate()
		i.invalidate()
		return false
	case tui.KeyCtrlL:
		i.requestRendererClear()
		i.invalidate()
		return false
	case tui.KeyPasteClipboard:
		i.pasteClipboard(ctx)
		return false
	case tui.KeyCtrlO:
		i.toggleToolExpansion()
		return false
	case tui.KeyPageUp:
		i.scrollBy(+i.chatPage())
		return false
	case tui.KeyPageDown:
		i.scrollBy(-i.chatPage())
		return false
	case tui.KeyUp:
		// Alt/Option+Up: pop the most recently queued ("sliding in")
		// message back into the editor so the user can edit and
		// resend it. Repeated presses keep peeling messages off the
		// tail of the queue; each press *replaces* the editor
		// contents (we don't append/push). When the queue is empty
		// the keypress falls through to the normal scroll behavior.
		if k.Alt {
			i.mu.Lock()
			var text string
			if i.agent != nil {
				text, _ = i.agent.PopQueuedMessage()
			}
			if text == "" {
				if n := len(i.queued); n > 0 {
					text = i.queued[n-1]
					i.queued = i.queued[:n-1]
				}
			}
			i.mu.Unlock()
			if text != "" {
				i.ed.SetValue(text)
				i.inputHistoryIndex = -1
				i.invalidate()
				return false
			}
		}
		// In multi-line / wrapped input, Up first moves inside the editor.
		// At the editor's top edge, plain Up can browse input history when
		// history browsing is safe/active; otherwise it falls back to chat
		// scrolling, preserving the old single-line scroll behavior.
		if !i.suggest.Active(i.ed.Value()) && !i.fileSuggest.Active(i.ed.Value()) {
			if i.ed.MoveVertical(-1) {
				i.invalidate()
				return false
			}
			if !k.Alt && i.handleInputHistoryKey(k) {
				return false
			}
			i.scrollBy(+3)
			return false
		}
	case tui.KeyDown:
		if !i.suggest.Active(i.ed.Value()) && !i.fileSuggest.Active(i.ed.Value()) {
			if i.ed.MoveVertical(+1) {
				i.invalidate()
				return false
			}
			if !k.Alt && i.handleInputHistoryKey(k) {
				return false
			}
			if i.scrollOffset > 0 {
				i.scrollBy(-3)
			}
			return false
		}
	}

	// Note: we intentionally do NOT gate the editor on i.busy here.
	// Typing while the agent is working is supported — submitted
	// messages are queued and delivered as follow-up turns when the
	// current turn ends. See the submit handler below.

	if k.Kind == tui.KeyEnter && (k.Alt || k.Shift) {
		i.ed.HandleKey(k)
		return false
	}

	// Slash suggestions: intercept up/down/tab/enter when the popup is visible.
	if i.suggest.Active(i.ed.Value()) {
		switch k.Kind {
		case tui.KeyUp:
			i.suggest.Up()
			return false
		case tui.KeyDown:
			i.suggest.Down()
			return false
		case tui.KeyPageUp:
			i.suggest.PageUp()
			return false
		case tui.KeyPageDown:
			i.suggest.PageDown()
			return false
		case tui.KeyTab:
			if name := i.suggest.Selection(i.ed.Value()); name != "" {
				i.ed.SetValue(name)
				i.suggest.Reset()
			}
			return false
		case tui.KeyEnter:
			// Enter on an ambiguous or partial slash prefix: complete to the
			// currently highlighted command and run it. That way typing
			// "/lo" + enter picks whichever of /login or /logout is selected
			// in the popup instead of submitting "/lo" as unknown. Also
			// clear the editor so the command doesn't linger after the
			// dialog opens/closes.
			if name := i.suggest.Selection(i.ed.Value()); name != "" {
				i.ed.Clear()
				i.suggest.Reset()
				return i.runSlash(ctx, name)
			}
		case tui.KeyEsc:
			i.ed.Clear()
			i.suggest.Reset()
			return false
		}
	}

	// File suggestions: intercept up/down/tab/enter when the @-popup is visible.
	if i.fileSuggest.Active(i.ed.Value()) {
		switch k.Kind {
		case tui.KeyUp:
			i.fileSuggest.Up()
			return false
		case tui.KeyDown:
			i.fileSuggest.Down()
			return false
		case tui.KeyRight:
			// Open selected directory. The filter the user typed picked
			// that directory at the current level; once we descend it no
			// longer applies to the directory's contents, so clear it.
			// Otherwise typing "@eda" then right would re-filter inside
			// eda/ by "eda" and show nothing.
			if i.fileSuggest.Right() {
				i.clearFileSuggestQuery()
			}
			return false
		case tui.KeyLeft:
			// Go back to parent directory. Clear the filter for the same
			// reason as Right: it was scoped to the level we just left.
			if i.fileSuggest.Left() {
				i.clearFileSuggestQuery()
			}
			return false
		case tui.KeyEnter:
			if entry, ok := i.fileSuggest.SelectedEntry(i.ed.Value()); ok {
				var chip string
				if entry.isDir {
					chip = "[dir:" + entry.rel + "/]"
				} else {
					chip = "[file:" + entry.rel + "]"
				}
				val := i.ed.Value()
				if idx := strings.LastIndex(val, "@"); idx >= 0 {
					val = val[:idx]
				}
				i.ed.SetValue(val + chip + " ")
				i.fileSuggest.Reset()
			}
			return false
		case tui.KeyEsc:
			val := i.ed.Value()
			if idx := strings.LastIndex(val, "@"); idx >= 0 {
				i.ed.SetValue(val[:idx])
			}
			i.fileSuggest.Reset()
			return false
		}
	}

	// Tab-complete a path token in the editor when no popup is open.
	// Recognises tokens that look like paths (start with ~, /, ./, ../
	// or contain a slash); shell-style completion expands ~, lists the
	// parent dir, and completes the basename to the longest common
	// prefix. Single match: full replace and trailing / for dirs.
	// No match: no-op. Plain bare words (no slash, no tilde) fall
	// through so Tab keeps its current no-op behaviour outside paths.
	if k.Kind == tui.KeyTab && !i.suggest.Active(i.ed.Value()) && !i.fileSuggest.Active(i.ed.Value()) {
		if i.tryPathTabComplete() {
			return false
		}
	}

	if i.inputHistoryIndex >= 0 && k.Kind != tui.KeyUp && k.Kind != tui.KeyDown {
		i.inputHistoryIndex = -1
	}

	if k.Kind == tui.KeyEsc {
		i.clipboardImages = nil
	}
	if submit := i.ed.HandleKey(k); submit {
		// SubmitValue() expands any [pasted text #N +L lines]
		// placeholders back into their bodies; the raw Value()
		// is only what the user sees on screen.
		text := strings.TrimRight(i.ed.SubmitValue(), "\n")
		// Expand [file:name] and [dir:name/] chips to full paths.
		text = expandFileChips(text, i.cfg.CWD)
		text, images := preparePromptWithClipboardImages(text, i.clipboardImages)
		if text == "" && len(images) == 0 {
			return false
		}
		clearSubmittedInput := func() {
			i.clipboardImages = nil
			i.ed.Clear()
			i.inputHistoryIndex = -1
			i.suggest.Reset()
			i.fileSuggest.Reset()
		}

		// Shell escapes and slash commands are text-only. An image-bearing
		// draft remains a normal provider prompt instead of silently dropping
		// its attachments.
		if len(images) == 0 {
			if cmd, ok := shellEscapeCommand(text); ok {
				clearSubmittedInput()
				i.startShellEscape(ctx, cmd)
				return false
			}
		}

		if len(images) == 0 && looksLikeSlashCommand(text) {
			clearSubmittedInput()
			head := text
			rest := ""
			if idx := strings.IndexAny(text, " \t"); idx >= 0 {
				head = text[:idx]
				rest = strings.TrimSpace(text[idx:])
			}
			if !isKnownSlashCommand(text) {
				// Try extensions before giving up. Extensions register
				// commands by bare name (no leading slash); strip it here.
				extName := strings.TrimPrefix(head, "/")
				if i.cfg.Extensions != nil && i.cfg.Extensions.HasCommand(extName) {
					go i.invokeExtensionCommand(ctx, extName, rest)
					return false
				}
				i.mu.Lock()
				i.statusErr = "unknown command " + head + " — type /help to see the list"
				i.statusOK = ""
				i.mu.Unlock()
				return false
			}
			// Slash commands run regardless of busy state. Commands that
			// would mutate the transcript or replace the agent (/clear,
			// /compact, /logout, /login, /model, /fork, /session fork)
			// cancel the active turn
			// first and wait for the goroutine to wind down so they don't
			// race with a streaming response. Safe commands (/help,
			// /jump, /sessions, /jail, /unjail, /exit) run immediately
			// without disturbing the active turn.
			if slashCommandCancelsTurn(text) {
				i.cancelAndWaitForIdle()
			}
			return i.runSlash(ctx, text)
		}

		if i.agent == nil {
			i.mu.Lock()
			i.statusErr = "not logged in. type /login first."
			i.mu.Unlock()
			return false
		}
		// Mirror the user's typed prompt into the paired Telegram
		// chat (when the bridge is active) so the Telegram thread
		// stays a complete record of the session, not just the half
		// that originated on the phone. On a goroutine so the
		// network write doesn't delay the local turn.
		if i.telegramBridge != nil && i.telegramBridge.Active() {
			go i.telegramBridge.OnUserTyped(text)
		}
		i.maybeStartSessionTitle(ctx, text)
		// If a turn is already in flight, queue this prompt inside the
		// agent loop so it is delivered at the next safe model-call
		// boundary instead of waiting for the whole run to finish.
		i.mu.Lock()
		busy := i.busy
		compacting := i.compacting
		ag := i.agent
		i.mu.Unlock()
		if busy {
			if len(images) > 0 {
				i.mu.Lock()
				i.statusErr = "can't queue clipboard images while a turn is running; wait for the current turn to finish"
				i.mu.Unlock()
				i.invalidate()
				return false
			}
			clearSubmittedInput()
			var handoff json.RawMessage
			var persistHandoff bool
			if ag != nil && !compacting {
				i.mu.Lock()
				handoff, persistHandoff = i.resetCompactContinuationLocked()
				ag.QueueMessage(text)
				i.mu.Unlock()
			} else {
				i.mu.Lock()
				handoff, persistHandoff = i.resetCompactContinuationLocked()
				i.queued = append(i.queued, text)
				i.mu.Unlock()
			}
			if persistHandoff {
				i.persistCompactHandoff(handoff)
			}
			i.invalidate()
			return false
		}
		clearSubmittedInput()
		i.startTurnWithImages(ctx, text, images)
	}
	return false
}

func (i *Interactive) pasteClipboard(ctx context.Context) {
	image, ok, _ := tui.ReadClipboardImage(ctx)
	if ok {
		i.mu.Lock()
		marker := fmt.Sprintf("[clipboard image #%d]", len(i.clipboardImages)+1)
		i.clipboardImages = append(i.clipboardImages, clipboardImageAttachment{
			Marker: marker,
			Image:  provider.ImageBlock{MimeType: image.MimeType, Data: image.Data},
		})
		i.ed.Insert(marker + " ")
		i.statusOK = ""
		i.statusErr = ""
		i.mu.Unlock()
		return
	}
	key, pastedText, err := resolveClipboardText(tui.Key{Kind: tui.KeyPasteClipboard}, tui.ReadClipboardText)
	i.mu.Lock()
	defer i.mu.Unlock()
	if err != nil {
		i.statusErr = "clipboard paste failed: " + err.Error()
		i.statusOK = ""
		return
	}
	if pastedText {
		i.ed.HandleKey(key)
		i.statusErr = ""
		i.statusOK = ""
		return
	}
	i.statusErr = "clipboard does not contain text or an image"
	i.statusOK = ""
}

func (i *Interactive) handleInputHistoryKey(k tui.Key) bool {
	if k.Kind != tui.KeyUp && k.Kind != tui.KeyDown {
		return false
	}
	// Do not steal normal vertical cursor movement. History browsing can only
	// start from an empty editor; once active, Up/Down keep walking
	// the ring so repeated presses work even though the editor now
	// contains the selected historical prompt.
	if i.inputHistoryIndex < 0 && !i.ed.IsEmpty() {
		return false
	}
	hist := i.inputHistory()
	if len(hist) == 0 {
		return false
	}

	if i.inputHistoryIndex < 0 {
		// Start just after the newest item so Up lands on the most
		// recent user prompt. A lone Down from an empty editor is not
		// history navigation; let the caller fall through to normal UI
		// behavior instead.
		if k.Kind != tui.KeyUp {
			return false
		}
		i.inputHistoryIndex = len(hist)
	}

	switch k.Kind {
	case tui.KeyUp:
		if i.inputHistoryIndex > 0 {
			i.inputHistoryIndex--
		}
	case tui.KeyDown:
		if i.inputHistoryIndex < len(hist) {
			i.inputHistoryIndex++
		}
	}

	if i.inputHistoryIndex >= len(hist) {
		i.ed.Clear()
	} else {
		i.ed.SetValue(hist[i.inputHistoryIndex])
	}
	return true
}

func (i *Interactive) inputHistory() []string {
	if i.agent == nil {
		return nil
	}
	msgs := i.agent.Messages()
	hist := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if !isForkableUserMessage(m) {
			continue
		}
		text := userMessageText(m)
		if strings.TrimSpace(text) == "" {
			continue
		}
		hist = append(hist, text)
	}
	return hist
}

func userMessageText(m provider.Message) string {
	var sb strings.Builder
	for _, c := range m.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

// invokeExtensionCommand fires an extension-registered slash command
// in a background goroutine, awaits the response, and applies the
// requested action (prompt / insert / display / noop). Errors and
// timeouts surface as a status_err line.

func (i *Interactive) invokeExtensionCommand(ctx context.Context, name, args string) {
	resp, err := i.cfg.Extensions.Invoke(ctx, name, args, 30*time.Second)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "extension /" + name + ": " + err.Error()
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if resp.Error != "" {
		i.mu.Lock()
		i.statusErr = "extension /" + name + ": " + resp.Error
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	switch resp.Action {
	case "open_panel":
		if resp.OpenPanel != nil {
			extName := name
			if i.cfg.Extensions != nil {
				if owner := i.cfg.Extensions.CommandOwner(name); owner != "" {
					extName = owner
				}
			}
			i.OpenPanel(extName, *resp.OpenPanel)
		}
	case "prompt":
		if strings.TrimSpace(resp.Prompt) == "" {
			return
		}
		i.startTurn(i.runCtx, resp.Prompt)
	case "insert":
		i.ed.Insert(resp.Insert)
		i.invalidate()
	case "display":
		i.appendExtensionNote(name, resp.Display, "info")
	case "noop", "":
		// nothing
	default:
		i.mu.Lock()
		i.statusErr = "extension /" + name + ": unknown action " + resp.Action
		i.mu.Unlock()
		i.invalidate()
	}
}

// appendExtensionNote renders an extension-originated note in the
// chat. Levels: "info" (muted), "warn" (warning), "error" (error),
// "success" (tool/ok green).
func (i *Interactive) appendExtensionNote(extName, msg, level string) {
	if msg == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	color := i.cfg.Theme.Muted
	switch level {
	case "warn":
		color = i.cfg.Theme.Warning
	case "error":
		color = i.cfg.Theme.Error
	case "success":
		color = i.cfg.Theme.Tool
	}
	prefix := i.cfg.Theme.FGColor(i.cfg.Theme.Accent, "["+extName+"] ")
	for _, line := range strings.Split(msg, "\n") {
		i.statusOK = "" // clear any stale ok
		i.statusErr = ""
		i.extNotes = append(i.extNotes, prefix+i.cfg.Theme.FGColor(color, line))
	}
	// Extension slash commands complete asynchronously. If their result is
	// displayed while confirmation is waiting, keep Esc assigned to the note
	// until it is dismissed rather than letting it answer the tool call.
	i.confirmDialog.Blur()
}

// HostHooks implementation for the extension manager. The manager
// holds an interface, not a concrete *Interactive, so these methods
// are the only thing the manager sees.

// Notify is the manager's NotifyFromExt entry point.
func (i *Interactive) Notify(extName, level, message string) {
	i.appendExtensionNote(extName, message, level)
	i.invalidate()
}

// Alert is the manager's AlertFromExt entry point. Alerts are emitted
// through the same terminal writer as redraws while holding the TUI mutex,
// so a BEL cannot be interleaved with a frame update.
func (i *Interactive) Alert(_ string, alert extproto.AlertRequest) {
	i.mu.Lock()
	i.emitAlertLocked(alert)
	i.mu.Unlock()
}

func terminalAlertsEnabled(enabled *bool) bool {
	return enabled == nil || *enabled
}

// emitAlertLocked applies the shared terminal-alert policy. The caller must
// hold i.mu because terminal writes share the renderer's output boundary.
func (i *Interactive) emitAlertLocked(alert extproto.AlertRequest) {
	if alert.Kind != extproto.AlertKindBell || !terminalAlertsEnabled(i.cfg.TerminalAlertsEnabled) || i.cfg.Terminal == nil {
		return
	}
	_ = tui.WriteBell(i.cfg.Terminal)
}

// scheduleMainAlert defers a main-session alert until the next redraw
// commits the final frame. Keeping this deferred even when no paced text is
// pending avoids racing the pacer's final state transition.
func (i *Interactive) scheduleMainAlert(reason string) {
	if reason == "" {
		return
	}
	i.mu.Lock()
	i.pendingAlert = &extproto.AlertRequest{Kind: extproto.AlertKindBell, Reason: reason}
	i.mu.Unlock()
	i.invalidate()
}

// ClearNotes removes every note line owned by extName from the
// bottom-sticky ext-notes block. Extensions use this to retract a
// transient status line (e.g. an approval prompt) once it no longer
// applies, instead of leaving it stacked forever. Notes from other
// extensions and internal notes (auto-compact) are left untouched.
func (i *Interactive) ClearNotes(extName string) {
	marker := "[" + extName + "] "
	i.mu.Lock()
	if len(i.extNotes) == 0 {
		i.mu.Unlock()
		return
	}
	kept := i.extNotes[:0:0]
	changed := false
	for _, line := range i.extNotes {
		if strings.Contains(line, marker) {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	if changed {
		i.extNotes = kept
	}
	i.mu.Unlock()
	if changed {
		i.invalidate()
	}
}

// Submit feeds text through the agent loop as if the user had typed it.
func (i *Interactive) Submit(text string) {
	if cmd, ok := shellEscapeCommand(text); ok {
		i.startShellEscape(i.runCtx, cmd)
		return
	}
	parent := i.runCtx
	if parent == nil {
		parent = context.Background()
	}
	i.mu.Lock()
	awaitingStartupPre := i.awaitingStartupPre
	i.mu.Unlock()
	if !awaitingStartupPre {
		i.maybeStartSessionTitle(parent, text)
	}
	i.startTurn(parent, text)
}

// completeStartupPre applies deferred InitialInput after entry.pre finishes.
// When AutoSubmitInitial was set, the deferred prompt is submitted; otherwise
// it only pre-fills the editor (CLI-supplied prompts).
func (i *Interactive) completeStartupPre() {
	i.mu.Lock()
	if !i.awaitingStartupPre {
		i.mu.Unlock()
		return
	}
	i.awaitingStartupPre = false
	deferred := i.deferredInitialInput
	auto := i.autoSubmitDeferred
	i.deferredInitialInput = ""
	i.autoSubmitDeferred = false
	onDone := i.cfg.OnStartupPreDone
	i.mu.Unlock()
	if onDone != nil {
		onDone()
	}
	i.startupPreDone <- startupPreResult{deferred: deferred, autoSubmit: auto}
	i.invalidate()
}

// applyStartupPreResult runs on the TUI event loop so the editor remains
// single-threaded. Input entered while resources were reloading wins over the
// deferred prefill rather than being overwritten.
func (i *Interactive) applyStartupPreResult(result startupPreResult) {
	if result.deferred == "" {
		return
	}
	if result.autoSubmit {
		i.Submit(result.deferred)
		return
	}
	if i.ed.IsEmpty() {
		i.ed.SetValue(result.deferred)
	}
}

// ApplySessionAgent swaps the live agent after a validated session resume.
// Unlike ApplyChangedCWD it does not mutate cwd-scoped resources or startup
// context; the session loader is changing the transcript/provider only.
func (i *Interactive) ApplySessionAgent(ag *core.Agent, providerName, model string) {
	i.ApplySessionAgentWithCompactHandoff(ag, providerName, model, nil)
}

func (i *Interactive) ApplySessionAgentWithCompactHandoff(ag *core.Agent, providerName, model string, compactHandoff json.RawMessage) {
	if ag == nil {
		return
	}
	i.agentMu.Lock()
	defer i.agentMu.Unlock()
	i.mu.Lock()
	i.prepareReplacementAgentLocked(ag)
	i.compactContinuation = decodeCompactHandoff(compactHandoff)
	i.agent = ag
	i.cfg.Provider = providerName
	i.cfg.Model = model
	i.view.Messages = filterHiddenTranscriptMessages(ag.Messages())
	i.cumUsage = ag.Cost()
	last := ag.LastTurnUsage()
	i.lastCtxInput = last.InputTokens + last.CacheReadTokens + last.CacheWriteTokens
	if len(i.view.Messages) > initialResumeTailLimit {
		i.view.TailLimit = initialResumeTailLimit
	} else {
		i.view.TailLimit = 0
	}
	i.view.InvalidateRenderCache()
	i.toolCalls = map[string]*tui.ToolCallView{}
	i.toolOrder = nil
	i.toolGate = map[string]int{}
	i.helpBlock = nil
	i.extNotes = nil
	i.parkedTurn = 0
	i.parkedTotal = 0
	i.mu.Unlock()
	i.invalidate()
}

// SetSubagentSessionScope changes the live-worker filter and then requests a
// redraw. Session changes must use this boundary after their host/session
// state commits so input-adjacent activity cannot remain scoped to the prior
// session until another UI event arrives.
func (i *Interactive) SetSubagentSessionScope(sessionID string) {
	if i.cfg.Supervisor == nil {
		return
	}
	i.cfg.Supervisor.SetActiveSession(sessionID)
	i.invalidate()
}

// ApplyChangedCWD is called by hosts after a successful /cd hook that do
// not provide startup context metadata.
func (i *Interactive) ApplyChangedCWD(ag *core.Agent, provider, model, cwd string) {
	i.applyChangedCWD(ag, provider, model, cwd, nil)
}

// ApplyChangedCWDWithStartupContext swaps a rebuilt agent and its display-only
// startup context into the running TUI after a successful /cd hook.
func (i *Interactive) ApplyChangedCWDWithStartupContext(ag *core.Agent, provider, model, cwd string, startupContextPaths []string) {
	i.applyChangedCWD(ag, provider, model, cwd, startupContextPaths)
}

func (i *Interactive) applyChangedCWD(ag *core.Agent, provider, model, cwd string, startupContextPaths []string) {
	home, _ := os.UserHomeDir()
	profiles, _ := subagents.Discover(cwd, home)
	subagentsAddendum := subagents.SystemPromptAddendum(profiles)

	i.agentMu.Lock()
	defer i.agentMu.Unlock()
	i.mu.Lock()
	i.prepareReplacementAgentLocked(ag)
	i.resetCompactContinuationLocked()
	i.agent = ag
	i.cfg.CWD = cwd
	i.cfg.SubagentsSystemAddendum = subagentsAddendum
	i.managedAutoSubagentsAddenda = autoSubagentsAddenda(i.cfg, i.autoSubagentsEnabledLocked())
	i.cfg.StartupContextPaths = append([]string(nil), startupContextPaths...)
	i.view.StartupContextPaths = nil
	i.view.InvalidateRenderCache()
	if i.cfg.ShowInstructionsAtStartup != nil && *i.cfg.ShowInstructionsAtStartup {
		i.view.StartupContextPaths = append(i.view.StartupContextPaths, startupContextPaths...)
	}
	// Re-report the working directory to the terminal so "new tab here"
	// tracks the /cd change (OSC 7, see issue #38).
	if i.cfg.Terminal != nil {
		if seq := tui.ReportCWD(cwd); seq != "" {
			_, _ = i.cfg.Terminal.Write([]byte(seq))
		}
	}
	i.cfg.Provider = provider
	i.cfg.Model = model
	titleCancel := i.titleCancel
	i.titleCancel = nil
	i.titleVersion++
	i.sessionTitle = ""
	i.titleRealPromptSeen = false
	i.titleGenerationStarted = false
	i.writeTerminalTitleLocked("")
	i.toolCalls = map[string]*tui.ToolCallView{}
	i.toolOrder = nil
	i.toolGate = map[string]int{}
	i.helpBlock = nil
	i.parkedTurn = 0
	i.statusErr = ""
	i.mu.Unlock()
	if titleCancel != nil {
		titleCancel()
	}
	i.fileSuggest.Reset()
	i.fileSuggest.SetCWD(cwd)
	i.invalidate()
}

// SubmitSlash runs text as a slash command in the TUI as if the user
// had typed it. text must start with '/' — callers that hand it
// plain prose silently get a no-op so a misbehaving extension can't
// run a stray prompt through this path. Read-only commands run in
// place; commands that would mutate the transcript or replace the
// agent cancel the active turn first via the same path the editor
// uses for typed slash commands.
func (i *Interactive) SubmitSlash(text string) {
	i.mu.Lock()
	i.doubleEscape.Reset()
	i.mu.Unlock()
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return
	}
	if slashCommandCancelsTurn(text) {
		i.cancelAndWaitForIdle()
	}
	i.runSlash(i.runCtx, text)
	i.invalidate()
}

// SubmitOrQueue runs text immediately if the agent is idle, or
// appends it to the pending queue if a turn is already in flight.
// Used by the telegram bridge (and by the editor submit path) so
// both input sources share the same "queue behind an active turn"
// semantics. Images are ignored for now — only the text prompt is
// forwarded — because the queued-prompt path is text-only; a
// follow-up can expand the queue entry to carry images.
func (i *Interactive) SubmitOrQueue(text string, images []provider.ImageBlock) {
	i.submitOrQueue(text, images, true)
}

func (i *Interactive) submitOrQueue(text string, images []provider.ImageBlock, userInput bool) {
	if cmd, ok := shellEscapeCommand(text); ok {
		i.startShellEscape(i.runCtx, cmd)
		return
	}
	i.mu.Lock()
	if i.agent == nil {
		i.statusErr = "not logged in. type /login first."
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.mu.Unlock()
	if userInput && i.cfg.EnsureMission != nil {
		if err := i.cfg.EnsureMission(text); err != nil {
			i.ReportError(fmt.Errorf("persist mission: %w", err))
			return
		}
	}
	// User input is an orchestration event, not merely a queue append. When
	// the manager is dormant behind a sealed worker wave, the coordinator makes
	// this the next manager turn and carries any completed worker summary with it.
	if userInput && i.coordinatorAcceptsUserInput() {
		if actions := i.applyCoordinator(orchestration.Event{Kind: orchestration.EventUserInput, Text: text}); len(actions) != 0 {
			i.executeCoordinatorActions(actions)
			return
		}
	}
	i.maybeStartSessionTitle(i.runCtx, text)
	i.mu.Lock()
	if i.busy {
		// Queue text only; images are dropped for queued prompts. Keep the
		// interactive mutex held through Agent.QueueMessage so turn teardown
		// cannot inspect the agent queue between the busy check and enqueue.
		// Compaction uses the host queue because it has no active agent loop
		// to drain Agent.QueueMessage entries.
		var handoff json.RawMessage
		var persistHandoff bool
		if i.agent != nil && !i.compacting {
			handoff, persistHandoff = i.resetCompactContinuationLocked()
			i.agent.QueueMessage(text)
		} else {
			handoff, persistHandoff = i.resetCompactContinuationLocked()
			i.queued = append(i.queued, text)
		}
		i.mu.Unlock()
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
		i.invalidate()
		return
	}
	i.mu.Unlock()
	i.startTurnWithImages(i.runCtx, text, images)
}

// CancelTurn aborts the active turn if one is running. Used by the
// telegram bridge when the paired user sends /stop.
// ChangelogVersion returns the version string of the changelog
// currently shown (or last shown). Used by the dismiss callback
// to store the correct version for dev builds.
func (i *Interactive) ChangelogVersion() string {
	if i.changelogDialog != nil {
		return i.changelogDialog.version
	}
	return ""
}

func (i *Interactive) CancelTurn() {
	i.mu.Lock()
	cancel := i.cancelTurn
	i.mu.Unlock()
	if cancel != nil {
		i.cancelCoordinator()

		i.mu.Lock()
		handoff, persistHandoff := i.resetCompactContinuationLocked()
		i.mu.Unlock()
		cancel()
		i.confirmDialog.CancelAll("turn cancelled")
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
	}
}

// Insert places text at the cursor in the editor.
func (i *Interactive) Insert(text string) {
	i.ed.Insert(text)
	i.invalidate()
}

// Display appends a styled note from extName to the chat without a
// model call.
func (i *Interactive) Display(extName, text string) {
	i.appendExtensionNote(extName, text, "info")
	i.invalidate()
}

// ReportError surfaces a host-side error in the interactive status area.
func (i *Interactive) ReportError(err error) {
	if err == nil {
		return
	}
	i.mu.Lock()
	i.statusOK = ""
	i.statusErr = err.Error()
	i.mu.Unlock()
	i.invalidate()
}

// SetStatus replaces one persistent status item owned by an extension.
func (i *Interactive) SetStatus(extName, key, level, text string) {
	if strings.TrimSpace(extName) == "" || strings.TrimSpace(key) == "" {
		return
	}
	i.mu.Lock()
	if i.extStatuses == nil {
		i.extStatuses = map[string]map[string]extensionStatus{}
	}
	if strings.TrimSpace(text) == "" {
		if items := i.extStatuses[extName]; items != nil {
			delete(items, key)
			if len(items) == 0 {
				delete(i.extStatuses, extName)
			}
		}
	} else {
		if i.extStatuses[extName] == nil {
			i.extStatuses[extName] = map[string]extensionStatus{}
		}
		i.extStatuses[extName][key] = extensionStatus{Level: level, Text: text}
	}
	i.mu.Unlock()
	i.invalidate()
}

// SetWidget replaces one persistent widget owned by an extension.
func (i *Interactive) SetWidget(extName, id, position, title string, lines []string) {
	if strings.TrimSpace(extName) == "" || strings.TrimSpace(id) == "" {
		return
	}
	position = extproto.NormalizeWidgetPosition(position)
	i.mu.Lock()
	if i.extWidgets == nil {
		i.extWidgets = map[string]map[string]extensionWidget{}
	}
	if i.extWidgets[extName] == nil {
		i.extWidgets[extName] = map[string]extensionWidget{}
	}
	i.extWidgets[extName][id] = extensionWidget{
		Position: position,
		Title:    title,
		Lines:    append([]string(nil), lines...),
	}
	i.mu.Unlock()
	i.invalidate()
}

// ClearWidget removes one persistent widget owned by an extension.
func (i *Interactive) ClearWidget(extName, id string) {
	i.mu.Lock()
	if items := i.extWidgets[extName]; items != nil {
		delete(items, id)
		if len(items) == 0 {
			delete(i.extWidgets, extName)
		}
	}
	i.mu.Unlock()
	i.invalidate()
}

// ClearExtensionChrome removes every persistent UI item owned by an
// extension that has exited. This keeps crashes, disable/reload operations,
// and startup failures from leaving stale widgets beside the transcript.
func (i *Interactive) ClearExtensionChrome(extName string) {
	if strings.TrimSpace(extName) == "" {
		return
	}
	marker := "[" + extName + "] "
	i.mu.Lock()
	changed := false
	if _, ok := i.extStatuses[extName]; ok {
		delete(i.extStatuses, extName)
		changed = true
	}
	if _, ok := i.extWidgets[extName]; ok {
		delete(i.extWidgets, extName)
		changed = true
	}
	if len(i.extNotes) > 0 {
		kept := i.extNotes[:0:0]
		for _, line := range i.extNotes {
			if strings.Contains(line, marker) {
				changed = true
				continue
			}
			kept = append(kept, line)
		}
		i.extNotes = kept
	}
	if i.extPanel != nil && i.extPanel.Active() && i.extPanel.ext == extName {
		i.extPanel.Close()
		changed = true
	}
	i.mu.Unlock()
	if changed {
		i.invalidate()
	}
}

func (i *Interactive) OpenPanel(extName string, spec extproto.PanelSpec) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.extPanel.Open(extName, spec.ID, spec.Title, spec.Lines, spec.Footer)
	i.confirmDialog.Blur()
	if i.cfg.Extensions != nil {
		cols, rows := i.cfg.Terminal.Size()
		_ = cols
		_ = rows
	}
	i.invalidate()
}

func (i *Interactive) UpdatePanel(extName, panelID, title string, lines []string, footer string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.extPanel.Active() && i.extPanel.ext == extName && i.extPanel.id == panelID {
		i.extPanel.Update(title, lines, footer)
		i.invalidate()
	}
}

func (i *Interactive) ClosePanel(extName, panelID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.extPanel.Active() && i.extPanel.ext == extName && i.extPanel.id == panelID {
		i.extPanel.Close()
		i.invalidate()
	}
}

func effectiveImageProtocol(override *bool) tui.ImageProtocol {
	detected := tui.DetectImageProtocol()
	if override == nil {
		return detected
	}
	if !*override {
		return tui.ImageProtocolNone
	}
	return detected
}

func imageProtocolName(p tui.ImageProtocol) string {
	switch p {
	case tui.ImageProtocolKitty:
		return "kitty/ghostty"
	case tui.ImageProtocolITerm2:
		return "iTerm2"
	default:
		return "none"
	}
}

func onOff(v bool) string {
	if v {
		return "enabled"
	}
	return "disabled"
}

const modalBackdropDimPercent = 50

func (i *Interactive) applyInputCursorColor() {
	if i == nil || i.cfg.Terminal == nil {
		return
	}
	color := tui.Color256(15)
	if i.cursorDimmed {
		color = i.cfg.Theme.DimColor(color, modalBackdropDimPercent)
	}
	_, _ = i.cfg.Terminal.Write([]byte(tui.CursorColor(color) + tui.CursorShapeBlock()))
}

// setInputCursorDimmed updates the terminal-owned cursor only when its layer
// changes. Reapplying cursor controls on every redraw can reset its blink
// timing before the terminal completes a normal blink cycle.
func (i *Interactive) setInputCursorDimmed(dimmed bool) {
	if i == nil || i.cursorDimmed == dimmed {
		return
	}
	i.cursorDimmed = dimmed
	i.applyInputCursorColor()
}

func reasoningSettingOptions(levels []string) []settingsOption {
	descriptions := map[string]string{
		"":        "no reasoning",
		"minimum": "very brief (~1k tokens)",
		"low":     "light (~2k tokens)",
		"medium":  "moderate (~8k tokens)",
		"high":    "deep (~16k tokens)",
		"xhigh":   "extra-high effort",
		"max":     "unconstrained effort",
	}
	options := make([]settingsOption, 0, len(levels))
	for _, level := range levels {
		label := level
		if label == "" {
			label = "off"
		}
		options = append(options, settingsOption{value: level, label: label, desc: descriptions[level]})
	}
	return options
}

func (i *Interactive) reasoningSettingItem() settingsItem {
	levels := []string{"", "minimum", "low", "medium", "high", "xhigh", "max"}
	reasoning := provider.NormalizeReasoning(i.cfg.Reasoning)
	desc := "reasoning depth for reasoning-capable models"
	hint := ""
	if m, err := provider.FindModel(i.cfg.Provider, i.cfg.Model); err == nil {
		levels = provider.AvailableReasoningLevels(m)
		reasoning = provider.ClampReasoningForModel(m, reasoning)
		if !m.Reasoning {
			hint = "current model does not support reasoning"
			desc += "; current model does not support reasoning"
		} else if len(levels) == 1 {
			hint = "current model has no adjustable reasoning levels"
			desc += "; current model has no adjustable reasoning levels"
		} else {
			desc += "; only levels supported by the current model are shown"
		}
	}
	options := reasoningSettingOptions(levels)
	choice := 0
	for idx, opt := range options {
		if opt.value == reasoning {
			choice = idx
			break
		}
	}

	return settingsItem{
		key:     "reasoning",
		label:   "reasoning level",
		desc:    desc,
		options: options,
		choice:  choice,
		hint:    hint,
	}
}

func (i *Interactive) openReasoningDialog() {
	i.settingsDialog.OpenDirectOption(i.reasoningSettingItem())
}

func (i *Interactive) openSettingsDialog() {
	detected := tui.DetectImageProtocol()
	imgEnabled := detected != tui.ImageProtocolNone
	if i.cfg.InlineImagesEnabled != nil {
		imgEnabled = *i.cfg.InlineImagesEnabled
	}
	imgDisabled := detected == tui.ImageProtocolNone
	imgHint := ""
	if imgDisabled {
		imgEnabled = false
		imgHint = "this terminal does not support inline images"
	} else {
		imgHint = "terminal supports " + imageProtocolName(detected)
	}

	terminalAlerts := terminalAlertsEnabled(i.cfg.TerminalAlertsEnabled)
	terminalTitles := terminalTitleEnabled(i.cfg.TerminalTitleEnabled)
	autoSubagents := false
	if i.cfg.AutoSubagentsEnabled != nil {
		autoSubagents = *i.cfg.AutoSubagentsEnabled
	}
	autoSubagentsDisabled := !i.autoSubagentsAvailable()
	autoSubagentsHint := i.autoSubagentsUnavailableHint()
	if autoSubagentsDisabled {
		autoSubagents = false
	}

	ponytailEnabled := i.ponytailEnabled()
	ponytailDisabled := !i.ponytailSettingsAvailable()
	ponytailHint := ""
	if ponytailDisabled {
		ponytailHint = "requires persistent settings and live prompt refresh support"
	}

	webSearchEnabled := i.webSearchEffectiveEnabled()
	webSearchDisabled := !i.webSearchSettingsAvailable()
	webSearchHint := i.webSearchUnavailableHint()

	fastMode := i.cfg.FastMode != nil && *i.cfg.FastMode
	fastModeHint := "OpenAI service tier"
	if !provider.SupportsFastMode(i.cfg.Provider) {
		fastModeHint = "only supported for OpenAI providers"
	}
	lspEnabled := i.cfg.LSPEnabled == nil || *i.cfg.LSPEnabled
	subagentLSPEnabled := i.cfg.SubagentLSPEnabled == nil || *i.cfg.SubagentLSPEnabled

	jailByDefault := i.cfg.JailByDefault != nil && *i.cfg.JailByDefault
	recursiveFiles := i.cfg.RecursiveFileSuggest != nil && *i.cfg.RecursiveFileSuggest
	respectGitignore := i.cfg.RespectGitignore == nil || *i.cfg.RespectGitignore
	compactMode := i.compactModeEnabled()
	showInstructions := i.cfg.ShowInstructionsAtStartup != nil && *i.cfg.ShowInstructionsAtStartup
	inputStyle := tui.NormalizeInputStyle(i.cfg.TUIInputStyle)
	statusPosition := tui.NormalizeStatusPosition(i.cfg.TUIStatusPosition)
	workingPosition := tui.NormalizeWorkingPosition(i.cfg.TUIWorkingPosition)
	subagentPosition := tui.NormalizeSubagentPosition(i.cfg.TUISubagentPosition)
	quickItems := i.quickModelSettingItems()

	autoCompactOptions := []settingsOption{
		{value: "0", label: "off", desc: "disable context-percentage triggers; payload-too-large recovery stays enabled"},
		{value: "70", label: "70%", desc: "compact earlier to keep more context headroom"},
		{value: "80", label: "80%", desc: "compact with moderate context headroom"},
		{value: "85", label: "85%", desc: "default balance between history and headroom"},
		{value: "90", label: "90%", desc: "retain more history before compacting"},
		{value: "95", label: "95%", desc: "retain most history before compacting"},
	}
	autoCompactThreshold := NormalizeAutoCompactThreshold(i.cfg.AutoCompactThreshold)
	autoCompactChoice := 0
	for idx, opt := range autoCompactOptions {
		if opt.value == strconv.Itoa(autoCompactThreshold) {
			autoCompactChoice = idx
			break
		}
	}

	reasoningItem := i.reasoningSettingItem()

	themeName := i.cfg.ThemeName
	if themeName == "" {
		themeName = "auto"
	}
	if themeName != "auto" && !tui.ThemeExists(i.cfg.ZutHome, themeName) {
		themeName = "auto"
		i.cfg.ThemeName = ""
		if i.cfg.SettingsStore != nil {
			_ = i.cfg.SettingsStore.SetTheme("auto")
		}
		i.applyThemeNow("auto")
	}
	themeOptions := []settingsOption{}
	themeChoice := 0
	availableThemes := tui.AvailableThemes(i.cfg.ZutHome)
	if i.cfg.ExtensionThemes != nil {
		availableThemes = append(availableThemes, i.cfg.ExtensionThemes()...)
	}
	for idx, opt := range availableThemes {
		themeOptions = append(themeOptions, settingsOption{value: opt.Value, label: opt.Label, desc: opt.Description})
		if opt.Value == themeName {
			themeChoice = idx
		}
	}

	inputStyleOptions := []settingsOption{
		{value: tui.InputStylePlain, label: "plain", desc: "render the input as the normal prompt line"},
		{value: tui.InputStyleLines, label: "lines", desc: "draw separator lines above and below the input"},
		{value: tui.InputStyleBlock, label: "block", desc: "render the input as a user-bubble-style block"},
	}
	inputStyleChoice := 0
	for idx, opt := range inputStyleOptions {
		if opt.value == inputStyle {
			inputStyleChoice = idx
			break
		}
	}
	statusPositionOptions := []settingsOption{
		{value: tui.StatusPositionAboveInput, label: "above input", desc: "show model, usage, and working directory above the input"},
		{value: tui.StatusPositionBelowInput, label: "below input", desc: "show model, usage, and working directory below the input"},
	}
	statusPositionChoice := 0
	for idx, opt := range statusPositionOptions {
		if opt.value == statusPosition {
			statusPositionChoice = idx
			break
		}
	}

	workingPositionOptions := []settingsOption{
		{value: tui.WorkingPositionAboveInput, label: "above input", desc: "show the working spinner above the input"},
		{value: tui.WorkingPositionBelowInput, label: "below input", desc: "show the working spinner below the input"},
	}
	workingPositionChoice := 0
	for idx, opt := range workingPositionOptions {
		if opt.value == workingPosition {
			workingPositionChoice = idx
			break
		}
	}

	subagentPositionOptions := []settingsOption{
		{value: tui.SubagentPositionAboveInput, label: "above input", desc: "show running subagents above the input"},
		{value: tui.SubagentPositionBelowInput, label: "below input", desc: "show running subagents immediately below the input"},
	}
	subagentPositionChoice := 0
	for idx, opt := range subagentPositionOptions {
		if opt.value == subagentPosition {
			subagentPositionChoice = idx
			break
		}
	}

	items := []settingsItem{
		{
			key:      "inline_images_enabled",
			label:    "render images when supported",
			desc:     "draw screenshots inline instead of showing a text placeholder",
			value:    imgEnabled,
			disabled: imgDisabled,
			hint:     imgHint,
		},
		{
			key:   "terminal_alerts_enabled",
			label: "terminal alerts",
			desc:  "ring the terminal when the main session needs attention or an extension raises an alert",
			value: terminalAlerts,
		},
		{
			key:   "terminal_title_enabled",
			label: "AI terminal titles",
			desc:  "ask the active model for a concise title after the first prompt; uses one extra hidden request",
			value: terminalTitles,
		},
		{
			key:      "auto_subagents_enabled",
			label:    "Subagent Orchestrator",
			desc:     "automatically delegate coding work to sub-agents; tools stay available when disabled",
			value:    autoSubagents,
			disabled: autoSubagentsDisabled,
			hint:     autoSubagentsHint,
		},
		{
			key:      "ponytail_enabled",
			label:    "ponytail coding mode",
			desc:     "apply compact coding guidance that favors small, validated, maintainable changes",
			value:    ponytailEnabled,
			disabled: ponytailDisabled,
			hint:     ponytailHint,
		},
		{
			key:      "web_search_enabled",
			label:    "web search",
			desc:     "search DuckDuckGo for bounded public-web sources; sends queries to the public backend",
			value:    webSearchEnabled,
			disabled: webSearchDisabled,
			hint:     webSearchHint,
		},
		{
			key:   "fast_mode",
			label: "fast mode",
			desc:  "request the provider's fast tier where supported; unsupported providers return an error",
			value: fastMode,
			hint:  fastModeHint,
		},
		{
			key:   "lsp_enabled",
			label: "lsp in main session",
			desc:  "enable the lsp tool and code diagnostics for the main agent",
			value: lspEnabled,
		},
		{
			key:   "subagent_lsp_enabled",
			label: "lsp in sub-agents",
			desc:  "allow newly spawned background sub-agents to use the lsp tool",
			value: subagentLSPEnabled,
		},
		{
			key:     "auto_compact_threshold",
			label:   "auto-compact threshold",
			desc:    "choose how full the model context can get before zut condenses conversation history",
			options: autoCompactOptions,
			choice:  autoCompactChoice,
		},
		{
			key:   "jail_by_default",
			label: "jail new sessions by default",
			desc:  "confine tools to the session working directory unless /unjail is used",
			value: jailByDefault,
		},
		{
			key:   "recursive_file_suggest",
			label: "recursive @-file search",
			desc:  "fuzzy-search the whole project tree when picking files with @ instead of browsing one directory at a time",
			value: recursiveFiles,
		},
		{
			key:   "respect_gitignore",
			label: "hide gitignored files in @-picker",
			desc:  "skip files and directories matched by the project's root .gitignore (and .git) when picking files with @",
			value: respectGitignore,
		},
		{
			key:   "compact_mode",
			label: "compact transcript rendering",
			desc:  "reduce visual chrome by rendering tool calls without boxes and sent messages without padded bubbles",
			value: compactMode,
		},
		{
			key:   "show_instructions_at_startup",
			label: "show loaded resources at startup",
			desc:  "list loaded context files, extensions, and user-installed skills above the transcript",
			value: showInstructions,
		},
		{
			key:   "tui_settings",
			label: "tui settings",
			desc:  "choose input chrome in its order around the prompt",
			children: []settingsItem{
				{
					key:     "tui_status_position",
					label:   "status position",
					desc:    "place model, usage, and working directory above or below the input",
					options: statusPositionOptions,
					choice:  statusPositionChoice,
				},
				{
					key:     "tui_working_position",
					label:   "working spinner position",
					desc:    "place the working spinner above or below the input",
					options: workingPositionOptions,
					choice:  workingPositionChoice,
				},
				{
					key:     "tui_input_style",
					label:   "input style",
					desc:    "choose between the plain prompt, lines, and a block input area",
					options: inputStyleOptions,
					choice:  inputStyleChoice,
				},
				{
					key:     "tui_subagent_position",
					label:   "running subagent position",
					desc:    "place live subagent activity above or below the input",
					options: subagentPositionOptions,
					choice:  subagentPositionChoice,
				},
			},
		},
		reasoningItem,
		{
			key:     "theme",
			label:   "color theme",
			desc:    "choose auto, inherited terminal colors, a built-in dark/light theme, or a loaded theme file",
			options: themeOptions,
			choice:  themeChoice,
		},
	}
	if len(quickItems) > 0 {
		items = append(items, settingsItem{
			key:      "quick_models",
			label:    "model shortcuts",
			desc:     "configure " + quickModelShortcutPrefix() + "+1 through " + quickModelShortcutPrefix() + "+9 quick model switches",
			children: quickItems,
		})
	}
	i.settingsDialog.Open(items)
}

func (i *Interactive) applySettingChange(act settingsAction) {
	switch {
	case strings.HasPrefix(act.Key, "quick_model_"):
		i.applyQuickModelSetting(act.Key, act.StringValue)
	case act.Key == "reasoning":
		i.applyReasoningSetting(act.StringValue)
	case act.Key == "theme":
		i.applyThemeSetting(act.StringValue)
	case act.Key == "auto_compact_threshold":
		i.applyAutoCompactThresholdSetting(act.StringValue)
	case act.Key == "tui_input_style":
		i.applyTUIInputStyleSetting(act.StringValue)
	case act.Key == "tui_status_position":
		i.applyTUIStatusPositionSetting(act.StringValue)
	case act.Key == "tui_working_position":
		i.applyTUIWorkingPositionSetting(act.StringValue)
	case act.Key == "tui_subagent_position":
		i.applyTUISubagentPositionSetting(act.StringValue)
	default:
		i.applySettingToggle(act.Key, act.Value)
	}
}

func (i *Interactive) applyAutoCompactThresholdSetting(value string) {
	threshold, err := strconv.Atoi(value)
	if err != nil {
		return
	}
	threshold = NormalizeAutoCompactThreshold(&threshold)
	if store, ok := i.cfg.SettingsStore.(autoCompactThresholdSettingsStore); ok {
		if err := store.SetAutoCompactThreshold(threshold); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.mu.Lock()
	i.cfg.AutoCompactThreshold = &threshold
	if threshold == 0 {
		i.statusOK = "auto-compact threshold off"
	} else {
		i.statusOK = fmt.Sprintf("auto-compact threshold %d%%", threshold)
	}
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

func (i *Interactive) quickModelSettingItems() []settingsItem {
	if len(i.cfg.QuickModelShortcuts) < 9 {
		next := make([]QuickModelShortcut, 9)
		copy(next, i.cfg.QuickModelShortcuts)
		i.cfg.QuickModelShortcuts = next
	}
	items := make([]settingsItem, 0, 9)
	for slot := 1; slot <= 9; slot++ {
		items = append(items, i.quickModelSettingItem(slot))
	}
	return items
}

func (i *Interactive) quickModelSettingItem(slot int) settingsItem {
	current := QuickModelShortcut{}
	if slot >= 1 && len(i.cfg.QuickModelShortcuts) >= slot {
		current = i.cfg.QuickModelShortcuts[slot-1]
	}
	hint := "not assigned"
	if current.Provider != "" && current.Model != "" {
		hint = current.Provider + " / " + current.Model
	}
	return settingsItem{
		key:    "quick_model_" + strconv.Itoa(slot),
		label:  "model " + strconv.Itoa(slot),
		desc:   quickModelShortcutLabel(slot) + " switches to this model. Enter opens the /model selector, Backspace clears.",
		picker: true,
		hint:   hint,
	}
}

func (i *Interactive) compactModeEnabled() bool {
	return i.cfg.CompactMode != nil && *i.cfg.CompactMode
}

func quickModelShortcutSlot(k tui.Key) int {
	if k.Kind != tui.KeyRune || k.Rune < '1' || k.Rune > '9' {
		return 0
	}
	if runtime.GOOS == "darwin" {
		if !k.Super && !k.Ctrl {
			return 0
		}
	} else if !k.Ctrl {
		return 0
	}
	return int(k.Rune - '0')
}

func quickModelShortcutPrefix() string {
	return "Ctrl"
}

func quickModelShortcutLabel(slot int) string {
	return quickModelShortcutPrefix() + "+" + strconv.Itoa(slot)
}

func (i *Interactive) openQuickModelPicker(slot int) {
	if slot < 1 || slot > 9 {
		return
	}
	i.quickModelAssign = slot
	current := i.cfg.Model
	if len(i.cfg.QuickModelShortcuts) >= slot && i.cfg.QuickModelShortcuts[slot-1].Model != "" {
		current = i.cfg.QuickModelShortcuts[slot-1].Model
	}
	var loggedIn []string
	if i.cfg.LoggedInProviders != nil {
		loggedIn = i.cfg.LoggedInProviders()
	}
	i.modelDialog.Open(current, loggedIn, i.cfg.Reasoning)
}

func (i *Interactive) applyQuickModelSelection(slot int, providerName, model string) {
	i.setQuickModelShortcut(slot, providerName, model)
}

func (i *Interactive) applyQuickModelShortcut(slot int) {
	if slot < 1 || slot > 9 {
		return
	}
	if i.busy {
		i.mu.Lock()
		i.statusErr = "cannot switch model while a turn is running"
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if len(i.cfg.QuickModelShortcuts) < slot {
		i.mu.Lock()
		i.statusErr = quickModelShortcutLabel(slot) + " is not assigned"
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	shortcut := i.cfg.QuickModelShortcuts[slot-1]
	if shortcut.Provider == "" || shortcut.Model == "" {
		i.mu.Lock()
		i.statusErr = quickModelShortcutLabel(slot) + " is not assigned"
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.swapModel(shortcut.Provider, shortcut.Model, i.cfg.BuildAgentFor, false)
	i.invalidate()
}

func (i *Interactive) applyQuickModelSetting(key, value string) {
	slotText := strings.TrimPrefix(key, "quick_model_")
	slot, err := strconv.Atoi(slotText)
	if err != nil || slot < 1 || slot > 9 {
		return
	}
	providerName, model := "", ""
	if value != "" {
		parts := strings.SplitN(value, "\t", 2)
		if len(parts) == 2 {
			providerName, model = parts[0], parts[1]
		}
	}
	i.setQuickModelShortcut(slot, providerName, model)
}

func (i *Interactive) setQuickModelShortcut(slot int, providerName, model string) {
	if len(i.cfg.QuickModelShortcuts) < slot {
		next := make([]QuickModelShortcut, slot)
		copy(next, i.cfg.QuickModelShortcuts)
		i.cfg.QuickModelShortcuts = next
	}
	i.cfg.QuickModelShortcuts[slot-1] = QuickModelShortcut{Provider: providerName, Model: model}
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetQuickModelShortcut(slot, providerName, model); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.mu.Lock()
	if model == "" {
		i.statusOK = quickModelShortcutLabel(slot) + " cleared"
	} else {
		i.statusOK = quickModelShortcutLabel(slot) + " set to " + providerName + " / " + model
	}
	i.statusErr = ""
	i.mu.Unlock()
	i.refreshQuickModelSettingsItem(slot)
	i.invalidate()
}

func (i *Interactive) refreshQuickModelSettingsItem(slot int) {
	if i.settingsDialog == nil || !i.settingsDialog.Active() || len(i.settingsDialog.items) == 0 {
		return
	}
	key := "quick_model_" + strconv.Itoa(slot)
	for idx, it := range i.settingsDialog.items {
		if it.key == key {
			i.settingsDialog.items[idx] = i.quickModelSettingItem(slot)
			return
		}
	}
}

func (i *Interactive) resetSettingsToggle(key string, value bool) {
	if i.settingsDialog == nil {
		return
	}
	for idx := range i.settingsDialog.items {
		if i.settingsDialog.items[idx].key == key {
			i.settingsDialog.items[idx].value = value
			return
		}
	}
}

func (i *Interactive) ponytailEnabled() bool {
	return i.cfg.PonytailEnabled == nil || *i.cfg.PonytailEnabled
}

func (i *Interactive) ponytailSettingsAvailable() bool {
	_, persistent := i.cfg.SettingsStore.(ponytailSettingsStore)
	return persistent && i.cfg.RefreshPrompt != nil
}

func (i *Interactive) webSearchEnabled() bool {
	return i.cfg.WebSearchEnabled == nil || *i.cfg.WebSearchEnabled
}

func (i *Interactive) webSearchToolAllowed() bool {
	return i.cfg.WebSearchToolAllowed == nil || *i.cfg.WebSearchToolAllowed
}

func (i *Interactive) webSearchEffectiveEnabled() bool {
	return i.webSearchToolAllowed() && (i.cfg.WebSearchInvocationOverride || i.webSearchEnabled())
}

func (i *Interactive) webSearchSettingsAvailable() bool {
	_, persistent := i.cfg.SettingsStore.(webSearchSettingsStore)
	return i.webSearchToolAllowed() && !i.cfg.WebSearchInvocationOverride && persistent && i.cfg.RefreshTools != nil
}

func (i *Interactive) webSearchUnavailableHint() string {
	if !i.webSearchToolAllowed() {
		return "unavailable in this session due to the launch-time tool capability ceiling"
	}
	if i.cfg.WebSearchInvocationOverride {
		return "enabled for this session by explicit --tools web_search; the persisted setting applies to invocations without --tools"
	}
	if !i.webSearchSettingsAvailable() {
		return "requires persistent settings and live tool refresh support"
	}
	return ""
}

func (i *Interactive) setWebSearchAvailable(available bool) {
	// Advance before publishing the new gate/child ceiling. A resolver that
	// captured the prior generation can no longer commit after this transition.
	// Callers may hold agentMu and i.mu; the callback must complete without
	// calling back into Interactive or requiring either lock.
	i.webSearchPolicyGeneration.Add(1)
	if i.cfg.SetWebSearchAvailable != nil {
		i.cfg.SetWebSearchAvailable(available)
	}
}

func (i *Interactive) applyWebSearchSettingToggle(key string, value bool) {
	previous := i.webSearchEnabled()
	store, persistent := i.cfg.SettingsStore.(webSearchSettingsStore)
	if !i.webSearchSettingsAvailable() || !persistent {
		i.resetSettingsToggle(key, i.webSearchEffectiveEnabled())
		i.mu.Lock()
		i.statusOK = ""
		i.statusErr = "web search unavailable: " + i.webSearchUnavailableHint()
		i.mu.Unlock()
		return
	}
	if err := store.SetWebSearchEnabled(value); err != nil {
		i.resetSettingsToggle(key, previous)
		i.mu.Lock()
		i.statusOK = ""
		i.statusErr = "settings: " + err.Error()
		i.mu.Unlock()
		return
	}
	val := value
	i.cfg.WebSearchEnabled = &val
	// Revoke and advance the policy generation for both directions. Enabling
	// remains fail-closed until the freshly resolved registry commits, while a
	// stale resolve from before this persisted transition is rejected.
	i.stripWebSearchTool()
	if err := i.cfg.RefreshTools(); err != nil {
		// Refresh failure leaves the old registry's state unknown. Revoke both
		// live execution and generic-child inheritance before rollback.
		i.stripWebSearchTool()
		errMsg := "settings: web search tool refresh: " + err.Error()
		if rollbackErr := store.SetWebSearchEnabled(previous); rollbackErr != nil {
			failClosed := false
			i.cfg.WebSearchEnabled = &failClosed
			i.resetSettingsToggle(key, false)
			errMsg += "; rollback persistence: " + rollbackErr.Error()
		} else {
			previousVal := previous
			i.cfg.WebSearchEnabled = &previousVal
			i.resetSettingsToggle(key, previous)
			if refreshErr := i.cfg.RefreshTools(); refreshErr != nil {
				errMsg += "; rollback refresh: " + refreshErr.Error()
				i.stripWebSearchTool()
			}
		}
		i.mu.Lock()
		i.statusOK = ""
		i.statusErr = errMsg
		i.mu.Unlock()
		return
	}
	i.mu.Lock()
	i.statusOK = "web search " + onOff(value)
	i.statusErr = ""
	i.mu.Unlock()
}

// stripWebSearchTool removes the callable web_search entry from the live
// registry without asking the resolver to run again. It is the fail-closed
// fallback when persistence rollback fails after a refresh failure.
func (i *Interactive) stripWebSearchTool() {
	// Revoke first so a stale tool snapshotted by core cannot execute while
	// the advertised registry is being replaced.
	i.setWebSearchAvailable(false)
	i.agentMu.Lock()
	defer i.agentMu.Unlock()
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.agent == nil {
		return
	}
	tools := i.agent.ToolsSnapshot()
	if _, ok := tools["web_search"]; !ok {
		return
	}
	delete(tools, "web_search")
	i.agent.SetTools(tools)
}

func (i *Interactive) applySettingToggle(key string, value bool) {
	// Every setting toggle forces a full repaint at the end — same
	// effect as the user pressing Ctrl+L — so any per-setting visual
	// change (image rendering, status copy, future toggles) lands
	// immediately instead of waiting for the next diff frame.
	defer func() {
		i.requestRendererClear()
		i.invalidate()
	}()
	switch key {
	case "inline_images_enabled":
		val := value
		i.cfg.InlineImagesEnabled = &val
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetInlineImages(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		i.mu.Lock()
		i.view.ImageProto = effectiveImageProtocol(i.cfg.InlineImagesEnabled)
		i.view.InvalidateRenderCache()
		i.statusOK = "inline image rendering " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "terminal_alerts_enabled":
		val := value
		i.mu.Lock()
		i.cfg.TerminalAlertsEnabled = &val
		i.mu.Unlock()
		if store, ok := i.cfg.SettingsStore.(terminalAlertsSettingsStore); ok {
			if err := store.SetTerminalAlertsEnabled(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		i.mu.Lock()
		i.statusOK = "terminal alerts " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "terminal_title_enabled":
		val := value
		var cancel context.CancelFunc
		i.mu.Lock()
		i.cfg.TerminalTitleEnabled = &val
		if value {
			i.writeTerminalTitleLocked(i.sessionTitle)
		} else {
			cancel = i.titleCancel
			i.titleCancel = nil
			i.titleVersion++
			i.writeTerminalTitleLocked("")
		}
		i.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if store, ok := i.cfg.SettingsStore.(terminalTitleSettingsStore); ok {
			if err := store.SetTerminalTitleEnabled(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		i.mu.Lock()
		i.statusOK = "AI terminal titles " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "web_search_enabled":
		apply := func() { i.applyWebSearchSettingToggle(key, value) }
		if i.cfg.SessionTransition != nil {
			i.cfg.SessionTransition(apply)
		} else {
			apply()
		}
	case "ponytail_enabled":
		previous := i.ponytailEnabled()
		store, available := i.cfg.SettingsStore.(ponytailSettingsStore)
		if !available || i.cfg.RefreshPrompt == nil {
			i.resetSettingsToggle(key, previous)
			i.mu.Lock()
			i.statusOK = ""
			i.statusErr = "ponytail coding mode unavailable: persistent settings and live prompt refresh are required"
			i.mu.Unlock()
			return
		}
		if err := store.SetPonytailEnabled(value); err != nil {
			i.resetSettingsToggle(key, previous)
			i.mu.Lock()
			i.statusOK = ""
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
		val := value
		i.cfg.PonytailEnabled = &val
		if err := i.cfg.RefreshPrompt(); err != nil {
			errMsg := "settings: ponytail prompt refresh: " + err.Error()
			if rollbackErr := store.SetPonytailEnabled(previous); rollbackErr != nil {
				// The first write succeeded, so keep the in-memory setting at
				// the value that remains durable instead of claiming that the
				// rollback took effect.
				i.resetSettingsToggle(key, value)
				errMsg += "; rollback persistence: " + rollbackErr.Error()
				if reconcileErr := i.cfg.RefreshPrompt(); reconcileErr != nil {
					errMsg += "; durable-state refresh: " + reconcileErr.Error()
				}
			} else {
				previousVal := previous
				i.cfg.PonytailEnabled = &previousVal
				i.resetSettingsToggle(key, previous)
				if refreshErr := i.cfg.RefreshPrompt(); refreshErr != nil {
					errMsg += "; rollback refresh: " + refreshErr.Error()
				}
			}
			i.mu.Lock()
			i.statusOK = ""
			i.statusErr = errMsg
			i.mu.Unlock()
			return
		}
		i.mu.Lock()
		i.statusOK = "ponytail coding mode " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "auto_subagents_enabled":
		if value && !i.autoSubagentsAvailable() {
			i.mu.Lock()
			i.statusOK = ""
			i.statusErr = "subagent orchestrator unavailable: " + i.autoSubagentsUnavailableHint()
			i.mu.Unlock()
			return
		}
		i.mu.Lock()
		previousFlag := i.cfg.AutoSubagentsEnabled
		previous := previousFlag != nil && *previousFlag
		val := value
		i.cfg.AutoSubagentsEnabled = &val
		i.mu.Unlock()
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetAutoSubagents(value); err != nil {
				i.mu.Lock()
				i.cfg.AutoSubagentsEnabled = previousFlag
				i.statusOK = ""
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				i.resetSettingsToggle(key, previous)
				return
			}
		}
		// The subagent tools remain available for explicit user requests.
		// Swap only the system-prompt guidance between strict orchestration and
		// on-demand delegation.
		i.applyAutoSubagentsTool()
		// Also swap the system-prompt addendum so the model knows whether it
		// should orchestrate proactively or delegate only on user request.
		i.applyAutoSubagentsSystemPrompt(value)
		i.mu.Lock()
		i.statusOK = "subagent orchestrator " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "fast_mode":
		previous := i.cfg.FastMode != nil && *i.cfg.FastMode
		if err := provider.ValidateFastMode(i.cfg.Provider, value); err != nil {
			i.resetSettingsToggle(key, previous)
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
		if store, ok := i.cfg.SettingsStore.(fastModeSettingsStore); ok {
			if err := store.SetFastMode(value); err != nil {
				i.resetSettingsToggle(key, previous)
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		val := value
		i.cfg.FastMode = &val
		if i.agent != nil {
			i.agent.SetFastMode(value)
		}
		if i.cfg.Supervisor != nil {
			i.cfg.Supervisor.SetFastMode(value)
		}
		i.mu.Lock()
		i.statusOK = "fast mode " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "lsp_enabled":
		previous := i.cfg.LSPEnabled == nil || *i.cfg.LSPEnabled
		store, hasStore := i.cfg.SettingsStore.(lspSettingsStore)
		if hasStore {
			if err := store.SetLSPEnabled(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		val := value
		i.cfg.LSPEnabled = &val
		if i.cfg.RefreshTools != nil {
			if err := i.cfg.RefreshTools(); err != nil {
				errMsg := "settings: refresh tools: " + err.Error()
				rollbackErr := error(nil)
				if hasStore {
					rollbackErr = store.SetLSPEnabled(previous)
				}
				if rollbackErr != nil {
					// The first write succeeded, so keep the in-memory setting at
					// the value that remains durable instead of claiming that the
					// rollback took effect.
					errMsg += "; rollback persistence: " + rollbackErr.Error()
					if reconcileErr := i.cfg.RefreshTools(); reconcileErr != nil {
						errMsg += "; durable-state refresh: " + reconcileErr.Error()
					}
				} else {
					previousVal := previous
					i.cfg.LSPEnabled = &previousVal
					if refreshErr := i.cfg.RefreshTools(); refreshErr != nil {
						errMsg += "; rollback refresh: " + refreshErr.Error()
					}
				}
				i.mu.Lock()
				i.statusOK = ""
				i.statusErr = errMsg
				i.mu.Unlock()
				return
			}
		}
		i.mu.Lock()
		i.statusOK = "lsp in main session " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "subagent_lsp_enabled":
		if store, ok := i.cfg.SettingsStore.(lspSettingsStore); ok {
			if err := store.SetSubagentLSPEnabled(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		val := value
		i.cfg.SubagentLSPEnabled = &val
		i.mu.Lock()
		i.statusOK = "lsp in sub-agents " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "jail_by_default":
		val := value
		i.cfg.JailByDefault = &val
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetJailByDefault(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		if i.cfg.Sandbox != nil {
			if value {
				i.cfg.Sandbox.Lock()
			} else {
				i.cfg.Sandbox.Unlock()
			}
		}
		i.mu.Lock()
		i.statusOK = "jail by default " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "recursive_file_suggest":
		val := value
		i.cfg.RecursiveFileSuggest = &val
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetRecursiveFileSuggest(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		// Flip the live picker so the next @ reflects the new mode
		// without restarting zut. SetRecursive drops its cache.
		i.fileSuggest.SetRecursive(value)
		i.mu.Lock()
		i.statusOK = "recursive @-file search " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "respect_gitignore":
		val := value
		i.cfg.RespectGitignore = &val
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetRespectGitignore(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		i.fileSuggest.SetRespectGitignore(value)
		i.mu.Lock()
		i.statusOK = "hide gitignored files in @-picker " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "compact_mode":
		val := value
		i.cfg.CompactMode = &val
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetCompactMode(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		i.mu.Lock()
		i.view.CompactMode = value
		i.view.InvalidateRenderCache()
		i.statusOK = "compact transcript rendering " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "show_instructions_at_startup":
		val := value
		i.cfg.ShowInstructionsAtStartup = &val
		if store, ok := i.cfg.SettingsStore.(showInstructionsSettingsStore); ok {
			if err := store.SetShowInstructionsAtStartup(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		i.mu.Lock()
		i.view.StartupAgentName = ""
		i.view.StartupContextPaths = nil
		i.view.StartupExtensionNames = nil
		i.view.StartupSkillNames = nil
		i.view.InvalidateRenderCache()
		if value {
			i.view.StartupAgentName = i.cfg.StartupAgentName
			i.view.StartupContextPaths = append(i.view.StartupContextPaths, i.cfg.StartupContextPaths...)
			i.view.StartupExtensionNames = append(i.view.StartupExtensionNames, i.cfg.StartupExtensionNames...)
			i.view.StartupSkillNames = append(i.view.StartupSkillNames, i.cfg.StartupSkillNames...)
		}
		i.statusOK = "show loaded resources at startup " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	}
}

func (i *Interactive) applyTUIInputStyleSetting(style string) {
	defer func() {
		i.requestRendererClear()
		i.invalidate()
	}()
	style = tui.NormalizeInputStyle(style)
	i.cfg.TUIInputStyle = style
	i.applyInputCursorColor()
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetTUIInputStyle(style); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.mu.Lock()
	i.statusOK = "input style " + style
	i.statusErr = ""
	i.mu.Unlock()
}

func (i *Interactive) applyTUIStatusPositionSetting(position string) {
	defer func() {
		i.requestRendererClear()
		i.invalidate()
	}()
	position = tui.NormalizeStatusPosition(position)
	i.cfg.TUIStatusPosition = position
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetTUIStatusPosition(position); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.mu.Lock()
	label := "above input"
	if position == tui.StatusPositionBelowInput {
		label = "below input"
	}
	i.statusOK = "status position " + label
	i.statusErr = ""
	i.mu.Unlock()
}

func (i *Interactive) applyTUIWorkingPositionSetting(position string) {
	defer func() {
		i.requestRendererClear()
		i.invalidate()
	}()
	position = tui.NormalizeWorkingPosition(position)
	i.cfg.TUIWorkingPosition = position
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetTUIWorkingPosition(position); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.mu.Lock()
	label := "above input"
	if position == tui.WorkingPositionBelowInput {
		label = "below input"
	}
	i.statusOK = "working spinner position " + label
	i.statusErr = ""
	i.mu.Unlock()
}

func (i *Interactive) applyTUISubagentPositionSetting(position string) {
	defer func() {
		i.requestRendererClear()
		i.invalidate()
	}()
	position = tui.NormalizeSubagentPosition(position)
	i.cfg.TUISubagentPosition = position
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetTUISubagentPosition(position); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.mu.Lock()
	label := "below input"
	if position == tui.SubagentPositionAboveInput {
		label = "above input"
	}
	i.statusOK = "running subagent position " + label
	i.statusErr = ""
	i.mu.Unlock()
}

func (i *Interactive) applyThemeSetting(name string) {
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetTheme(name); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.cfg.ThemeName = name
	if name == "auto" {
		i.cfg.ThemeName = ""
	}
	i.applyThemeNow(name)
}

func (i *Interactive) applyThemeNow(name string) {
	if name == "" {
		name = "auto"
	}
	detected := tui.Dark
	if tui.IsLightTheme(i.cfg.Theme) {
		detected = tui.Light
	}
	// Keep the startup terminal snapshot available when a user switches
	// themes live; inherited mode must not issue OSC queries while the TUI
	// already owns raw stdin.
	detected.Terminal = i.cfg.Theme.Terminal
	th, applied, err := tui.LoadThemeFromHome(i.cfg.ZutHome, name, detected)
	if err != nil {
		if i.cfg.SettingsStore != nil {
			_ = i.cfg.SettingsStore.SetTheme("auto")
		}
		i.cfg.ThemeName = ""
		th, _, _ = tui.LoadThemeFromHome(i.cfg.ZutHome, "auto", detected)
		i.mu.Lock()
		i.statusErr = "theme missing; reset to default"
		i.mu.Unlock()
	} else {
		i.mu.Lock()
		label := applied
		if label == "" {
			label = "auto"
		}
		i.statusOK = "theme " + label
		i.statusErr = ""
		i.mu.Unlock()
	}
	i.mu.Lock()
	i.cfg.Theme = th
	i.view.Theme = th
	i.view.InvalidateRenderCache()
	i.mu.Unlock()
	i.ed.Prompt = th.AccentBar(th.Accent)
	i.applyInputCursorColor()
	i.spin.Configure(th)
	i.requestRendererTheme(th)
	i.requestRendererClear()
	i.invalidate()
}

func (i *Interactive) applyReasoningSetting(level string) {
	defer func() {
		i.requestRendererClear()
		i.invalidate()
	}()
	level = provider.NormalizeReasoning(level)
	i.cfg.Reasoning = level
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetReasoning(level); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	if i.cfg.OnReasoningChanged != nil {
		i.cfg.OnReasoningChanged(level)
	}
	i.mu.Lock()
	if i.agent != nil {
		i.agent.Reasoning = level
	}
	label := level
	if label == "" {
		label = "off"
	}
	i.statusOK = "reasoning level " + label
	i.statusErr = ""
	i.mu.Unlock()
}

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
func buildStudyPrompt(arg, cwd string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "Read and understand everything in the current directory."
	}
	abs := arg
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, abs)
	}
	display := arg
	if rel, err := filepath.Rel(cwd, abs); err == nil && !strings.HasPrefix(rel, "..") {
		display = rel
	}
	if info, err := os.Stat(abs); err == nil && !info.IsDir() {
		return "Read and understand the file " + display + "."
	}
	return "Read and understand everything in the directory " + display + "."
}

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
// editor instances owned by btwDialog and subagentsDialog without each
// dialog needing its own copy.
func tryPathTabCompleteEditor(ed *tui.Editor, cwd string) bool {
	if ed == nil {
		return false
	}
	val := ed.Value()
	// Find the trailing run of non-whitespace.
	start := len(val)
	for start > 0 {
		r := val[start-1]
		if r == ' ' || r == '\t' || r == '\n' {
			break
		}
		start--
	}
	token := val[start:]
	if token == "" {
		return false
	}
	if !looksLikePathToken(token) {
		return false
	}

	// Resolve the absolute parent directory + base prefix to match.
	parentAbs, basePrefix, displayParent, ok := resolvePathTabToken(token, cwd)
	if !ok {
		return true
	}
	entries, err := os.ReadDir(parentAbs)
	if err != nil {
		return true
	}
	var names []string
	var isDir []bool
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, basePrefix) {
			continue
		}
		// Hide dotfiles unless the user explicitly typed a leading dot,
		// mirroring bash's default behaviour.
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(basePrefix, ".") {
			continue
		}
		names = append(names, name)
		isDir = append(isDir, e.IsDir())
	}
	if len(names) == 0 {
		return true
	}

	var completed string
	var completedIsDir bool
	if len(names) == 1 {
		completed = names[0]
		completedIsDir = isDir[0]
	} else {
		completed = longestCommonPrefix(names)
		if completed == basePrefix {
			// Already at the deepest unambiguous prefix; nothing to add.
			return true
		}
	}

	// Build the replacement token in the same display form the user
	// typed (preserve ~ vs absolute vs relative).
	newToken := displayParent + completed
	if len(names) == 1 && completedIsDir {
		newToken += "/"
	}

	ed.SetValue(val[:start] + newToken)
	return true
}

// tryPathTabComplete is the Interactive-bound convenience wrapper.
// It calls the free helper against the main editor and invalidates
// the frame on a successful rewrite.
func (i *Interactive) tryPathTabComplete() bool {
	if tryPathTabCompleteEditor(i.ed, i.cfg.CWD) {
		i.invalidate()
		return true
	}
	return false
}

// looksLikePathToken reports whether tok is shaped like a filesystem
// path. Paths must either start with ~, /, ./, ../ or contain a /.
// Plain words are excluded so Tab on "hello" stays a no-op.
func looksLikePathToken(tok string) bool {
	if tok == "" {
		return false
	}
	if tok[0] == '~' || tok[0] == '/' {
		return true
	}
	if strings.HasPrefix(tok, "./") || strings.HasPrefix(tok, "../") {
		return true
	}
	return strings.Contains(tok, "/")
}

// resolvePathTabToken splits tok into (absolute parent dir, basename
// prefix to match, display-form parent the user typed). ok is false
// when the parent dir can't be resolved (e.g. ~ with no $HOME).
func resolvePathTabToken(tok, cwd string) (parentAbs, basePrefix, displayParent string, ok bool) {
	// Detect ~ expansion.
	expanded := tok
	homePrefix := ""
	if tok == "~" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", "", "", false
		}
		// "~" alone: complete in $HOME. parent = home, base = "".
		return home, "", "~/", true
	}
	if strings.HasPrefix(tok, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", "", "", false
		}
		expanded = home + tok[1:]
		homePrefix = "~"
	}

	dir, base := splitDirBase(expanded)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}

	// Reconstruct the display form the user typed for the parent,
	// keeping ~ when they used it. The base is dropped — the caller
	// substitutes the completed name.
	display := tok[:len(tok)-len(base)]
	if homePrefix != "" && !strings.HasPrefix(display, "~") {
		display = homePrefix + display[len(homePrefix):]
	}
	return dir, base, display, true
}

// splitDirBase is like filepath.Split but preserves the trailing
// slash convention: "foo" => (".", "foo"); "foo/" => ("foo", "");
// "a/b" => ("a/", "b"); "/" => ("/", ""). Returned dir always has
// the trailing separator when non-empty so callers can rebuild paths
// by concatenation.
func splitDirBase(p string) (dir, base string) {
	if p == "" {
		return ".", ""
	}
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ".", p
	}
	return p[:i+1], p[i+1:]
}

func longestCommonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	prefix := ss[0]
	for _, s := range ss[1:] {
		n := 0
		for n < len(prefix) && n < len(s) && prefix[n] == s[n] {
			n++
		}
		prefix = prefix[:n]
		if prefix == "" {
			return ""
		}
	}
	return prefix
}

func (i *Interactive) runSlash(ctx context.Context, cmd string) (done bool) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return false
	}

	if isSkillCommand(cmd) {
		var available []*skills.Skill
		if i.cfg.SkillSnapshot != nil {
			available = i.cfg.SkillSnapshot()
		}
		prompt, _, err := expandSkillCommand(cmd, available)
		if err != nil {
			i.mu.Lock()
			i.statusErr = err.Error()
			i.statusOK = ""
			i.mu.Unlock()
			return false
		}
		i.submitOrQueuePrompt(ctx, prompt)
		return false
	}

	switch strings.ToLower(parts[0]) {
	case "/goal":
		i.runGoalCommand(ctx, cmd, parts)
	case "/exit":
		return true
	case "/clear":
		if i.agent != nil {
			i.agent.SetMessages(nil)
		}
		i.mu.Lock()
		i.toolCalls = map[string]*tui.ToolCallView{}
		i.toolOrder = nil
		i.toolGate = map[string]int{}
		i.statusErr = ""
		i.statusOK = ""
		i.helpBlock = nil
		i.parkedTurn = 0
		i.parkedTotal = 0
		i.scrollOffset = 0
		i.extNotes = nil
		i.reloadErrors = nil
		handoff, persistHandoff := i.resetCompactContinuationLocked()
		i.view.InvalidateRenderCache()
		i.mu.Unlock()
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
	case "/help":
		i.mu.Lock()
		i.helpBlock = renderHelpBlock(i.cfg.Theme, i.lastCols(), i.llamaConfigured)
		i.statusErr = ""
		i.statusOK = ""
		// Pin the viewport to the newest content so the help block,
		// which we just appended to the end of the transcript, is
		// what the user actually sees.
		i.scrollOffset = 0
		i.mu.Unlock()
	case "/login":
		i.dialog.Open(i.cfg.ZutHome)
	case "/logout":
		if len(parts) >= 2 {
			// Explicit target: /logout anthropic | openai | all
			i.doLogout(parts[1])
			break
		}
		// No arg: open the picker over whichever providers are
		// currently logged in. If nothing's logged in, bail with a
		// status line.
		i.openLogoutDialog()
	case "/model":
		if len(parts) >= 2 {
			i.applyModelSelection("", parts[1])
		} else if i.llamaConfigured && i.cfg.RefreshLlamaCPPModels != nil {
			i.mu.Lock()
			i.modelRefreshing = true
			i.statusOK = "Refreshing models"
			i.statusErr = ""
			i.mu.Unlock()
			go func() {
				refreshCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
				i.modelRefresh <- modelRefreshResult{err: i.cfg.RefreshLlamaCPPModels(refreshCtx)}
			}()
		} else {
			i.openModelPickerAfterRefresh(nil)
		}
	case "/llama":
		if i.cfg.LlamaCPPConfig == nil {
			i.mu.Lock()
			i.statusErr = "llama.cpp configuration is unavailable"
			i.statusOK = ""
			i.mu.Unlock()
			break
		}
		baseURL, apiKey, configErr := i.cfg.LlamaCPPConfig()
		if configErr == nil {
			configErr = i.llamaDialog.Open(baseURL, apiKey, i.invalidate)
		}
		if configErr != nil {
			i.mu.Lock()
			i.statusErr = configErr.Error()
			i.statusOK = ""
			i.mu.Unlock()
		}
	case "/reasoning":
		i.openReasoningDialog()
	case "/fast":
		enabled := i.cfg.FastMode == nil || !*i.cfg.FastMode
		i.applySettingToggle("fast_mode", enabled)
	case "/orchestrator":
		enabled := i.cfg.AutoSubagentsEnabled == nil || !*i.cfg.AutoSubagentsEnabled
		i.applySettingToggle("auto_subagents_enabled", enabled)
	case "/settings":
		i.openSettingsDialog()
	case "/sessions":
		i.mu.Lock()
		i.sessionLoads = i.sessionDialog.Open(ctx, i.sessionsRoot(), i.cfg.CWD)
		i.mu.Unlock()
	case "/fork":
		i.doSessionFork()
	case "/jump":
		i.openJumpDialog(parts[1:])
	case "/btw":
		i.openBtwDialog(parts[1:])
	case "/skills":
		i.openSkillsDialog()
	case "/compact":
		i.runCompact(ctx, compactContinuationRequest{origin: compactOriginManual})
	case "/study":
		// Canned prompt that tells the agent to read every file
		// in some target so its later turns have the whole thing
		// in context. With no argument, the target is the current
		// directory. With an argument, the target is whatever the
		// user passed — typed by hand, drag-dropped, or selected
		// via the @ file picker (which is why we accept both files
		// and directories; the @-picker chips for both have already
		// been expanded to absolute paths by expandFileChips above).
		// Dispatched through the normal queue-or-start path so it
		// behaves identically to typing the prompt by hand.
		studyPrompt := buildStudyPrompt(strings.TrimSpace(strings.TrimPrefix(cmd, parts[0])), i.cfg.CWD)
		i.mu.Lock()
		busy := i.busy
		compacting := i.compacting
		ag := i.agent
		i.mu.Unlock()
		if busy {
			var handoff json.RawMessage
			var persistHandoff bool
			if ag != nil && !compacting {
				i.mu.Lock()
				handoff, persistHandoff = i.resetCompactContinuationLocked()
				ag.QueueMessage(studyPrompt)
				i.mu.Unlock()
			} else {
				i.mu.Lock()
				handoff, persistHandoff = i.resetCompactContinuationLocked()
				i.queued = append(i.queued, studyPrompt)
				i.mu.Unlock()
			}
			if persistHandoff {
				i.persistCompactHandoff(handoff)
			}
			i.invalidate()
			break
		}
		i.startTurn(ctx, studyPrompt)
	case "/cd":
		// Hidden command: switch the running session's cwd. Not in
		// slash_suggest, not in /help. Used by the workspaces
		// extension's panel-key Enter handler so picking a row
		// jumps zut into that directory without relaunching.
		//
		// Recovers the raw argument (path) from the original cmd
		// string rather than parts, so paths with spaces survive.
		// The host's ChangeCWD hook handles validation, session
		// close + reopen, agent rebuild, sandbox re-rooting, and
		// re-jail-if-jailed semantics.
		if i.cfg.ChangeCWD == nil {
			i.mu.Lock()
			i.statusErr = "/cd unavailable: host did not wire ChangeCWD"
			i.mu.Unlock()
			break
		}
		path := strings.TrimSpace(strings.TrimPrefix(cmd, parts[0]))
		if path == "" {
			i.mu.Lock()
			i.statusErr = "/cd: missing path"
			i.mu.Unlock()
			break
		}
		if err := i.cfg.ChangeCWD(path); err != nil {
			i.mu.Lock()
			i.statusErr = "/cd: " + err.Error()
			i.statusOK = ""
			i.mu.Unlock()
			break
		}
		// ChangeCWD has already updated i.cfg.CWD and swapped the
		// agent + session. Reset transient TUI state so the new
		// session opens clean.
		i.mu.Lock()
		i.toolCalls = map[string]*tui.ToolCallView{}
		i.toolOrder = nil
		i.toolGate = map[string]int{}
		i.helpBlock = nil
		i.parkedTurn = 0
		i.statusOK = "cwd " + i.cfg.CWD
		i.statusErr = ""
		i.mu.Unlock()
		i.fileSuggest.Reset()
		i.fileSuggest.SetCWD(i.cfg.CWD)
		i.invalidate()
	case "/jail":
		if i.cfg.Sandbox == nil {
			i.mu.Lock()
			i.statusErr = "sandbox not available in this build"
			i.mu.Unlock()
			break
		}
		i.cfg.Sandbox.Lock()
		i.mu.Lock()
		i.statusOK = "jailed to " + i.cfg.CWD + " (tools cannot touch paths outside this directory)"
		i.statusErr = ""
		i.mu.Unlock()
	case "/unjail":
		if i.cfg.Sandbox == nil {
			i.mu.Lock()
			i.statusErr = "sandbox not available in this build"
			i.mu.Unlock()
			break
		}
		i.cfg.Sandbox.Unlock()
		i.mu.Lock()
		i.statusOK = "unjailed"
		i.statusErr = ""
		i.mu.Unlock()
	case "/reload-ext":
		i.runReloadExt(ctx)
	case "/telegram", "/tg":
		if len(parts) >= 2 {
			i.doTelegram(parts[1])
			break
		}
		i.openTelegramDialog()
	case "/session":
		if len(parts) >= 2 {
			action := parts[1]
			arg := ""
			if len(parts) >= 3 {
				arg = strings.Join(parts[2:], " ")
			}
			i.doSessionOp(action, arg)
			break
		}
		i.openSessionOpsDialog()
	case "/subagents":
		i.runSubagents(ctx, parts[1:])
	default:
		// Last-resort fallback: try the extension manager. Built-in
		// cases above always win; this branch only fires for slash
		// commands the extension manager registered. Same routing as
		// the editor's submit-handler dispatch path so the autocomplete
		// "enter on highlighted suggestion" flow also works.
		extName := strings.TrimPrefix(parts[0], "/")
		if i.cfg.Extensions != nil && i.cfg.Extensions.HasCommand(extName) {
			rest := ""
			if len(parts) > 1 {
				rest = strings.Join(parts[1:], " ")
			}
			go i.invokeExtensionCommand(ctx, extName, rest)
			return false
		}
		i.mu.Lock()
		i.statusErr = "unknown command: " + parts[0]
		i.mu.Unlock()
	}
	return false
}

func (i *Interactive) openModelPickerAfterRefresh(refreshErr error) {
	var loggedIn []string
	if i.cfg.LoggedInProviders != nil {
		loggedIn = i.cfg.LoggedInProviders()
	}
	i.modelDialog.Open(i.cfg.Model, loggedIn, i.cfg.Reasoning)
	i.mu.Lock()
	if refreshErr != nil {
		i.statusErr = "llama.cpp model refresh: " + refreshErr.Error()
		i.statusOK = ""
	} else if i.statusOK == "Refreshing models" {
		i.statusOK = ""
	}
	i.mu.Unlock()
}

// openLogoutDialog shows the provider picker for `/logout` with no
// argument. Only providers the user is currently logged into are
// listed, plus an "all" entry when more than one is present. If
// nothing's logged in, writes a status line instead of opening an
// empty dialog.
func (i *Interactive) openLogoutDialog() {
	if i.cfg.AuthManager == nil {
		i.mu.Lock()
		i.statusErr = "no auth manager configured"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	store := i.cfg.AuthManager.Store()
	if store == nil {
		i.mu.Lock()
		i.statusErr = "auth store is not available"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	creds, err := store.Load()
	if err != nil {
		i.mu.Lock()
		i.statusErr = "read auth store: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}

	var items []logoutItem
	for _, p := range []string{"anthropic", "kimi", "google", "github-copilot"} {
		if creds.Has(p) {
			method := creds.Method(p)
			if method == "oauth" {
				method = "subscription"
			}
			items = append(items, logoutItem{
				label:  providerLabel(p),
				target: p,
				method: method,
			})
		}
	}
	if creds.OpenAI.APIKey != "" || creds.OpenAI.APIKeyCommand != nil {
		items = append(items, logoutItem{label: providerLabel("openai"), target: "openai", method: "api key"})
	}
	if creds.OpenAI.OAuth != nil {
		items = append(items, logoutItem{label: providerLabel("openai-codex"), target: "openai-codex", method: "subscription"})
	}
	for p, c := range creds.AdditionalAPIKeyCreds {
		if c.APIKey != "" || c.APIKeyCommand != nil || c.BaseURL != "" {
			items = append(items, logoutItem{label: providerLabel(p), target: p, method: "api key"})
		}
	}
	if len(items) == 0 {
		i.mu.Lock()
		i.statusOK = "no credentials stored; already logged out"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if len(items) > 1 {
		items = append(items, logoutItem{label: "all", target: "all"})
	}

	i.logoutDialog.Open(items)
	i.invalidate()
}

// doLogout clears credentials for the given provider (or all providers)
// from auth.json. If the active agent was using those credentials, it
// is torn down so the user is forced through /login before their next
// prompt.
//
// target: "anthropic" | "openai" | "kimi" | "github-copilot" | "all"
func (i *Interactive) doLogout(target string) {
	if i.cfg.AuthManager == nil {
		i.mu.Lock()
		i.statusErr = "no auth manager configured"
		i.mu.Unlock()
		return
	}
	store := i.cfg.AuthManager.Store()
	if store == nil {
		i.mu.Lock()
		i.statusErr = "auth store is not available"
		i.mu.Unlock()
		return
	}

	var providers []string
	switch target {
	case "", "all":
		providers = append([]string{"anthropic", "openai", "openai-codex", "kimi", "google", "github-copilot"}, auth.APIKeyProviders()...)
	case "anthropic", "openai", "openai-codex", "kimi", "google", "github-copilot":
		providers = []string{target}
	default:
		known := false
		for _, p := range auth.APIKeyProviders() {
			if target == p {
				known = true
				break
			}
		}
		if !known {
			i.mu.Lock()
			i.statusErr = "unknown provider: " + target
			i.mu.Unlock()
			return
		}
		providers = []string{target}
	}

	var errs []string
	clearedCurrent := false
	for _, p := range providers {
		var err error
		switch p {
		case "openai":
			err = store.ClearAPIKey("openai")
		case "openai-codex":
			err = store.ClearOAuth("openai")
		default:
			err = store.Clear(p)
		}
		if err != nil {
			errs = append(errs, p+": "+err.Error())
			continue
		}
		if p == "kimi" && i.cfg.SetKimiCLIFallbackDisabled != nil {
			if err := i.cfg.SetKimiCLIFallbackDisabled(true); err != nil {
				errs = append(errs, p+": "+err.Error())
				continue
			}
		}
		if p == i.cfg.Provider {
			clearedCurrent = true
		}
	}

	llamaConfigured := i.llamaConfigured
	if i.cfg.LlamaCPPConfig != nil {
		baseURL, _, err := i.cfg.LlamaCPPConfig()
		llamaConfigured = err == nil && baseURL != ""
	}

	i.mu.Lock()
	i.llamaConfigured = llamaConfigured
	if len(errs) > 0 {
		i.statusErr = "logout errors: " + strings.Join(errs, "; ")
		i.mu.Unlock()
		return
	}
	i.statusErr = ""
	var handoff json.RawMessage
	var persistHandoff bool
	if clearedCurrent {
		// The running agent was using a credential we just wiped. Drop
		// it so prompts can't go out with the stale client, and hint at
		// /login. Close any LSP processes owned by its tools first.
		if i.agent != nil {
			_ = tools.CloseLSPManagers(i.agent.ToolsSnapshot())
		}
		i.agent = nil
		handoff, persistHandoff = i.resetCompactContinuationLocked()
		i.statusOK = "logged out of " + strings.Join(providers, ", ") + ". type /login to sign back in."
	} else {
		i.statusOK = "logged out of " + strings.Join(providers, ", ")
	}
	i.mu.Unlock()
	if persistHandoff {
		i.persistCompactHandoff(handoff)
	}
}

func providerSetupInfo(provider string) (string, []string, bool) {
	const docsURL = "https://raw.githubusercontent.com/bnema/zut/main/docs/providers.md"
	switch provider {
	case "amazon-bedrock":
		return "Amazon Bedrock setup", []string{
			"Amazon Bedrock uses AWS credentials instead of a generic zut API-key entry.",
			"Configure an AWS profile, IAM keys, bearer token, or role-based credentials.",
			"",
			"For Bedrock API keys, set:",
			"  AWS_BEARER_TOKEN_BEDROCK=...",
			"  AWS_REGION=us-east-1",
			"",
			"Docs:",
			"  " + docsURL,
		}, true
	case "google-vertex":
		return "Google Vertex AI setup", []string{
			"Google Vertex AI usually uses Google Cloud credentials and project settings.",
			"Set a Google API key, application-default credentials, or a service account.",
			"",
			"Common environment:",
			"  GOOGLE_CLOUD_API_KEY=...",
			"  GOOGLE_CLOUD_PROJECT=...",
			"  GOOGLE_CLOUD_LOCATION=us-central1",
			"",
			"Docs:",
			"  " + docsURL,
		}, true
	case "cloudflare-workers-ai":
		return "Cloudflare Workers AI setup", []string{
			"Cloudflare Workers AI needs both an API token and an account ID.",
			"",
			"Set:",
			"  CLOUDFLARE_API_KEY=...",
			"  CLOUDFLARE_ACCOUNT_ID=...",
			"",
			"Docs:",
			"  " + docsURL,
		}, true
	case "cloudflare-ai-gateway":
		return "Cloudflare AI Gateway setup", []string{
			"Cloudflare AI Gateway needs an API token, account ID, and gateway ID.",
			"",
			"Set:",
			"  CLOUDFLARE_API_KEY=...",
			"  CLOUDFLARE_ACCOUNT_ID=...",
			"  CLOUDFLARE_GATEWAY_ID=...",
			"",
			"Docs:",
			"  " + docsURL,
		}, true
	case "azure-openai-responses":
		return "Azure OpenAI Responses setup", []string{
			"Azure OpenAI needs an API key plus your Azure endpoint or deployment setup.",
			"",
			"Set:",
			"  AZURE_OPENAI_API_KEY=...",
			"  AZURE_OPENAI_BASE_URL=https://your-resource.openai.azure.com",
			"  AZURE_OPENAI_API_VERSION=2024-02-01",
			"",
			"Docs:",
			"  " + docsURL,
		}, true
	default:
		return "", nil, false
	}
}

func (i *Interactive) saveLlamaCPPLogin(baseURL, apiKey string) {
	if i.cfg.AuthManager == nil || i.cfg.AuthManager.Store() == nil {
		i.dialog.ShowResult(false, "auth store is unavailable")
		return
	}
	if err := i.cfg.AuthManager.Store().SetEndpointCredential(provider.LlamaCPPProviderID, baseURL, apiKey); err != nil {
		i.dialog.ShowResult(false, err.Error())
		return
	}
	i.mu.Lock()
	i.llamaConfigured = true
	i.statusErr = ""
	i.statusOK = "configured llama.cpp router"
	i.mu.Unlock()
	i.dialog.ShowResult(true, "")
}

func (i *Interactive) startAPIKeyFlow(provider string) {
	if title, lines, ok := providerSetupInfo(provider); ok {
		i.dialog.ShowInfo(title, lines)
		return
	}
	if provider == "kimi" && i.cfg.SetKimiCLIFallbackDisabled != nil {
		_ = i.cfg.SetKimiCLIFallbackDisabled(false)
	}
	url, err := i.cfg.AuthManager.StartAPIKey(provider)
	if err != nil {
		i.dialog.ShowResult(false, err.Error())
		return
	}
	i.dialog.ShowWaiting(url)
}

func (i *Interactive) startOAuthFlow(provider string) {
	if provider == "kimi" && i.cfg.SetKimiCLIFallbackDisabled != nil {
		_ = i.cfg.SetKimiCLIFallbackDisabled(false)
	}
	// Device-code providers already support headless login and must only
	// start one polling flow.
	if provider == "kimi" || provider == "xai" || provider == "github-copilot" {
		loginURL, err := i.cfg.AuthManager.StartOAuth(provider)
		if err != nil {
			i.dialog.ShowResult(false, err.Error())
			return
		}
		i.dialog.ShowWaiting(loginURL)
		return
	}
	// Always run the manual/copy-code flow in parallel with the local
	// callback server so headless environments (docker, SSH) can paste
	// the authorization code directly without first pressing 'p'.
	_, err := i.cfg.AuthManager.StartOAuth(provider)
	if err != nil {
		i.dialog.ShowResult(false, err.Error())
		return
	}
	manualURL, mErr := i.cfg.AuthManager.StartManualOAuth(provider)
	if mErr == nil {
		i.dialog.ShowWaiting(manualURL)
	} else {
		i.dialog.ShowResult(false, mErr.Error())
	}
}

func (i *Interactive) startManualOAuthFlow(provider string) {
	if i.cfg.AuthManager == nil {
		return
	}
	i.cfg.AuthManager.CancelOAuth()
	url, err := i.cfg.AuthManager.StartManualOAuth(provider)
	if err != nil {
		i.dialog.ShowResult(false, err.Error())
		return
	}
	i.dialog.url = url
	i.invalidate()
}

func (i *Interactive) submitManualOAuthCode(code string) {
	if i.cfg.AuthManager == nil {
		return
	}
	go func() {
		if err := i.cfg.AuthManager.CompleteManualOAuth(i.runCtx, code); err != nil {
			i.dialog.ShowResult(false, err.Error())
			i.invalidate()
		}
	}()
}

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
func (i *Interactive) cancelAndWaitForIdle() {
	i.mu.Lock()
	busy := i.busy
	cancel := i.cancelTurn
	i.mu.Unlock()
	if !busy {
		return
	}
	if cancel != nil {
		i.mu.Lock()
		handoff, persistHandoff := i.resetCompactContinuationLocked()
		i.mu.Unlock()
		cancel()
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
	}
	// ConfirmToolCall waits on its response channel rather than the turn
	// context, so cancellation must also resolve every pending prompt.
	i.confirmDialog.CancelAll("turn cancelled")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		i.mu.Lock()
		done := !i.busy
		i.mu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// openBtwDialog opens the side-chat overlay with a frozen snapshot
// of the current main session. The optional argument is auto-
// submitted as the first question, so '/btw does X work?' fires the
// model call immediately instead of just opening an empty dialog.
func (i *Interactive) openBtwDialog(args []string) {
	i.agentMu.Lock()
	defer i.agentMu.Unlock()
	i.mu.Lock()
	ag := i.agent
	system := ""
	if ag != nil {
		system, _ = ag.PromptConfig()
	}
	theme := i.cfg.Theme
	model := i.cfg.Model
	cwd := i.cfg.CWD
	compact := i.compactModeEnabled()
	flatTools := i.cfg.FlatTools
	lineInput := tui.NormalizeInputStyle(i.cfg.TUIInputStyle) == tui.InputStyleLines
	i.mu.Unlock()
	if ag == nil {
		i.mu.Lock()
		i.statusErr = "not logged in. type /login first."
		i.mu.Unlock()
		return
	}
	seed := strings.TrimSpace(strings.Join(args, " "))
	i.btwDialog.Open(theme, ag, system, model, cwd, seed, compact, flatTools, lineInput, i.invalidate)
	i.invalidate()
}

// submitOrQueuePrompt submits a slash command's expanded prompt immediately,
// or queues it behind the active turn using the normal text-only queue.
func (i *Interactive) submitOrQueuePrompt(ctx context.Context, prompt string) {
	i.mu.Lock()
	if i.agent == nil {
		i.statusErr = "not logged in. type /login first."
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if i.busy {
		// Keep the mutex held while enqueueing so the turn-completion
		// goroutine cannot publish idle and inspect an empty queue between
		// this check and the append.
		var handoff json.RawMessage
		var persistHandoff bool
		if i.compacting {
			handoff, persistHandoff = i.resetCompactContinuationLocked()
			i.queued = append(i.queued, prompt)
		} else {
			handoff, persistHandoff = i.resetCompactContinuationLocked()
			i.agent.QueueMessage(prompt)
		}
		i.mu.Unlock()
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
		i.invalidate()
		return
	}
	i.mu.Unlock()
	i.startTurn(ctx, prompt)
}

// openSkillsDialog opens the skill inspector. The picker reflects
// whatever SkillSnapshot returns at call time, so edits to a
// SKILL.md made during a session show up on the next /skills.
func (i *Interactive) openSkillsDialog() {
	var list []*skills.Skill
	if i.cfg.SkillSnapshot != nil {
		list = i.cfg.SkillSnapshot()
	}
	i.skillsDialog.Open(list)
	i.invalidate()
}

// openJumpDialog builds a /jump picker from the current transcript.
// If the user typed "/jump foo" with a filter and it matches exactly
// one turn, jump there directly without showing the dialog.
func (i *Interactive) openJumpDialog(args []string) {
	if i.view == nil || len(i.view.Messages) == 0 {
		i.mu.Lock()
		i.statusErr = "nothing to jump to \u2014 the session is empty"
		i.mu.Unlock()
		return
	}
	filter := strings.TrimSpace(strings.Join(args, " "))
	i.jumpDialog.Open(i.view.Messages, filter)
	// Shortcut: with a filter argument that matches exactly one turn,
	// jump immediately and skip the picker.
	if filter != "" {
		if tgts := i.jumpDialog.Targets(); len(tgts) == 1 {
			t := tgts[0]
			i.jumpDialog.Close()
			i.applyJumpSelection(t.MessageIdx, t.TurnNo)
		}
	}
}

// applyJumpSelection scrolls the chat viewport so the user message at
// msgIdx is visible at (or near) the top of the chat area. Uses the
// anchor slice returned by view.BuildWithAnchors so the mapping from
// message index to row is exact, regardless of variable-height tool
// blocks above the target.
func (i *Interactive) applyJumpSelection(msgIdx, turnNo int) {
	cols := i.lastCols()
	chat, anchors := i.view.BuildWithAnchors(cols)
	var row int
	found := false
	for _, a := range anchors {
		if a.MessageIdx == msgIdx {
			row = a.Row
			found = true
			break
		}
	}
	if !found {
		i.mu.Lock()
		i.statusErr = "could not resolve jump target"
		i.mu.Unlock()
		return
	}

	chatLen := len(chat)
	page := i.chatPage()
	if page < 1 {
		page = 1
	}
	// scrollOffset is measured from the bottom of the chat slice, so
	// to place `row` at the top of the viewport we want:
	//     chatLen - scrollOffset - page == row
	// Solve for scrollOffset and clamp to [0, chatLen-page].
	offset := chatLen - (row + page)
	if offset < 0 {
		offset = 0
	}
	maxOffset := chatLen - page
	if maxOffset < 0 {
		maxOffset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}

	i.mu.Lock()
	i.scrollOffset = offset
	i.parkedTurn = turnNo
	i.parkedTotal = totalTurnsLocked(i.view.Messages)
	i.statusOK = fmt.Sprintf("jumped to turn %d", turnNo)
	i.statusErr = ""
	i.mu.Unlock()
}

// totalTurnsLocked counts user messages in the transcript. Caller is
// assumed to hold i.mu (the name is a mild reminder; this function
// itself doesn't touch shared state beyond the slice it's handed).
func totalTurnsLocked(msgs []provider.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == provider.RoleUser {
			n++
		}
	}
	return n
}

// applySessionSelection loads the given session via the cli-provided
// callback and snaps the viewport to the bottom (the latest message)
// so the user lands at the live tail of the resumed conversation.
func (i *Interactive) applySessionSelection(path string) {
	if i.cfg.LoadSession == nil {
		i.mu.Lock()
		i.statusErr = "session loading is not wired in this build"
		i.mu.Unlock()
		return
	}
	i.mu.Lock()
	if i.sessionLoading {
		i.statusErr = "already resuming a session"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.sessionLoading = true
	i.statusOK = "resuming session: " + path
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
	i.markSessionTitleSwitching()

	go func() {
		err := i.cfg.LoadSession(path)
		if err != nil {
			i.restoreFailedSessionTitle()
			i.mu.Lock()
			i.sessionLoading = false
			i.statusErr = err.Error()
			i.statusOK = ""
			i.mu.Unlock()
			i.invalidate()
			return
		}
		i.restoreLoadedSessionTitle()
		state := decodeCompactHandoff(i.currentCompactHandoff())
		i.mu.Lock()
		i.compactContinuation = state
		i.sessionLoading = false
		i.statusOK = "resumed session: " + path
		i.statusErr = ""
		i.parkedTurn = 0
		i.parkedTotal = 0
		i.scrollOffset = 0
		// Fresh transcript swapped in: drop the auto-follow baseline so
		// the next render's follow guard doesn't see the wholesale
		// length change as a delta and jump the viewport.
		i.prevChatLen = 0
		i.prevChatCols = 0
		i.extNotes = nil
		i.view.InvalidateRenderCache()
		if i.agent != nil {
			i.view.Messages = filterHiddenTranscriptMessages(i.agent.Messages())
			i.cumUsage = i.agent.Cost()
			if last := i.agent.LastTurnUsage(); last.InputTokens > 0 || last.CacheReadTokens > 0 || last.CacheWriteTokens > 0 {
				i.lastCtxInput = last.InputTokens + last.CacheReadTokens + last.CacheWriteTokens
			} else {
				i.lastCtxInput = 0
			}
			// Snap to the tail again — the swap brought in a fresh
			// transcript whose markdown / chroma cost we don't want
			// blocking the redraw.
			if len(i.view.Messages) > initialResumeTailLimit {
				i.view.TailLimit = initialResumeTailLimit
			} else {
				i.view.TailLimit = 0
			}
		}
		i.mu.Unlock()
		i.invalidate()
		if state.reason != compactContinuationNone {
			i.startRestoredCompactHandoff(i.runCtx)
		}
	}()
}

func (i *Interactive) applyModelSelection(prov, model string) {
	i.swapModel(prov, model, i.cfg.BuildAgentFor, false)
}

// applyRescueModelSelection is like applyModelSelection but routes
// through BuildAgentForRescue so launch-time --api-key / --base-url
// overrides are dropped before the new agent is built. Falls back to
// the regular builder when the host doesn't wire a rescue builder.
func (i *Interactive) applyRescueModelSelection(prov, model string) {
	builder := i.cfg.BuildAgentForRescue
	if builder == nil {
		builder = i.cfg.BuildAgentFor
	}
	i.swapModel(prov, model, builder, true)
}

// swapModel applies a /model selection (or a rescue selection) using
// the supplied builder. rescue=true tags the success message so the
// user can see that launch-time overrides were ignored.
func (i *Interactive) swapModel(prov, model string, builder func(string, string) (*core.Agent, string, string, error), rescue bool) {
	var replaced bool
	swap := func() {
		replaced = i.swapModelUnserialized(prov, model, builder, rescue)
	}
	if i.cfg.SessionTransition != nil {
		i.cfg.SessionTransition(swap)
	} else {
		swap()
	}
	if replaced {
		i.resetCompactHandoff()
	}
}

func (i *Interactive) swapModelUnserialized(prov, model string, builder func(string, string) (*core.Agent, string, string, error), rescue bool) bool {
	if model == "" {
		return false
	}
	m, err := provider.FindModel(prov, model)
	if err != nil {
		i.mu.Lock()
		i.statusErr = err.Error()
		i.mu.Unlock()
		return false
	}
	// Same provider AND not a rescue retry: just swap the model on
	// the existing agent. Mixed-API providers dispatch from model metadata,
	// so the client remains reusable. Rescue retries always rebuild so a stale
	// auth header / base URL can't carry over.
	if !rescue && i.agent != nil && m.Provider == i.cfg.Provider {
		i.mu.Lock()
		i.cfg.Model = m.ID
		i.agent.Model = m.ID
		i.statusOK = "model: " + m.ID
		i.statusErr = ""
		i.mu.Unlock()
		if i.cfg.PersistModel != nil {
			i.cfg.PersistModel(i.cfg.Provider, m.ID)
		}
		return false
	}
	if builder == nil {
		i.mu.Lock()
		i.statusErr = "cannot switch provider: no builder configured"
		i.mu.Unlock()
		return false
	}
	// Snapshot the current transcript and cumulative usage BEFORE we
	// build the replacement agent so we can hand them off. Without
	// this the user perceives the entire session as wiped on a
	// cross-provider /model swap.
	var carryMsgs []provider.Message
	var carryCost provider.Usage
	if i.agent != nil {
		carryMsgs = i.agent.Messages()
		carryCost = i.agent.Cost()
	}

	ag, p, md, err := builder(m.Provider, m.ID)
	if err != nil {
		i.mu.Lock()
		i.statusErr = err.Error()
		i.mu.Unlock()
		return false
	}

	// Replay the transcript and seed the cost on the freshly-built
	// agent. Messages travel cleanly between providers because they
	// use the same provider.Message shape; tool-call ids are local
	// to a turn so cross-provider continuation never confuses the
	// new model (it just sees the assistant's reply, no orphan
	// tool_use blocks because /model swaps are gated to idle state).
	if len(carryMsgs) > 0 {
		ag.SetMessages(carryMsgs)
	}
	ag.SeedCost(carryCost)

	if i.agent != nil && i.agent != ag {
		_ = tools.CloseLSPManagers(i.agent.ToolsSnapshot())
	}
	i.agentMu.Lock()
	i.mu.Lock()
	i.prepareReplacementAgentLocked(ag)
	i.agent = ag
	i.cfg.Provider = p
	i.cfg.Model = md
	if rescue {
		i.statusOK = "rescue retry: switched to " + p + " / " + md + " (ignored --api-key / --base-url)"
	} else {
		i.statusOK = "switched to " + p + " / " + md
	}
	i.statusErr = ""
	// Render cache keys are width+content based, so the new agent's
	// identical messages will reuse the existing entries. Nothing
	// to invalidate.
	i.mu.Unlock()
	i.agentMu.Unlock()
	// The new agent was built off the base tool registry, so any
	// dynamically-registered tools need to be reattached. The apply
	// helpers are no-ops when their feature is inactive, so the
	// cross-provider path still works on a vanilla setup.
	i.applyAutoSubagentsTool()
	i.applyTelegramTools(i.telegramBridge != nil)
	if i.cfg.PersistModel != nil {
		i.cfg.PersistModel(p, md)
	}
	return true
}

func (i *Interactive) handleAuthEvent(ev auth.Event) {
	switch ev.Kind {
	case "started":
		i.dialog.ShowWaiting(ev.URL)
	case "browser_open":
		// no-op
	case "error":
		i.dialog.ShowResult(false, ev.Message)
	case "success":
		// Keep the credential-driven resolve and its final replacement in the
		// same transition as settings/session changes. Otherwise a build that
		// started under the old web-search policy could commit afterward.
		var buildErr error
		var clearHandoff bool
		buildAndCommitLogin := func() {
			ag, prov, model, err := i.cfg.BuildAgent()
			if err != nil {
				buildErr = err
				return
			}
			i.agentMu.Lock()
			i.mu.Lock()
			oldAgent := i.agent
			i.prepareReplacementAgentLocked(ag)
			_, clearHandoff = i.resetCompactContinuationLocked()
			i.agent = ag
			i.cfg.Provider = prov
			i.cfg.Model = model
			i.statusErr = ""
			i.statusOK = "logged in to " + ev.Provider + " via " + ev.Method
			i.mu.Unlock()
			i.agentMu.Unlock()
			if oldAgent != nil {
				_ = tools.CloseLSPManagers(oldAgent.ToolsSnapshot())
			}
			i.applyAutoSubagentsTool()
			i.applyTelegramTools(i.telegramBridge != nil)
			// Authentication can change the provider used by the live
			// agent. Persist it on the active session just like /model.
			if i.cfg.PersistModel != nil {
				i.cfg.PersistModel(prov, model)
			}
		}
		if i.cfg.SessionTransition != nil {
			i.cfg.SessionTransition(buildAndCommitLogin)
		} else {
			buildAndCommitLogin()
		}
		if buildErr != nil {
			i.dialog.ShowResult(false, buildErr.Error())
			return
		}
		if clearHandoff {
			i.persistCompactHandoff(nil)
		}
		i.dialog.ShowResult(true, "")
	}
}

// clearPendingCompactTurnLocked drops work that only makes sense after the
// current compaction. The caller must hold i.mu.
func (i *Interactive) clearPendingCompactTurnLocked() {
	i.pendingCompactPrompt = ""
	i.pendingCompactImages = nil
	i.hasPendingCompactPrompt = false
	i.continueAfterCompact = false
	i.pendingPostCompactNote = ""
}

// compactHandoffLocked returns a self-contained persistence snapshot. The
// caller must hold i.mu.
func (i *Interactive) compactHandoffLocked() json.RawMessage {
	return encodeCompactHandoff(i.compactContinuation)
}

// setCompactContinuationLocked updates the private handoff state and returns
// its persistence snapshot plus whether persistence is needed. The caller must
// hold i.mu.
func (i *Interactive) setCompactContinuationLocked(state compactContinuationState) (json.RawMessage, bool) {
	if i.compactContinuation == state {
		return i.compactHandoffLocked(), false
	}
	i.compactContinuation = state
	return i.compactHandoffLocked(), true
}

// resetCompactContinuationLocked clears the private handoff state and returns
// its persistence snapshot plus whether persistence is needed. The caller must
// hold i.mu.
func (i *Interactive) resetCompactContinuationLocked() (json.RawMessage, bool) {
	return i.setCompactContinuationLocked(compactContinuationState{})
}

func (i *Interactive) persistCompactHandoff(state json.RawMessage) {
	if persist := i.cfg.PersistCompactHandoff; persist != nil {
		if err := persist(state); err != nil {
			i.ReportError(fmt.Errorf("persist compact handoff: %w", err))
		}
	}
}

func (i *Interactive) resetCompactHandoff() {
	i.mu.Lock()
	state, persist := i.resetCompactContinuationLocked()
	i.mu.Unlock()
	if persist {
		i.persistCompactHandoff(state)
	}
}

func (i *Interactive) currentCompactHandoff() json.RawMessage {
	if current := i.cfg.CurrentCompactHandoff; current != nil {
		return current()
	}
	return nil
}

func (i *Interactive) restoreCompactHandoff(state compactContinuationState) {
	i.mu.Lock()
	i.compactContinuation = state
	i.mu.Unlock()
}

func (i *Interactive) restoreCurrentCompactHandoff() compactContinuationState {
	state := decodeCompactHandoff(i.currentCompactHandoff())
	i.restoreCompactHandoff(state)
	return state
}

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
func classifyCompactHandoffResume(messages []provider.Message) compactHandoffResume {
	lastHandoff := -1
	for idx := len(messages) - 1; idx >= 0; idx-- {
		message := messages[idx]
		if message.Role == provider.RoleUser && message.Meta[autoCompactContinueMetaKey] == "true" {
			lastHandoff = idx
			break
		}
	}
	if lastHandoff < 0 {
		return compactHandoffAppendPrompt
	}
	for _, message := range messages[lastHandoff+1:] {
		switch message.Role {
		case provider.RoleTool:
			continue
		case provider.RoleAssistant:
			hasToolCall := false
			for _, content := range message.Content {
				if _, ok := content.(provider.ToolCallBlock); ok {
					hasToolCall = true
					break
				}
			}
			if !hasToolCall {
				return compactHandoffDiscard
			}
		default:
			return compactHandoffDiscard
		}
	}
	return compactHandoffContinueExisting
}

// startRestoredCompactHandoff resumes a handoff checkpoint restored from the
// active session. When its hidden context message was already persisted, the
// agent continues that existing turn rather than appending it again.
func (i *Interactive) startRestoredCompactHandoff(parent context.Context) {
	if parent == nil {
		parent = i.runCtx
	}
	if parent == nil {
		parent = context.Background()
	}
	i.mu.Lock()
	state := i.compactContinuation
	ag := i.agent
	busy := i.busy || i.compacting || i.autoCompacting || i.shellRunning
	if state.reason == compactContinuationNone || ag == nil || busy {
		i.mu.Unlock()
		return
	}

	var next string
	continueQueued := false
	var resume compactHandoffResume
	var handoff json.RawMessage
	var persistHandoff bool
	switch {
	case state.reason != compactContinuationForcedLength && len(i.queued) > 0:
		next, i.queued = i.queued[0], i.queued[1:]
		handoff, persistHandoff = i.resetCompactContinuationLocked()
	case state.reason != compactContinuationForcedLength && ag.QueuedMessageCount() > 0:
		continueQueued = true
		handoff, persistHandoff = i.resetCompactContinuationLocked()
	default:
		resume = classifyCompactHandoffResume(ag.Messages())
		if resume == compactHandoffDiscard {
			handoff, persistHandoff = i.resetCompactContinuationLocked()
		}
	}
	// Reserve the turn slot before persisting or dispatching so concurrent input
	// can only queue behind the restored handoff, never start a competing turn.
	starting := next != "" || continueQueued || resume == compactHandoffContinueExisting || resume == compactHandoffAppendPrompt
	i.busy = starting
	i.mu.Unlock()
	if persistHandoff {
		i.persistCompactHandoff(handoff)
	}
	if !starting {
		i.invalidate()
		return
	}
	switch {
	case next != "":
		i.startTurn(parent, next)
	case continueQueued:
		i.startTurnRequest(parent, "", nil, true, false)
	case resume == compactHandoffContinueExisting:
		i.startTurnRequest(parent, "", nil, true, false)
	case resume == compactHandoffAppendPrompt:
		i.startAutoCompactContinuation(parent)
	}
}

// startAutoCompactContinuation adds an internal user turn that tells the
// model to resume the task after threshold compaction. Provider requests
// cannot continue from an assistant tail on every supported API, so the
// continuation must be represented as a user message. It is hidden from
// the rendered transcript but remains in the persisted/model context.
func (i *Interactive) startAutoCompactContinuation(parent context.Context) {
	i.mu.Lock()
	ag := i.agent
	if ag == nil {
		i.busy = false
		pendingIdleWork := i.takePendingIdleWorkLocked()
		i.mu.Unlock()
		runPendingIdleWork(pendingIdleWork)
		i.invalidate()
		return
	}
	// The caller selected the continuation while holding i.mu. Keep that
	// ordering through the hidden append so an explicitly queued user prompt
	// cannot be overtaken by an old-task handoff. Forced truncation is the
	// existing exception: it keeps priority and the queued prompt runs after it.
	if i.compactContinuation.reason != compactContinuationForcedLength && len(i.queued) > 0 {
		next := i.queued[0]
		i.queued = i.queued[1:]
		handoff, persistHandoff := i.resetCompactContinuationLocked()
		i.mu.Unlock()
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
		i.startTurn(parent, next)
		return
	}
	if i.compactContinuation.reason != compactContinuationForcedLength && ag.QueuedMessageCount() > 0 {
		handoff, persistHandoff := i.resetCompactContinuationLocked()
		i.mu.Unlock()
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
		i.startTurnRequest(parent, "", nil, true, false)
		return
	}
	ag.AppendUserContext(autoCompactContinuationPrompt, map[string]string{
		autoCompactContinueMetaKey: "true",
	})
	i.mu.Unlock()
	i.startTurnRequest(parent, "", nil, true, false)
}

// runCompact invokes core.Agent.Compact and reflects the progress in
// the tui. It runs in a goroutine so the ui stays responsive; esc/ctrl+c
// cancel via the same cancelTurn channel used for normal turns.
//
// When auto is true the spinner message is pinned to "condensing
// history" and the status bar surfaces "(auto)" next to the context
// percentage so it's obvious the system triggered this, not the user.
// request.force tells the post-compact hand-off to resume even if the
// transcript tail has visible assistant text, for example after StopLength.
func (i *Interactive) runCompact(parent context.Context, request compactContinuationRequest) {
	auto := request.origin != compactOriginManual
	if i.agent == nil {
		i.mu.Lock()
		i.statusErr = "not logged in. type /login first."
		i.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	i.mu.Lock()
	i.busy = true
	i.compacting = true
	i.spin.Start()
	i.activity = newAgentActivity(i.cfg.Provider, i.cfg.Model)
	if auto {
		i.activity.kind = activityCondensingHistory
		i.autoCompacting = true
	} else {
		i.activity.kind = activityCompactingHistory
	}
	i.cancelTurn = cancel
	var initialHandoff json.RawMessage
	persistInitialHandoff := false
	if request.origin == compactOriginManual || request.origin == compactOriginRecovery {
		initialHandoff, persistInitialHandoff = i.resetCompactContinuationLocked()
	}
	i.statusErr = ""
	i.statusOK = ""
	// Do NOT set streamOn: the summary text should not be visible
	// in the chat while compacting. The user just sees the spinner
	// and can keep typing / queue prompts.
	i.scrollOffset = 0
	i.helpBlock = nil
	i.mu.Unlock()
	if persistInitialHandoff {
		i.persistCompactHandoff(initialHandoff)
	}
	i.invalidate()

	go func() {
		// Sink discards deltas — we don't stream the summary to the UI.
		sink := func(delta string) {}
		msgsBefore := i.agent.Messages()
		i.mu.Lock()
		statusRescueActive := i.compactContinuation.reason == compactContinuationStatusRescue
		i.mu.Unlock()
		continuationReason := classifyCompactionContinuation(request.origin, statusRescueActive, request.lastStop, request.turnError, msgsBefore)
		if request.force {
			continuationReason = compactContinuationForcedLength
		}
		// Keep the usual recent tail when possible, but never let it cover
		// the whole transcript. A short session can still be at 70–90% of
		// a model's window because one prompt or tool result is large; the
		// automatic path must summarize that session instead of failing
		// before it reaches the compaction provider.
		keepTail := 4
		if n := len(msgsBefore); n > 0 && keepTail >= n {
			keepTail = n - 1
		}
		summary, err := i.agent.Compact(ctx, keepTail, sink)
		_ = summary
		goalMessage, goalActive := i.goalContinuationMessage()
		i.mu.Lock()
		// Keep busy/compacting asserted while cleanup and queue selection run.
		// Completion updates can arrive in this window; clearing busy before
		// inspecting the queues lets them race a new turn or be stranded.
		i.resetStreamingStateLocked()
		i.cancelTurn = nil
		i.autoCompacting = false
		pendingIdleWork := i.takePendingIdleWorkLocked()

		// Drain pending work after the transcript is clean. Overflow recovery
		// continues the user message already in the transcript. Pre-turn
		// compaction preserves its complete text-and-images request. Regular
		// prompts typed during compaction remain in the host queue.
		var next string
		var nextImages []provider.ImageBlock
		var hasNext bool
		var continueExisting bool
		var continueAutomatically bool
		var continueGoal bool
		var handoff json.RawMessage
		var persistHandoff bool

		switch {
		case err != nil && ctx.Err() != nil:
			i.statusErr = ""
			if auto {
				i.statusOK = "auto-condense cancelled"
			} else {
				i.statusOK = "compaction cancelled"
			}
			i.queued = nil // drop queue on cancel
			i.clearPendingCompactTurnLocked()
			handoff, persistHandoff = i.resetCompactContinuationLocked()
			if i.agent != nil {
				i.agent.DrainQueuedMessages()
			}
		case err != nil:
			i.statusErr = "compaction failed: " + err.Error()
			i.statusOK = ""
			i.queued = nil // drop queue on error
			i.clearPendingCompactTurnLocked()
			handoff, persistHandoff = i.resetCompactContinuationLocked()
			if i.agent != nil {
				i.agent.DrainQueuedMessages()
			}
		default:
			i.statusErr = ""
			// Read token count from the compaction message meta.
			tokens := ""
			msgs := i.agent.Messages()
			if len(msgs) > 0 && msgs[0].Meta["compaction"] == "true" {
				tokens = msgs[0].Meta["tokens_before"]
			}
			switch {
			case i.pendingPostCompactNote != "":
				i.statusOK = i.pendingPostCompactNote
			case tokens != "":
				i.statusOK = fmt.Sprintf("compacted from ~%s tokens (ctrl+o to expand)", tokens)
			default:
				i.statusOK = "compacted (ctrl+o to expand)"
			}
			i.pendingPostCompactNote = ""
			i.extNotes = stripAutoCompactNotes(i.extNotes)
			i.lastCtxInput = 0
			i.toolCalls = map[string]*tui.ToolCallView{}
			i.toolOrder = nil
			i.toolGate = map[string]int{}
			i.resetTranscriptRenderLocked()
			switch {
			case i.continueAfterCompact:
				continueExisting = true
				i.continueAfterCompact = false
			case i.hasPendingCompactPrompt:
				next = i.pendingCompactPrompt
				nextImages = i.pendingCompactImages
				i.pendingCompactPrompt = ""
				i.pendingCompactImages = nil
				i.hasPendingCompactPrompt = false
				hasNext = true
			case continuationReason == compactContinuationForcedLength:
				// Forced truncated-output continuation keeps its existing
				// priority over an explicitly queued prompt.
				handoff, persistHandoff = i.setCompactContinuationLocked(compactContinuationState{reason: continuationReason})
				continueAutomatically = true
			case len(i.queued) > 0:
				next, i.queued = i.queued[0], i.queued[1:]
				hasNext = true
				handoff, persistHandoff = i.resetCompactContinuationLocked()
			case continuationReason == compactContinuationStructuralTail || continuationReason == compactContinuationStatusRescue:
				if continuationReason == compactContinuationStatusRescue {
					attempts := 1
					if i.compactContinuation.reason == compactContinuationStatusRescue {
						attempts = i.compactContinuation.rescueAttempts + 1
					}
					if attempts > maxStatusRescueContinuations {
						handoff, persistHandoff = i.resetCompactContinuationLocked()
						break
					}
					handoff, persistHandoff = i.setCompactContinuationLocked(compactContinuationState{
						reason:         continuationReason,
						rescueAttempts: attempts,
					})
				} else {
					handoff, persistHandoff = i.setCompactContinuationLocked(compactContinuationState{reason: continuationReason})
				}
				continueAutomatically = true
			case auto && goalActive:
				continueGoal = true
				handoff, persistHandoff = i.resetCompactContinuationLocked()
			default:
				handoff, persistHandoff = i.resetCompactContinuationLocked()
			}
		}
		// Keep the host busy until the hand-off decision is committed under
		// the mutex. A completion update arriving after the compaction result
		// but before this assignment is then queued for the selected follow-up
		// instead of starting a competing turn.
		i.busy = hasNext || continueExisting || continueAutomatically || continueGoal
		i.compacting = false
		i.autoCompacting = false
		i.mu.Unlock()
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
		runPendingIdleWork(pendingIdleWork)
		i.invalidate()

		if hasNext || continueExisting || continueAutomatically || continueGoal {
			p := i.runCtx
			if p == nil {
				p = context.Background()
			}
			switch {
			case continueExisting:
				i.startTurnRequest(p, "", nil, true, true)
			case hasNext:
				i.startTurnWithImages(p, next, nextImages)
			case continueAutomatically:
				i.startAutoCompactContinuation(p)
			case continueGoal:
				i.startGoalContinuation(p, goalMessage)
			}
		}
	}()
}

// shellEscapeCommand reports whether text is a "!command" shell
// escape and, if so, returns the command with the leading '!' (and
// surrounding whitespace) stripped. A bare "!" with no command is
// treated as not an escape so it falls through to the normal prompt
// path rather than running an empty shell.
func shellEscapeCommand(text string) (string, bool) {
	return ShellEscapeCommand(text)
}

// ShellEscapeCommand reports whether text is a "!command" shell
// escape and, if so, returns the command with the leading '!' (and
// surrounding whitespace) stripped. A bare "!" with no command is
// treated as not an escape so it falls through to the normal prompt
// path rather than running an empty shell.
func ShellEscapeCommand(text string) (string, bool) {
	trimmed := strings.TrimLeft(text, " \t")
	if !strings.HasPrefix(trimmed, "!") {
		return "", false
	}
	cmd := strings.TrimSpace(strings.TrimPrefix(trimmed, "!"))
	if cmd == "" {
		return "", false
	}
	return cmd, true
}

// startShellEscape runs a "!command" in the same shell the bash tool
// uses, in the session working directory, honoring the /jail sandbox.
// It shares the busy/cancel state with the agent: esc cancels it, and
// it refuses to start while a turn or another shell escape is already
// in flight. Its terminal-log output is appended to the transcript as
// user context without automatically starting a model turn.
func (i *Interactive) startShellEscape(parent context.Context, cmd string) {
	i.mu.Lock()
	if i.busy || i.shellRunning {
		i.statusErr = "busy — wait for the current turn to finish before running a shell command"
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if parent == nil {
		parent = i.runCtx
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	i.busy = true
	i.shellRunning = true
	i.shellLive = "$ " + cmd + "\n\n"
	i.cancelTurn = cancel
	i.statusErr = ""
	i.statusOK = ""
	i.spin.Start()
	i.activity = agentActivity{activity: activity{kind: activityRunningShellCommand}}
	// Clear stale extension notes the same way a new turn would so the
	// screen doesn't accumulate transient state.
	i.extNotes = nil
	i.scrollOffset = 0
	i.parkedTurn = 0
	i.parkedTotal = 0
	i.helpBlock = nil
	sandbox := i.cfg.Sandbox
	cwd := i.cfg.CWD
	i.mu.Unlock()
	i.invalidate()

	go func() {
		defer cancel()
		raw, _ := json.Marshal(map[string]any{"command": cmd})
		bash := &tools.BashTool{CWD: cwd, Sandbox: sandbox}
		progress := func(chunk string) {
			i.mu.Lock()
			i.shellLive += chunk
			i.mu.Unlock()
			i.invalidate()
		}
		res, err := bash.Execute(ctx, raw, progress)

		var out string
		if err != nil {
			out = "$ " + cmd + "\n\n" + err.Error() + "\n\n[error]"
		} else {
			for _, c := range res.Content {
				if tb, ok := c.(provider.TextBlock); ok {
					out += tb.Text
				}
			}
		}
		cancelled := ctx.Err() != nil
		failed := err != nil || res.IsError || cancelled
		if cancelled {
			out += "\n\n[cancelled]"
		}

		if i.agent != nil {
			i.agent.AppendUserContext(out, map[string]string{shellEscapeMetaKey: "true"})
		}

		i.mu.Lock()
		i.shellRunning = false
		i.shellLive = ""
		i.busy = false
		i.cancelTurn = nil
		pendingIdleWork := i.takePendingIdleWorkLocked()
		awaitingPre := i.awaitingStartupPre
		if failed {
			if cancelled {
				i.statusErr = "shell command cancelled"
			} else {
				i.statusErr = "shell command failed"
			}
			i.statusOK = ""
		} else {
			i.statusOK = "shell command finished"
			i.statusErr = ""
		}
		i.mu.Unlock()
		runPendingIdleWork(pendingIdleWork)
		i.invalidate()
		if awaitingPre {
			i.completeStartupPre()
		}
	}()
}

func (i *Interactive) startTurn(parent context.Context, prompt string) {
	i.startTurnWithImages(parent, prompt, nil)
}

func (i *Interactive) startTurnWithImages(parent context.Context, prompt string, images []provider.ImageBlock) {
	i.startTurnRequest(parent, prompt, images, false, false)
}

// startTurnRequest starts a new prompt or continues an existing user turn.
// continueExisting selects Agent.Continue so callers can retry a transcript
// message without appending it twice. overflowRecoveryAttempted suppresses
// the pre-turn threshold guard and the one-shot overflow recovery path; a
// normal post-compaction continuation leaves it false so a still-too-large
// rebuilt context can recover once more.
func (i *Interactive) startTurnRequest(parent context.Context, prompt string, images []provider.ImageBlock, continueExisting, overflowRecoveryAttempted bool) {
	if i.agent == nil {
		// Text startup pre cannot run without credentials; continue so
		// deferred InitialInput (pre-fill or auto-submit) still applies.
		i.mu.Lock()
		awaitingPre := i.awaitingStartupPre
		i.mu.Unlock()
		if awaitingPre {
			i.completeStartupPre()
		}
		return
	}
	// Pre-turn safety: if the most recent context measurement is
	// already past the auto-compact threshold, condense before
	// sending so the next outbound request stays under the limit.
	// The condense flow re-fires the user's queued prompt for us, so
	// we just hand it off and exit.
	i.mu.Lock()
	var resetHandoff json.RawMessage
	var persistResetHandoff bool
	if !continueExisting {
		resetHandoff, persistResetHandoff = i.resetCompactContinuationLocked()
	}
	needsPreCompact := !overflowRecoveryAttempted && !i.autoCompacting && i.shouldAutoCompactLocked()
	if needsPreCompact {
		// Reserve the compaction hand-off before releasing i.mu. A completion
		// update arriving in the small gap before runCompact acquires the
		// mutex must join the host queue rather than starting a competing turn.
		i.busy = true
		i.compacting = true
		i.autoCompacting = true
		if continueExisting {
			i.continueAfterCompact = true
		} else {
			i.pendingCompactPrompt = prompt
			i.pendingCompactImages = append([]provider.ImageBlock(nil), images...)
			i.hasPendingCompactPrompt = true
		}
		i.statusErr = ""
		i.extNotes = append(i.extNotes, autoCompactNoteLine(i.cfg.Theme, "context near limit — condensing history before sending..."))
		i.pendingPostCompactNote = "context auto-compacted; sending your last message"
		i.mu.Unlock()
		if persistResetHandoff {
			i.persistCompactHandoff(resetHandoff)
		}
		i.invalidate()
		i.runCompact(parent, compactContinuationRequest{origin: compactOriginPreTurnThreshold})
		return
	}
	i.mu.Unlock()
	if persistResetHandoff {
		i.persistCompactHandoff(resetHandoff)
	}

	ctx, cancel := context.WithCancel(parent)
	i.mu.Lock()
	i.busy = true
	i.spin.Start()
	i.activity = newAgentActivity(i.cfg.Provider, i.cfg.Model)
	i.cancelTurn = cancel
	i.statusErr = ""
	i.statusOK = ""
	i.streaming.Reset()
	i.streamOn = true
	i.pendingAlert = nil
	i.toolCalls = map[string]*tui.ToolCallView{}
	i.toolOrder = nil
	i.toolGate = map[string]int{}
	i.extNotes = nil   // ext notes are one-shot; a new prompt clears them
	i.scrollOffset = 0 // jump back to the bottom on new turn
	// Lift the resume tail cap once the user starts interacting. The
	// cap is purely a first-paint optimization (don't markdown the
	// whole history before showing anything). Keeping it active during
	// a turn makes the rendered chat a sliding window: appended
	// messages push older ones off the TOP of the buffer, which the
	// renderer must treat as a change above the viewport and repaint
	// fully, snapping the terminal's native scrollback to the bottom on
	// every streamed chunk. A fresh session has no cap (append-only),
	// which is why the jump only shows up in resumed sessions. Dropping
	// the cap here makes resumed turns append-only too.
	i.view.TailLimit = 0
	// Reset the auto-follow baseline so the very next render at
	// interactive.go:1053 doesn't see a synthetic shrink between
	// "last frame had the previous turn's tool overlay" and
	// "this frame had it cleared above". Without this, the guard
	// reads delta = -(rows in cleared overlay) and decrements
	// scrollOffset, which on terminals that mirror zut's pane
	// scroll into the host scrollbar visibly yanks the viewport.
	// See autofollow_shrink_test.go for the exact arithmetic.
	i.prevChatLen = 0
	i.prevChatCols = 0
	i.parkedTurn = 0 // starting a turn clears the /jump parked state
	i.parkedTotal = 0
	i.helpBlock = nil // hide the help block once the user asks something
	i.mu.Unlock()
	i.invalidate()

	var (
		lastStop    provider.StopReason
		lastTurnErr error
	)
	sink := func(ev core.AgentEvent) {
		if e, ok := ev.(core.EvTurnEnd); ok {
			lastStop = e.Stop
			lastTurnErr = e.Err
		}
		i.handleEventForPresentation(ev)
	}

	releaseCompletionHold := i.beginCompletionDeliveryHold()
	go func() {
		defer releaseCompletionHold()
		var err error
		if continueExisting {
			err = i.agent.Continue(ctx, sink)
		} else {
			err = i.agent.Prompt(ctx, prompt, images, sink)
		}
		i.mu.Lock()
		// Keep busy asserted through final cleanup and queue selection. A
		// watcher may enqueue a completion summary concurrently with the
		// provider's final event; publishing idle before inspecting queues can
		// strand that summary in the core agent queue or start an overlapping
		// turn.
		// Don't touch streamPending / streamFlushPending here — the
		// pacer may still be draining the final deltas and needs to
		// paint them even though Prompt has returned. It will reset
		// streamOn on its own once the buffer empties.
		if len(i.streamPending) == 0 {
			i.streamOn = false
		}
		i.cancelTurn = nil
		pendingIdleWork := i.takePendingIdleWorkLocked()
		if err != nil && ctx.Err() == nil {
			i.statusErr = err.Error()
		}
		// Decide whether to offer a model rescue picker for recoverable
		// provider failures (auth/rate/temporary). The picker opens after
		// the mutex is released so it can take its own locks freely.
		var (
			offer       bool
			rescueWhy   string
			rescueImgs  []provider.ImageBlock
			rescueModel string
			rescueProv  string
			rescueFprov string
		)
		if err != nil && ctx.Err() == nil {
			if ok, reason := classifyRescueError(err); ok {
				offer = true
				rescueWhy = reason
				rescueImgs = images
				rescueModel = i.cfg.Model
				rescueProv = i.cfg.Provider
				rescueFprov = extractFailedProvider(err)
				if rescueFprov == "" {
					rescueFprov = i.cfg.Provider
				}
				// Suppress the red banner — the rescue dialog already
				// surfaces the failure.
				i.statusErr = ""
			}
		}
		// Detect responses that reject the current context, either as an
		// HTTP 413 payload limit or a model context-window error. Token-
		// based auto-compact can miss both when metadata is stale or the
		// limit is measured in raw bytes. Compact once, then continue the
		// user message already present in the transcript.
		contextOverflow := err != nil && ctx.Err() == nil && provider.IsContextOverflowError(err)
		recoverContextOverflow := contextOverflow && !overflowRecoveryAttempted
		if recoverContextOverflow {
			i.statusErr = ""
			i.continueAfterCompact = true
			i.extNotes = append(i.extNotes, autoCompactNoteLine(i.cfg.Theme, "request was too large. condensing history before retrying ..."))
			i.pendingPostCompactNote = "context auto-compacted; retrying your last message"
		}
		// Persist the assistant's reply (and every tool row before
		// it) to the session file while the turn memory is hot.
		// Without this, WriteNewTranscript only fires at zut exit,
		// meaning a crash or ungraceful kill drops the whole
		// conversation. FlushSession is idempotent (it advances the
		// baseline so subsequent flushes only write new rows).
		flush := i.cfg.FlushSession
		i.mu.Unlock()
		runPendingIdleWork(pendingIdleWork)
		if flush != nil {
			flush()
		}
		terminalGoalError := ctx.Err() == nil && !offer && !recoverContextOverflow && (err != nil || lastTurnErr != nil || lastStop == provider.StopError)
		if terminalGoalError {
			i.updateActiveGoal(core.GoalBlocked, "turn ended with an error")
		}
		goalMessage, goalActive := i.goalContinuationMessage()
		i.mu.Lock()
		awaitingPre := i.awaitingStartupPre
		// A newer explicit prompt may have cleared the handoff while the
		// completed turn was being flushed. Re-read it under the mutex before
		// deciding whether this continuation may spend another rescue attempt.
		statusRescueActive := i.compactContinuation.reason == compactContinuationStatusRescue
		// Pop the next queued message, if any, and relaunch.
		var next string
		var hasNext bool
		if !awaitingPre && len(i.queued) > 0 && ctx.Err() == nil && err == nil {
			next, i.queued = i.queued[0], i.queued[1:]
			hasNext = true
		}
		// If the turn was cancelled or errored, drop the queue so the
		// user isn't bombarded with stale messages after an interrupt.
		if ctx.Err() != nil || (err != nil && !recoverContextOverflow) {
			i.queued = nil
			if i.agent != nil {
				i.agent.DrainQueuedMessages()
			}
		}
		// Decide whether the next thing to do is an auto-compaction.
		// Only fires when the turn completed cleanly AND no host-side
		// or agent-side queued messages are waiting (otherwise a queued
		// message would race the condense).
		agentQueued := 0
		if i.agent != nil {
			agentQueued = i.agent.QueuedMessageCount()
		}
		continueQueued := !awaitingPre && !hasNext && agentQueued > 0 && err == nil && ctx.Err() == nil
		shouldAutoCompact := !awaitingPre && !hasNext && agentQueued == 0 && err == nil && ctx.Err() == nil && i.shouldAutoCompactLocked()
		continueStatusRescue := false
		var handoff json.RawMessage
		var persistHandoff bool
		if statusRescueActive && i.agent != nil && !awaitingPre && !hasNext && !continueQueued && !shouldAutoCompact && err == nil && ctx.Err() == nil && lastStop == provider.StopEnd && lastTurnErr == nil {
			followUpMessages := i.agent.Messages()
			reason := classifyCompactionContinuation(compactOriginManual, true, lastStop, lastTurnErr, followUpMessages)
			if reason == compactContinuationStatusRescue && i.compactContinuation.rescueAttempts < maxStatusRescueContinuations {
				handoff, persistHandoff = i.setCompactContinuationLocked(compactContinuationState{
					reason:         compactContinuationStatusRescue,
					rescueAttempts: i.compactContinuation.rescueAttempts + 1,
				})
				continueStatusRescue = true
			} else {
				handoff, persistHandoff = i.resetCompactContinuationLocked()
			}
		}
		if !continueStatusRescue && (i.agent == nil || ctx.Err() != nil || err != nil || awaitingPre || hasNext || continueQueued || offer || recoverContextOverflow || (!shouldAutoCompact && !statusRescueActive)) {
			handoff, persistHandoff = i.resetCompactContinuationLocked()
		}
		continueGoal := goalActive && !awaitingPre && !hasNext && !continueQueued && !continueStatusRescue && !offer && !recoverContextOverflow && !shouldAutoCompact && err == nil && ctx.Err() == nil && lastStop == provider.StopEnd && lastTurnErr == nil
		// The agent run can finish before the paced final text reaches the
		// transcript. A compaction replaces that transcript, so it must never
		// race the still-live stream frame; otherwise stale deltas can repaint
		// after the replacement and corrupt the scrollback renderer's model.
		if recoverContextOverflow || shouldAutoCompact {
			i.resetStreamingStateLocked()
		}
		alertReason := mainAlertReason(ctx, err, lastTurnErr, lastStop, awaitingPre, hasNext || agentQueued > 0 || continueGoal, offer, recoverContextOverflow, shouldAutoCompact)
		i.busy = hasNext || continueQueued || continueStatusRescue || continueGoal || recoverContextOverflow || shouldAutoCompact
		i.mu.Unlock()
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
		if alertReason != "" {
			i.scheduleMainAlert(alertReason)
		}
		i.invalidate()
		parent := i.runCtx
		if parent == nil {
			parent = context.Background()
		}
		if awaitingPre {
			i.completeStartupPre()
			return
		}
		switch {
		case hasNext:
			i.startTurn(parent, next)
		case continueQueued:
			i.startTurnRequest(parent, "", nil, true, false)
		case continueStatusRescue:
			i.startAutoCompactContinuation(parent)
		case continueGoal:
			i.startGoalContinuation(parent, goalMessage)
		case offer:
			i.openRescueDialog(rescueProv, rescueFprov, rescueModel, rescueWhy, prompt, rescueImgs)
		case recoverContextOverflow:
			i.runCompact(parent, compactContinuationRequest{origin: compactOriginRecovery})
		case shouldAutoCompact:
			i.runCompact(parent, compactContinuationRequest{
				origin:    compactOriginAfterTurnThreshold,
				force:     lastStop == provider.StopLength,
				lastStop:  lastStop,
				turnError: lastTurnErr,
			})
		}
	}()
}

func mainAlertReason(ctx context.Context, err, turnErr error, stop provider.StopReason, awaitingPre, hasNext, rescue, recovering, autoCompacting bool) string {
	if awaitingPre || ctx.Err() != nil || hasNext || recovering || autoCompacting {
		return ""
	}
	if rescue {
		return "rescue_required"
	}
	if stop == provider.StopAborted {
		return ""
	}
	if err != nil || turnErr != nil || stop == provider.StopError {
		return "agent_error"
	}
	if stop == provider.StopLength {
		return "response_truncated"
	}
	return "agent_done"
}

// openRescueDialog surfaces the rescue model picker after a recoverable
// provider failure. The pending prompt + images are stashed on the
// Interactive so a later applyRescueSelection can re-run the same turn
// against the freshly-picked model. activeProvider/failedProvider are
// usually the same, but some clients embed different prefixes in their
// errors than the configured provider id, so we accept both.
func (i *Interactive) openRescueDialog(activeProvider, failedProvider, failedModel, reason, prompt string, images []provider.ImageBlock) {
	if i.rescueDialog == nil {
		return
	}
	loggedIn := []string{}
	if i.cfg.LoggedInProviders != nil {
		loggedIn = i.cfg.LoggedInProviders()
	}
	fprov := failedProvider
	if fprov == "" {
		fprov = activeProvider
	}
	i.mu.Lock()
	i.pendingRescuePrompt = prompt
	i.pendingRescueImages = images
	i.mu.Unlock()
	i.rescueDialog.Open(failedModel, loggedIn, fprov, failedModel, reason, prompt)
	i.invalidate()
}

// applyRescueSelection switches model (cross-provider if needed) and
// re-runs the same prompt+images that just failed. Mirrors
// applyModelSelection's transcript-carry logic so the user keeps full
// session continuity across the swap.
func (i *Interactive) applyRescueSelection(prov, model, prompt string) {
	if model == "" {
		return
	}
	i.applyRescueModelSelection(prov, model)
	i.mu.Lock()
	images := i.pendingRescueImages
	if prompt == "" {
		prompt = i.pendingRescuePrompt
	}
	i.pendingRescuePrompt = ""
	i.pendingRescueImages = nil
	i.mu.Unlock()
	parent := i.runCtx
	if parent == nil {
		parent = context.Background()
	}
	i.startTurnWithImages(parent, prompt, images)
}

func stripAutoCompactNotes(notes []string) []string {
	if len(notes) == 0 {
		return notes
	}
	out := notes[:0]
	for _, n := range notes {
		if strings.Contains(n, "condensing history") {
			continue
		}
		out = append(out, n)
	}
	return out
}

// autoCompactNoteLine returns a styled chat-area note for the
// inline auto-compact heads-up. Lives in extNotes so it survives
// the busy-spinner overwrite of the status row.
func autoCompactNoteLine(th tui.Theme, msg string) string {
	return "  " + th.FGColor(th.Warning, "⚠ "+msg)
}

// NormalizeAutoCompactThreshold applies the persisted auto-compaction
// setting's supported values and default. It is exported so non-interactive
// hosts can share the interactive mode's exact configuration semantics.
func NormalizeAutoCompactThreshold(threshold *int) int {
	const defaultAutoCompactThreshold = 85
	if threshold == nil {
		return defaultAutoCompactThreshold
	}
	switch *threshold {
	case 0, 70, 80, 85, 90, 95:
		return *threshold
	default:
		return defaultAutoCompactThreshold
	}
}

// ShouldAutoCompact reports whether the latest request context reached the
// configured percentage of the model's advertised context window.
func ShouldAutoCompact(inputTokens, contextWindow, thresholdPercent int) bool {
	if inputTokens <= 0 || contextWindow <= 0 || thresholdPercent <= 0 {
		return false
	}
	return float64(inputTokens)/float64(contextWindow) >= float64(thresholdPercent)/100
}

// classifyCompactionContinuation chooses a private handoff reason from the
// successful turn tail. Structural unfinished-tail handling is independent of
// the status matcher. A status rescue is admitted only for an after-turn
// threshold compaction or an already-active status handoff, and only after a
// normal successful end.
func classifyCompactionContinuation(origin compactContinuationOrigin, statusRescueActive bool, stop provider.StopReason, turnErr error, msgs []provider.Message) compactContinuationReason {
	if origin != compactOriginManual && structuralCompactionContinuation(msgs) {
		return compactContinuationStructuralTail
	}
	if (origin != compactOriginAfterTurnThreshold && !statusRescueActive) || stop != provider.StopEnd || turnErr != nil {
		return compactContinuationNone
	}
	if likelyForwardWorkStatus(msgs) {
		return compactContinuationStatusRescue
	}
	return compactContinuationNone
}

// structuralCompactionContinuation preserves the pre-existing unfinished-tail
// behavior without consulting the status matcher.
func structuralCompactionContinuation(msgs []provider.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	if last.Role != provider.RoleAssistant {
		return true
	}
	hasText := false
	for _, content := range last.Content {
		switch block := content.(type) {
		case provider.ToolCallBlock:
			return true
		case provider.TextBlock:
			if strings.TrimSpace(block.Text) != "" {
				hasText = true
			}
		case provider.ReasoningBlock:
			// Reasoning without a visible answer is not terminal.
		default:
			return true
		}
	}
	return !hasText
}

// likelyForwardWorkStatus is deliberately narrow. It recognizes only clear
// first-person future-work commitments and never treats generic plan prose as
// a host control signal.
func likelyForwardWorkStatus(msgs []provider.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	if last.Role != provider.RoleAssistant {
		return false
	}
	var text strings.Builder
	for _, content := range last.Content {
		block, ok := content.(provider.TextBlock)
		if !ok {
			if _, hasTool := content.(provider.ToolCallBlock); hasTool {
				return false
			}
			continue
		}
		if strings.TrimSpace(block.Text) == "" {
			continue
		}
		if text.Len() > 0 {
			text.WriteByte(' ')
		}
		text.WriteString(block.Text)
	}
	if text.Len() == 0 {
		return false
	}
	normalized := strings.ToLower(strings.Join(strings.Fields(text.String()), " "))
	normalized = strings.NewReplacer("’", "'", "‘", "'", "`", "'").Replace(normalized)
	for _, phrase := range []string{
		"next i will",
		"next, i will",
		"i'll now",
		"i will now",
		"i am going to",
		"i need to continue",
		"then i will",
		"then, i will",
		"i will proceed",
	} {
		if containsFutureWorkPhrase(normalized, phrase) {
			return true
		}
	}
	return false
}

func containsFutureWorkPhrase(text, phrase string) bool {
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], phrase)
		if index < 0 {
			return false
		}
		index += offset
		end := index + len(phrase)
		if !wordRuneBefore(text, index) && !wordRuneAfter(text, end) {
			return true
		}
		offset = end
	}
	return false
}

func wordRuneBefore(text string, index int) bool {
	if index == 0 {
		return false
	}
	rune_, _ := utf8.DecodeLastRuneInString(text[:index])
	return unicode.IsLetter(rune_) || unicode.IsDigit(rune_) || rune_ == '_'
}

func wordRuneAfter(text string, index int) bool {
	if index == len(text) {
		return false
	}
	rune_, _ := utf8.DecodeRuneInString(text[index:])
	return unicode.IsLetter(rune_) || unicode.IsDigit(rune_) || rune_ == '_'
}

// shouldAutoCompactLocked reports whether the last turn pushed context
// usage past the auto-compact threshold. Must be called with i.mu
// held; it reads lastCtxInput and the current model's context window.
func (i *Interactive) shouldAutoCompactLocked() bool {
	if i.agent == nil {
		return false
	}
	if i.autoCompacting {
		return false
	}
	m, err := provider.FindModel(i.cfg.Provider, i.cfg.Model)
	if err != nil || m.ContextWindow <= 0 {
		return false
	}
	threshold := NormalizeAutoCompactThreshold(i.cfg.AutoCompactThreshold)
	return ShouldAutoCompact(i.lastCtxInput, m.ContextWindow, threshold)
}

func eventAffectsPresentation(ev core.AgentEvent) bool {
	switch ev.(type) {
	case core.EvToolProgress:
		return false
	default:
		return true
	}
}

func (i *Interactive) bumpToolRevisionLocked(tc *tui.ToolCallView) {
	i.toolRenderRevision++
	if i.toolRenderRevision == 0 {
		// Overflow is practically unreachable, but resetting the sequence must
		// also drop old revisions before they can alias new frames.
		if i.view != nil {
			i.view.InvalidateRenderCache()
		}
		i.toolRenderRevision = 1
	}
	tc.Revision = i.toolRenderRevision
}

func (i *Interactive) handleEventForPresentation(ev core.AgentEvent) {
	i.handleEvent(ev)
	// Progress payloads are still delivered to the core sink and applied
	// in order, but they have no interactive-visible representation.
	if eventAffectsPresentation(ev) {
		i.invalidate()
	}
}

func (i *Interactive) handleEvent(ev core.AgentEvent) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.activity.apply(ev)
	switch e := ev.(type) {
	case core.EvAssistantStart:
		// Fires at the top of every oneTurn, including follow-up
		// turns after tool use. Without this, the streaming buffer
		// is still marked off from the previous assistant message
		// and the final summary text pops in all at once instead
		// of typewriter-streaming delta by delta.
		i.streaming.Reset()
		i.streamPending = i.streamPending[:0]
		i.streamFlushPending = false
		i.streamOn = true
		// Clear the live tool-call overlay. Any tools from the
		// previous round are now fully folded into the transcript
		// (assistant tool_use block + tool role message with the
		// result), so keeping them in the overlay would duplicate
		// them in the view — once inside the finalised transcript
		// and once below the streaming block, with the streaming
		// summary sandwiched in between. The next EvToolUseStart
		// will populate fresh entries for this turn's tools.
		i.toolCalls = map[string]*tui.ToolCallView{}
		i.toolOrder = nil
		i.toolGate = map[string]int{}
	case core.EvTextDelta:
		// Buffer into streamPending; the paintPace ticker drains
		// it into i.streaming a few runes at a time for a smooth
		// typewriter effect independent of upstream chunk size.
		i.streamPending = append(i.streamPending, []rune(e.Delta)...)
		i.streamOn = true
	case core.EvAssistantMessage:
		// OnAssistant + telegram mirroring always fire on message
		// arrival — they read the FINAL message content, which is
		// complete regardless of what's still in the pacer.
		i.assistantMessageSideEffects(e.Message)
		// If the pacer still has characters to drain, keep streamOn
		// true and mark flush pending; the paintPace ticker will
		// drain the remainder and reset streaming state when done.
		// Otherwise (rare: full-replay sessions, abort paths) clear
		// synchronously so a later render doesn't show stale text.
		if len(i.streamPending) > 0 {
			i.streamFlushPending = true
			return
		}
		i.resetStreamingStateLocked()
	case core.EvToolUseStart:
		// Live streaming: pre-create the view so the user sees the
		// tool call being composed in real time. Any subsequent
		// EvToolCall for the same ID updates the same struct (the
		// final parsed args + name are already known here).
		if _, exists := i.toolCalls[e.ID]; !exists {
			tc := &tui.ToolCallView{
				ID:        e.ID,
				Name:      e.Name,
				Streaming: true,
			}
			i.bumpToolRevisionLocked(tc)
			i.toolCalls[e.ID] = tc
			i.toolOrder = append(i.toolOrder, e.ID)
			i.gateToolLocked(e.ID)
		}
	case core.EvToolUseArgs:
		if tc, ok := i.toolCalls[e.ID]; ok {
			tc.RawJSONBuf += e.Delta
			i.bumpToolRevisionLocked(tc)
			// Refresh the live path as soon as it parses; used in
			// the header (write /Users/example/Desktop/demo.ts)
			// while the content is still streaming.
			if p, pok, _ := tui.ExtractPartialStringField(tc.RawJSONBuf, "path"); pok {
				tc.LivePath = p
			} else if p, pok, _ := tui.ExtractPartialStringField(tc.RawJSONBuf, "file_path"); pok {
				tc.LivePath = p
			}
		}
	case core.EvToolUseEnd:
		if tc, ok := i.toolCalls[e.ID]; ok {
			tc.Streaming = false
			i.bumpToolRevisionLocked(tc)
		}
	case core.EvToolCall:
		// If we already pre-created the view during streaming, just
		// refresh the final Args summary. Otherwise create a new one
		// (non-streaming providers or legacy paths).
		if tc, ok := i.toolCalls[e.ID]; ok {
			tc.Args = tui.ShortArgs(e.Name, e.Args)
			i.bumpToolRevisionLocked(tc)
			if tc.RawJSONBuf == "" {
				tc.RawJSONBuf = string(e.Args)
			}
			tc.Streaming = false
		} else {
			tc := &tui.ToolCallView{
				ID:         e.ID,
				Name:       e.Name,
				Args:       tui.ShortArgs(e.Name, e.Args),
				RawJSONBuf: string(e.Args),
			}
			i.bumpToolRevisionLocked(tc)
			i.toolCalls[e.ID] = tc
			i.toolOrder = append(i.toolOrder, e.ID)
			i.gateToolLocked(e.ID)
		}
	case core.EvToolResult:
		if tc, ok := i.toolCalls[e.ID]; ok {
			tc.Done = true
			tc.Error = e.Result.IsError
			tc.Preview = ""
			var text strings.Builder
			for _, c := range e.Result.Content {
				if tb, ok := c.(provider.TextBlock); ok {
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(tb.Text)
				}
			}
			tc.Result = text.String()
			i.bumpToolRevisionLocked(tc)
		}
		if update, ok := tools.GoalUpdateFromResult(e.Result); ok {
			if update.Status == core.GoalActive {
				// A manager may advance a terminal goal to the next persisted
				// goal in the same mission. Do not let a late tool result resume
				// a goal explicitly paused by the user.
				if i.goalStatus != core.GoalPaused {
					i.goalStatus = core.GoalActive
				}
			} else if i.goalStatus == core.GoalActive {
				i.goalStatus = update.Status
			}
		}
	case core.EvUsage:
		i.cumUsage = e.Cumulative
		if contextUsed := e.Usage.InputTokens + e.Usage.CacheReadTokens + e.Usage.CacheWriteTokens; contextUsed > 0 {
			i.lastCtxInput = contextUsed
		}
	case core.EvTurnEnd:
		if e.Stop == provider.StopAborted {
			i.resetStreamingStateLocked()
			i.statusErr = ""
			i.statusOK = "cancelled"
			return
		}
		if e.Stop == provider.StopLength {
			// The model hit its output-token cap mid-response, so the
			// reply (often a long write/edit) is truncated. Surface it
			// explicitly, otherwise the turn just ends and reads like
			// the UI gave up. The agent already requests the model's
			// full MaxOutput budget, so this means the response genuinely
			// exceeded that ceiling; ask the user to continue.
			i.statusErr = "response hit the model's output-token limit and was cut off, ask it to continue"
			i.statusOK = ""
			return
		}
		// Don't surface mid-loop stream errors as a red banner here.
		// EvTurnEnd fires after every step in a multi-step tool loop,
		// so a transient 503 / network blip would briefly paint a red
		// banner over the still-streaming chat before the agent loop
		// either retries or exits. The final error (if any) is set by
		// startTurnWithImages once Prompt() returns, and recoverable
		// failures are routed to the rescue picker instead — which
		// keeps the chat clean while the agent is working.
		_ = e.Err
	}
}

// WebSearchPolicyGeneration returns the generation a resolver must present at
// its final interactive commit. Policy transitions advance it before changing
// the gate or generic-child ceiling.
func (i *Interactive) WebSearchPolicyGeneration() uint64 {
	return i.webSearchPolicyGeneration.Load()
}

// ApplyAgentPromptConfig commits a resolved prompt and tool registry only if
// ag is still the live interactive agent. Callers without a long-running
// resolve use the current policy generation.
func (i *Interactive) ApplyAgentPromptConfig(ag *core.Agent, system string, tools core.Registry) (core.Registry, bool) {
	return i.applyAgentPromptConfig(ag, system, tools, 0, false)
}

// ApplyAgentPromptConfigAtWebSearchGeneration additionally rejects a registry
// resolved before a web-search policy transition. This final check is made in
// the same critical section as live-agent replacement and the core prompt swap.
func (i *Interactive) ApplyAgentPromptConfigAtWebSearchGeneration(ag *core.Agent, system string, tools core.Registry, generation uint64) (core.Registry, bool) {
	return i.applyAgentPromptConfig(ag, system, tools, generation, true)
}

func (i *Interactive) applyAgentPromptConfig(ag *core.Agent, system string, tools core.Registry, generation uint64, checkGeneration bool) (core.Registry, bool) {
	if ag == nil {
		return nil, false
	}
	// Match the lock order used by dynamic tool mutations so a refresh
	// cannot race a snapshot-and-replace operation that would reintroduce
	// the old registry after the atomic prompt swap.
	i.agentMu.Lock()
	defer i.agentMu.Unlock()
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.agent != ag || checkGeneration && i.webSearchPolicyGeneration.Load() != generation {
		return nil, false
	}
	if i.telegramBridge != nil {
		// Telegram prompts arrive over an external messaging channel without a
		// per-request confirmation surface. Keep the normal interactive
		// registry from reintroducing web_search from the moment the bridge is
		// attached, including while its startup handshake is in flight.
		delete(tools, "web_search")
	}
	oldTools := ag.SetPromptConfig(system, tools)
	_, webSearchAvailable := tools["web_search"]
	i.setWebSearchAvailable(webSearchAvailable)
	return oldTools, true
}

// prepareReplacementAgentLocked applies session-wide capability policy before
// ag becomes visible to prompt submission. The caller holds agentMu and i.mu.
func (i *Interactive) prepareReplacementAgentLocked(ag *core.Agent) {
	if ag == nil {
		i.setWebSearchAvailable(false)
		return
	}
	registry := ag.ToolsSnapshot()
	if i.telegramBridge != nil {
		// External Telegram prompts may arrive as soon as the replacement is
		// published, so post-swap cleanup is too late.
		delete(registry, "web_search")
		ag.SetTools(registry)
	}
	_, webSearchAvailable := registry["web_search"]
	i.setWebSearchAvailable(webSearchAvailable && i.telegramBridge == nil)
}

// DeferUntilIdle runs work immediately when the interactive agent is idle,
// or after the current turn/compaction/shell operation releases the busy
// state. Callbacks run outside i.mu.
func (i *Interactive) DeferUntilIdle(fn func()) {
	if fn == nil {
		return
	}
	i.mu.Lock()
	if i.busy {
		i.pendingIdleWork = append(i.pendingIdleWork, fn)
		i.mu.Unlock()
		return
	}
	i.mu.Unlock()
	fn()
}

func (i *Interactive) takePendingIdleWorkLocked() []func() {
	work := i.pendingIdleWork
	i.pendingIdleWork = nil
	return work
}

func runPendingIdleWork(work []func()) {
	for _, fn := range work {
		if fn != nil {
			fn()
		}
	}
}

// Agent returns the current agent, if any. Used by cli.go to flush the
// final transcript to the session file.
func (i *Interactive) Agent() *core.Agent {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.agent
}

// silence unused import in some build configs
var _ = fmt.Sprintf

// runReloadExt triggers a live reload of every extension (discovered
// + explicit). Runs on a goroutine so the TUI stays responsive; the
// Manager.Reload takes a couple of hundred ms to shut down subprocs
// and respawn them. Shows a temporary status line throughout.
func (i *Interactive) runReloadExt(ctx context.Context) {
	if i.cfg.Extensions == nil {
		i.mu.Lock()
		i.statusErr = "no extension manager in this build"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.mu.Lock()
	i.statusOK = "Reloading extensions..."
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()

	go func() {
		stats := i.cfg.Extensions.Reload(ctx, 2*time.Second)
		msg, failed := formatReloadStatus(stats)
		seq := i.setReloadStatus(msg, failed)
		i.invalidate()
		go i.dismissReloadStatus(ctx, seq, msg, failed)
	}()
}

// Confirm implements core.Confirmer. The agent goroutine calls
// this synchronously before every tool invocation when --no-yolo is
// active. We push the request onto the confirmDialog queue, trigger
// a redraw, and block the caller until the user answers.
//
// If the session is cancelled or the TUI exits mid-prompt, any
// pending request is refused via CancelAll so the agent doesn't
// deadlock.
func (i *Interactive) Confirm(toolName string, preview string) core.ConfirmDecision {
	return i.ConfirmToolCall(core.ToolCallConfirmation{Name: toolName, Summary: preview})
}

// ConfirmToolCall attaches a side-effect-free preview to the matching live
// tool panel, then blocks until the user approves or refuses the call.
func (i *Interactive) ConfirmToolCall(call core.ToolCallConfirmation) core.ConfirmDecision {
	resp := make(chan core.ConfirmDecision, 1)
	if isBtwOrigin(call.Origin) {
		req := &confirmRequest{
			toolName:      call.Name,
			preview:       call.Summary,
			resp:          resp,
			returnToChild: true,
		}
		if call.ID == "" || i.btwDialog == nil || !i.btwDialog.enqueueToolConfirmation(call.Origin, call.ID, call.Summary, call.Content, func() {
			i.confirmDialog.Enqueue(req)
		}) {
			return core.ConfirmDecision{Allow: false, Reason: "side-chat tool call canceled"}
		}
		i.invalidate()
		return <-resp
	}
	if call.ID != "" {
		i.mu.Lock()
		if tc, ok := i.toolCalls[call.ID]; ok {
			tc.Args = call.Summary
			tc.Preview = call.Content
			i.activity.activity = activity{kind: activityAwaitingConfirmation, tool: call.Name, provider: i.cfg.Provider, model: i.cfg.Model}
		}
		i.mu.Unlock()
	}
	i.confirmDialog.Enqueue(&confirmRequest{
		toolName: call.Name,
		preview:  call.Summary,
		resp:     resp,
	})
	i.invalidate()
	return <-resp
}

// openTelegramDialog shows the picker for `/telegram` with no arg.
// Items depend on current state: disconnect + status when running,
// connect + status when stopped.
func (i *Interactive) openTelegramDialog() {
	items := i.telegramMenuItems()
	if len(items) == 0 {
		i.mu.Lock()
		i.statusErr = "telegram not configured. run `zut telegram-bot setup` first."
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.telegramDialog.Open(items)
	i.invalidate()
}

// telegramMenuItems builds the dialog entries for the current
// bridge state. Returns empty when no bot.json exists so the
// caller can show a helpful status line instead of an empty menu.
func (i *Interactive) telegramMenuItems() []telegramItem {
	cfg, err := telegram.LoadConfig(i.cfg.ZutHome)
	if err != nil || cfg.BotToken == "" {
		return nil
	}
	var items []telegramItem
	if i.telegramBridge != nil && i.telegramBridge.Active() {
		items = append(items, telegramItem{label: "disconnect", action: "disconnect", hint: "stop mirroring"})
		st := i.telegramBridge.State()
		hint := "active"
		if st.Username != "" {
			hint += " as @" + st.Username
		}
		items = append(items, telegramItem{label: "status", action: "status", hint: hint})
	} else {
		label := "connect"
		hint := "start mirroring dms into this session"
		if cfg.AllowedUserID == 0 {
			hint = "awaiting pairing (send /start to the bot once connected)"
		}
		items = append(items, telegramItem{label: label, action: "connect", hint: hint})
		items = append(items, telegramItem{label: "status", action: "status", hint: "disconnected"})
	}
	return items
}

// doTelegram dispatches one of the three explicit actions. Called
// from /telegram <action> or after the picker selects a row.
func (i *Interactive) doTelegram(action string) {
	switch action {
	case "connect":
		i.telegramConnect()
	case "disconnect":
		i.telegramDisconnect()
	case "status":
		i.telegramStatus()
	default:
		i.mu.Lock()
		i.statusErr = "unknown telegram action: " + action + " (use connect, disconnect, or status)"
		i.mu.Unlock()
		i.invalidate()
	}
}

// telegramConnect starts the bridge. Refuses if it's already
// running or if the on-disk bot.json is missing a token.
func (i *Interactive) telegramConnect() {
	if i.telegramBridge != nil && i.telegramBridge.Active() {
		i.mu.Lock()
		i.statusOK = "telegram already connected"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	cfg, err := telegram.LoadConfig(i.cfg.ZutHome)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "telegram: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if cfg.BotToken == "" {
		i.mu.Lock()
		i.statusErr = "telegram: no bot token configured. run `zut telegram-bot setup` first."
		i.mu.Unlock()
		i.invalidate()
		return
	}
	// Refuse to start when a background daemon is already polling
	// the same bot. Two concurrent long-poll consumers race each
	// update and one always loses, so DMs get half-delivered. The
	// user can `zut telegram-bot stop` first, then /telegram connect.
	if pid, alive, _ := telegram.IsRunning(i.cfg.ZutHome); alive && pid > 0 {
		i.mu.Lock()
		i.statusErr = fmt.Sprintf("telegram: bot daemon already running (pid %d). stop it with `zut telegram-bot stop` first.", pid)
		i.mu.Unlock()
		i.invalidate()
		return
	}
	bridge := &telegram.Bridge{
		Client: telegram.NewClient(cfg.BotToken),
		Config: cfg,
		Save: func(next telegram.Config) error {
			return telegram.SaveConfig(i.cfg.ZutHome, next)
		},
		Host: &telegramHost{iv: i},
	}
	i.mu.Lock()
	i.telegramBridge = bridge
	i.mu.Unlock()
	// Strip web_search before the bridge's startup handshake can accept a
	// Telegram update. ApplyAgentPromptConfig also keeps it stripped from
	// concurrent refreshes while this bridge pointer is attached.
	i.applyTelegramTools(true)
	if err := bridge.Start(i.runCtx); err != nil {
		i.mu.Lock()
		i.telegramBridge = nil
		i.mu.Unlock()
		refreshErr := i.refreshToolsAfterTelegram()
		i.mu.Lock()
		i.statusErr = "telegram connect failed: " + err.Error()
		if refreshErr != nil {
			i.statusErr += "; tool refresh: " + refreshErr.Error()
		}
		i.mu.Unlock()
		i.invalidate()
		return
	}
	state := bridge.State()
	label := "telegram connected"
	if state.Username != "" {
		label += " as @" + state.Username
	}
	if state.PairedID == 0 {
		label += " — send /start to the bot from your phone to claim it"
	}
	i.mu.Lock()
	i.statusOK = label
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

// telegramDisconnect stops the bridge. No-op when already stopped.
func (i *Interactive) telegramDisconnect() {
	if i.telegramBridge == nil || !i.telegramBridge.Active() {
		i.mu.Lock()
		i.statusOK = "telegram already disconnected"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	bridge := i.telegramBridge
	bridge.Stop()
	i.mu.Lock()
	// Clear the pointer before rebuilding the normal registry. This both
	// allows web_search back into the refresh and marks the bridge inactive
	// for any concurrent prompt-config commit.
	i.telegramBridge = nil
	i.mu.Unlock()
	refreshErr := i.refreshToolsAfterTelegram()
	i.mu.Lock()
	if refreshErr != nil {
		i.statusOK = ""
		i.statusErr = "telegram disconnect tool refresh: " + refreshErr.Error()
	} else {
		i.statusOK = "telegram disconnected"
		i.statusErr = ""
	}
	i.mu.Unlock()
	i.invalidate()
}

// refreshToolsAfterTelegram restores normal tools only through a successful
// resolver refresh. Failure removes Telegram-only tools but leaves web_search
// revoked in the live registry, stale snapshots, and generic-child policy.
func (i *Interactive) refreshToolsAfterTelegram() error {
	if i.cfg.RefreshTools == nil {
		i.stripWebSearchTool()
		i.applyTelegramTools(false)
		return errors.New("live tool refresh is unavailable")
	}
	if err := i.cfg.RefreshTools(); err != nil {
		i.stripWebSearchTool()
		i.applyTelegramTools(false)
		return err
	}
	i.applyTelegramTools(false)
	return nil
}

// telegramSenderAdapter wraps the bridge so the tools package can
// drive it without importing telegram directly. The Active() check
// is forwarded to the bridge so the tool can fail clearly with a
// model-readable error when the user disconnected mid-turn.
type telegramSenderAdapter struct {
	bridge *telegram.Bridge
}

func (a telegramSenderAdapter) SendImage(ctx context.Context, path, caption string) error {
	if a.bridge == nil {
		return fmt.Errorf("telegram bridge is not connected")
	}
	return a.bridge.SendImage(ctx, path, caption)
}

func (a telegramSenderAdapter) SendDocument(ctx context.Context, path, caption string) error {
	if a.bridge == nil {
		return fmt.Errorf("telegram bridge is not connected")
	}
	return a.bridge.SendDocument(ctx, path, caption)
}

func (a telegramSenderAdapter) Active() bool {
	return a.bridge != nil && a.bridge.Active()
}

// TrackSubagentWorker is the exported entry point used by the cli to
// hand a freshly-spawned auto-subagents agent off to the shared tracker.
func (i *Interactive) TrackSubagentWorker(a *subagents.Agent, task string, _ bool) {
	i.trackSubagentWorker(a, task, false, false)
}

// TrackResumedSubagentWorker watches a resumed follow-up independently of the
// worker's original task. A long-lived daemon can complete several manager
// turns, each of which must produce its own automatic delivery.
func (i *Interactive) TrackResumedSubagentWorker(a *subagents.Agent, prompt string, _ bool) {
	i.trackSubagentWorker(a, prompt, true, false)
}

// PrepareResumedSubagentWorker registers a resumed turn before its prompt is
// sent. The returned cleanup removes the registration if the resume operation
// fails before delivery. It is used by the resume tool's pre-send hook; the
// existing TrackResumedSubagentWorker signature remains the post-success API.
func (i *Interactive) PrepareResumedSubagentWorker(a *subagents.Agent, prompt string, _ bool) func() {
	return i.trackSubagentWorker(a, prompt, true, true)
}

// TrackStoppedSubagentWorker watches a requested worker shutdown. If an active
// task watcher already owns the worker, the shared tracker leaves that watcher
// responsible for the terminal outcome.
func (i *Interactive) TrackStoppedSubagentWorker(a *subagents.Agent) {
	if i == nil || a == nil {
		return
	}
	tracker := i.ensureCompletionTracker()
	i.registerCoordinatorWorker(a.ID)
	tracker.TrackExit(a, a.Task)
	i.invalidate()
	i.requestCompletionDelivery()
}

func (i *Interactive) trackSubagentWorker(a *subagents.Agent, task string, followUp, future bool) func() {
	if i == nil || a == nil {
		return nil
	}
	tracker := i.ensureCompletionTracker()
	i.registerCoordinatorWorker(a.ID)
	var cancel func()
	if future {
		cancel = tracker.TrackFutureTurn(a, task, followUp)
	} else {
		cancel = tracker.TrackTurn(a, task, followUp)
	}
	i.invalidate()
	i.requestCompletionDelivery()
	return cancel
}

func (i *Interactive) ensureCompletionTracker() *subagents.CompletionTracker {
	// Tests and lightweight embedders may construct Interactive with a struct
	// literal instead of NewInteractive, so retain this lazy fallback.
	i.completionDeliveryMu.Lock()
	defer i.completionDeliveryMu.Unlock()
	if i.completionTracker == nil {
		i.completionTracker = subagents.NewCompletionTracker()
	}
	return i.completionTracker
}

func (i *Interactive) ensureCoordinatorLocked() *orchestration.Coordinator {
	if i.turnCoordinator == nil {
		i.turnCoordinator = orchestration.New()
	}
	return i.turnCoordinator
}

func (i *Interactive) coordinatorAcceptsUserInput() bool {
	if i == nil {
		return false
	}
	i.completionDeliveryMu.Lock()
	accepts := i.ensureCoordinatorLocked().AcceptsUserInput()
	i.completionDeliveryMu.Unlock()
	return accepts
}

func (i *Interactive) applyCoordinator(event orchestration.Event) []orchestration.Action {
	if i == nil {
		return nil
	}
	i.completionDeliveryMu.Lock()
	result := i.ensureCoordinatorLocked().Apply(event)
	i.completionDeliveryMu.Unlock()
	return result.Actions
}

// registerCoordinatorWorker associates a tracker registration with the open
// manager wave. Direct embedders that register a worker outside startTurn get
// a one-worker sealed wave, retaining the exported tracker API's old behavior.
func (i *Interactive) registerCoordinatorWorker(workerID string) {
	if i == nil || workerID == "" {
		return
	}
	i.completionDeliveryMu.Lock()
	coordinator := i.ensureCoordinatorLocked()
	implicitWave := i.completionDeliveryHolds == 0
	if implicitWave {
		coordinator.Apply(orchestration.Event{Kind: orchestration.EventManagerStarted})
	}
	i.coordinatorWorkerSeq++
	registrationID := fmt.Sprintf("%s#%d", workerID, i.coordinatorWorkerSeq)
	if i.coordinatorWorkerIDs == nil {
		i.coordinatorWorkerIDs = make(map[string][]string)
	}
	i.coordinatorWorkerIDs[workerID] = append(i.coordinatorWorkerIDs[workerID], registrationID)
	coordinator.Apply(orchestration.Event{Kind: orchestration.EventWorkerRegistered, WorkerID: registrationID})
	var actions []orchestration.Action
	if implicitWave {
		actions = coordinator.Apply(orchestration.Event{Kind: orchestration.EventManagerFinished}).Actions
	}
	i.completionDeliveryMu.Unlock()
	i.executeCoordinatorActions(actions)
}

func (i *Interactive) cancelCoordinator() {
	if i == nil {
		return
	}
	i.completionDeliveryMu.Lock()
	coordinator := i.ensureCoordinatorLocked()
	actions := coordinator.Apply(orchestration.Event{Kind: orchestration.EventCancelled}).Actions
	// A later user turn starts a fresh wave. Drop tracker registrations as well
	// as coordinator identities so late outcomes cannot match a new worker with
	// the same Agent.ID.
	if i.completionTracker != nil {
		i.completionTracker.Reset()
	}
	i.turnCoordinator = orchestration.New()
	i.coordinatorWorkerIDs = nil
	i.completionDeliveryMu.Unlock()
	i.executeCoordinatorActions(actions)
}

func (i *Interactive) takeCoordinatorWorkerID(agentID string) string {
	if i == nil || agentID == "" {
		return ""
	}
	i.completionDeliveryMu.Lock()
	defer i.completionDeliveryMu.Unlock()
	ids := i.coordinatorWorkerIDs[agentID]
	if len(ids) == 0 {
		return ""
	}
	workerID := ids[0]
	if len(ids) == 1 {
		delete(i.coordinatorWorkerIDs, agentID)
	} else {
		i.coordinatorWorkerIDs[agentID] = ids[1:]
	}
	return workerID
}

func (i *Interactive) executeCoordinatorActions(actions []orchestration.Action) {
	for _, action := range actions {
		if action.Kind != orchestration.ActionRunManager {
			continue
		}
		prompt := action.Text
		if len(action.Completions) != 0 {
			instruction := "Briefly summarise the collective outcome for the user. Reference the agents by id. If any failed, suggest a follow-up; otherwise confirm completion. Do not spawn new sub-agents unless the user asks."
			update := subagents.FormatCompletionUpdate(action.Completions, instruction)
			if prompt != "" {
				prompt = update + "\n\nQueued user request:\n" + prompt
			} else {
				prompt = update
			}
		}
		if prompt != "" {
			i.submitOrQueue(prompt, nil, false)
		} else if action.Reason == orchestration.WakeGoal {
			parent := i.runCtx
			if parent == nil {
				parent = context.Background()
			}
			i.requestGoalContinuationIfIdle(parent)
		}
	}
}

// requestCompletionDelivery starts at most one waiter for the current active
// set. A request arriving while that waiter is formatting or submitting is
// picked up before the waiter exits, so a later worker cannot be lost in the
// handoff between batches.
func (i *Interactive) requestCompletionDelivery() {
	if i == nil {
		return
	}
	i.ensureCompletionTracker()
	i.completionDeliveryMu.Lock()
	i.completionDeliveryRequest = true
	if i.completionDeliveryRunning || i.completionDeliveryHolds != 0 {
		i.completionDeliveryMu.Unlock()
		return
	}
	i.completionDeliveryRunning = true
	i.completionDeliveryMu.Unlock()

	go i.deliverCompletionUpdates()
}

// beginCompletionDeliveryHold keeps all completions observed during one
// parent model turn together. Tool calls are executed sequentially inside the
// core agent, so the release point is the owning registration boundary for
// same-parent-turn batching rather than a timing-based debounce.
func (i *Interactive) beginCompletionDeliveryHold() func() {
	if i == nil {
		return func() {}
	}
	i.completionDeliveryMu.Lock()
	i.ensureCoordinatorLocked().Apply(orchestration.Event{Kind: orchestration.EventManagerStarted})
	i.completionDeliveryHolds++
	i.completionDeliveryMu.Unlock()
	return i.releaseCompletionDeliveryHold
}

func (i *Interactive) releaseCompletionDeliveryHold() {
	if i == nil {
		return
	}
	start := false
	var actions []orchestration.Action
	i.completionDeliveryMu.Lock()
	if i.completionDeliveryHolds > 0 {
		i.completionDeliveryHolds--
	}
	if i.completionDeliveryHolds == 0 {
		actions = i.ensureCoordinatorLocked().Apply(orchestration.Event{Kind: orchestration.EventManagerFinished}).Actions
	}
	if i.completionDeliveryHolds == 0 && i.completionDeliveryRequest && !i.completionDeliveryRunning {
		i.completionDeliveryRunning = true
		start = true
	}
	i.completionDeliveryMu.Unlock()
	i.executeCoordinatorActions(actions)
	if start {
		go i.deliverCompletionUpdates()
	}
}

func (i *Interactive) completionWaitContext() context.Context {
	i.mu.Lock()
	ctx := i.runCtx
	i.mu.Unlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func (i *Interactive) deliverCompletionUpdates() {
	for {
		i.completionDeliveryMu.Lock()
		i.completionDeliveryRequest = false
		tracker := i.completionTracker
		i.completionDeliveryMu.Unlock()

		batch, err := tracker.WaitIdle(i.completionWaitContext())
		if err == nil && len(batch) != 0 {
			var actions []orchestration.Action
			for _, completion := range batch {
				workerID := i.takeCoordinatorWorkerID(completion.AgentID)
				if workerID == "" {
					continue
				}
				actions = append(actions, i.applyCoordinator(orchestration.Event{
					Kind:       orchestration.EventWorkerFinished,
					WorkerID:   workerID,
					Completion: completion,
				})...)
			}
			i.executeCoordinatorActions(actions)
		}

		i.completionDeliveryMu.Lock()
		if i.completionDeliveryRequest {
			i.completionDeliveryMu.Unlock()
			continue
		}
		i.completionDeliveryRunning = false
		i.completionDeliveryMu.Unlock()
		return
	}
}

// autoSubagentsAddenda returns the prompt blocks owned by subagent prompting,
// in the same order Resolve appends them to a new agent. The profile manifest
// is independent of strict orchestration and remains available in on-demand
// mode whenever the primary agent can spawn a worker.
func autoSubagentsAddenda(cfg InteractiveConfig, orchestrating bool) []string {
	addenda := make([]string, 0, 2)
	if cfg.Supervisor != nil && autoSubagentsToolAllowedConfig(cfg) {
		if addendum := strings.TrimSpace(cfg.SubagentsSystemAddendum); addendum != "" {
			addenda = append(addenda, addendum)
		}
	}
	if !orchestrating {
		if cfg.Supervisor != nil && autoSubagentsAnyToolAllowedConfig(cfg) {
			if addendum := strings.TrimSpace(cfg.OnDemandSubagentsSystemAddendum); addendum != "" {
				addenda = append(addenda, addendum)
			}
		}
		return addenda
	}

	if addendum := strings.TrimSpace(cfg.AutoSubagentsSystemAddendum); addendum != "" {
		addenda = append(addenda, addendum)
	}
	return addenda
}

// removeLastAutoSubagentsAddendum removes one occurrence of a block known to
// have been appended by auto-subagents. Resolve appends owned blocks at the
// end, so removing the last occurrence preserves an identical base block.
func removeLastAutoSubagentsAddendum(system, addendum string) (string, bool) {
	idx := strings.LastIndex(system, addendum)
	if idx < 0 {
		return system, false
	}
	return system[:idx] + system[idx+len(addendum):], true
}

// applyAutoSubagentsSystemPrompt swaps the prompt blocks owned by subagent
// prompting on the running agent. Toggling strict orchestration changes the
// delegation guidance while retaining the profile manifest when usable.
func (i *Interactive) applyAutoSubagentsSystemPrompt(orchestrating bool) {
	// i.mu is sufficient here: this path updates the existing agent's system
	// prompt and managed addenda in place; it does not replace the agent or
	// its tool registry, so agentMu is not required.
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.agent == nil {
		return
	}

	sys, _ := i.agent.PromptConfig()
	changed := false
	for idx := len(i.managedAutoSubagentsAddenda) - 1; idx >= 0; idx-- {
		var removed bool
		sys, removed = removeLastAutoSubagentsAddendum(sys, i.managedAutoSubagentsAddenda[idx])
		changed = changed || removed
	}
	i.managedAutoSubagentsAddenda = nil

	addenda := autoSubagentsAddenda(i.cfg, orchestrating)
	if changed && len(addenda) > 0 {
		sys = strings.TrimRight(sys, "\n")
	}
	for _, addendum := range addenda {
		if sys != "" {
			sys += "\n\n"
		}
		sys += addendum
		i.managedAutoSubagentsAddenda = append(i.managedAutoSubagentsAddenda, addendum)
		changed = true
	}
	if !changed {
		return
	}
	if len(addenda) == 0 {
		sys = strings.TrimRight(sys, "\n") + "\n"
	}
	i.agent.SetSystemPrompt(sys)
}

// applyAutoSubagentsTool registers the canonical subagent tools whenever
// launch-time policy permits them. Auto-subagents controls prompt behavior,
// while the tools remain available for explicit user-requested delegation.
// Mirrors applyTelegramTools' snapshot+mutate pattern so extension tools and
// /reload-ext additions survive a settings change.
func (i *Interactive) autoSubagentsAvailable() bool {
	return i.cfg.Supervisor != nil && autoSubagentsAnyToolAllowedConfig(i.cfg)
}

func (i *Interactive) autoSubagentsUnavailableHint() string {
	var hints []string
	if i.cfg.Supervisor == nil {
		hints = append(hints, "subagent supervisor not available in this mode")
	}
	if !autoSubagentsAnyToolAllowedConfig(i.cfg) {
		hints = append(hints, "launch-time tool policy excludes subagent manager tools")
	}
	return strings.Join(hints, "; ")
}

func autoSubagentsToolAllowedConfig(cfg InteractiveConfig) bool {
	return cfg.AutoSubagentsToolAllowed == nil || *cfg.AutoSubagentsToolAllowed
}

func autoSubagentsStatusToolAllowedConfig(cfg InteractiveConfig) bool {
	if cfg.AutoSubagentsStatusToolAllowed != nil {
		return *cfg.AutoSubagentsStatusToolAllowed
	}
	// Preserve the old single-boolean embedding contract: when callers have
	// not supplied the new field, status follows the existing delegation gate.
	return autoSubagentsToolAllowedConfig(cfg)
}

func autoSubagentsStopToolAllowedConfig(cfg InteractiveConfig) bool {
	if cfg.AutoSubagentsStopToolAllowed != nil {
		return *cfg.AutoSubagentsStopToolAllowed
	}
	return autoSubagentsToolAllowedConfig(cfg)
}

func autoSubagentsResumeToolAllowedConfig(cfg InteractiveConfig) bool {
	if cfg.AutoSubagentsResumeToolAllowed != nil {
		return *cfg.AutoSubagentsResumeToolAllowed
	}
	return autoSubagentsToolAllowedConfig(cfg)
}

func autoSubagentsAnyToolAllowedConfig(cfg InteractiveConfig) bool {
	return autoSubagentsToolAllowedConfig(cfg) ||
		autoSubagentsStatusToolAllowedConfig(cfg) ||
		autoSubagentsStopToolAllowedConfig(cfg) ||
		autoSubagentsResumeToolAllowedConfig(cfg)
}

func (i *Interactive) autoSubagentsToolAllowed() bool {
	return autoSubagentsToolAllowedConfig(i.cfg)
}

func (i *Interactive) autoSubagentsStatusToolAllowed() bool {
	return autoSubagentsStatusToolAllowedConfig(i.cfg)
}

func (i *Interactive) autoSubagentsStopToolAllowed() bool {
	return autoSubagentsStopToolAllowedConfig(i.cfg)
}

func (i *Interactive) autoSubagentsResumeToolAllowed() bool {
	return autoSubagentsResumeToolAllowedConfig(i.cfg)
}

func (i *Interactive) autoSubagentsEnabledLocked() bool {
	return i.cfg.AutoSubagentsEnabled != nil && *i.cfg.AutoSubagentsEnabled && i.autoSubagentsAvailable()
}

func (i *Interactive) applyAutoSubagentsTool() {
	i.agentMu.Lock()
	defer i.agentMu.Unlock()
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.agent == nil {
		return
	}
	current := i.agent.ToolsSnapshot()
	next := core.Registry{}
	for name, t := range current {
		if name == "subagent_spawn" || name == "subagent_status" || name == "subagent_stop" || name == "subagent_resume" {
			continue
		}
		next[name] = t
	}
	if i.autoSubagentsAvailable() {
		if i.autoSubagentsToolAllowed() {
			canonical := &tools.SubagentSpawnTool{
				Supervisor:       i.cfg.Supervisor,
				Enabled:          func() bool { return true },
				DefaultModel:     func() string { return i.cfg.Model },
				DefaultProvider:  func() string { return i.cfg.Provider },
				DefaultReasoning: func() string { return i.cfg.Reasoning },
				ResolveSubagent:  i.cfg.ResolveSubagent,
				OnSpawned: func(a *subagents.Agent, task string, required bool) {
					i.TrackSubagentWorker(a, task, required)
				},
			}
			next[canonical.Name()] = canonical
		}
		if i.autoSubagentsStatusToolAllowed() {
			statusTool := &tools.SubagentStatusTool{
				Supervisor: i.cfg.Supervisor,
				Enabled:    func() bool { return true },
			}
			next[statusTool.Name()] = statusTool
		}
		if i.autoSubagentsStopToolAllowed() {
			stopTool := &tools.SubagentStopTool{
				Supervisor:      i.cfg.Supervisor,
				Enabled:         func() bool { return true },
				OnStopRequested: i.TrackStoppedSubagentWorker,
			}
			next[stopTool.Name()] = stopTool
		}
		if i.autoSubagentsResumeToolAllowed() {
			resumeTool := &tools.SubagentResumeTool{
				Supervisor: i.cfg.Supervisor,
				Enabled:    func() bool { return true },
				BeforeResumed: func(a *subagents.Agent, prompt string, required bool) func() {
					return i.PrepareResumedSubagentWorker(a, prompt, required)
				},
			}
			next[resumeTool.Name()] = resumeTool
		}
	}
	i.agent.SetTools(next)
}

// applyTelegramTools registers (active=true) or removes (active=false)
// the telegram_send_image and telegram_send_file tools on the running
// agent so the model only sees them while the bridge is connected.
// Snapshots and mutates the live tool registry so any extension or
// /reload-ext additions made while Telegram is connected survive a
// later /telegram disconnect. The two Telegram entries are always replaced;
// web_search is additionally stripped only while the external bridge is active.
func (i *Interactive) applyTelegramTools(active bool) {
	if active {
		// The child ceiling must be in place even when no live main agent exists,
		// and before Bridge.Start can accept an external prompt.
		i.setWebSearchAvailable(false)
	}
	i.agentMu.Lock()
	defer i.agentMu.Unlock()
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.agent == nil {
		return
	}
	current := i.agent.ToolsSnapshot()
	next := core.Registry{}
	for name, t := range current {
		if name == "telegram_send_image" || name == "telegram_send_file" {
			continue
		}
		if active && name == "web_search" {
			// External Telegram prompts have no per-request confirmation
			// surface, so V1 does not expose native web search while paired.
			continue
		}
		next[name] = t
	}
	if active {
		sender := telegramSenderAdapter{bridge: i.telegramBridge}
		next["telegram_send_image"] = &tools.TelegramSendImageTool{
			CWD: i.cfg.CWD, Sandbox: i.cfg.Sandbox, Sender: sender,
		}
		next["telegram_send_file"] = &tools.TelegramSendFileTool{
			CWD: i.cfg.CWD, Sandbox: i.cfg.Sandbox, Sender: sender,
		}
	}
	i.agent.SetTools(next)
	_, webSearchAvailable := next["web_search"]
	i.setWebSearchAvailable(webSearchAvailable && !active)
}

// telegramStatus writes a one-liner describing the bridge state.
// Reports on both the in-tui bridge and the background daemon so
// the user isn't confused when the daemon owns the poll loop.
func (i *Interactive) telegramStatus() {
	var msg string
	if i.telegramBridge != nil && i.telegramBridge.Active() {
		s := i.telegramBridge.State()
		msg = "telegram: connected (tui bridge)"
		if s.Username != "" {
			msg += " as @" + s.Username
		}
		if s.PairedID != 0 {
			msg += fmt.Sprintf(" - paired with user %d", s.PairedID)
		} else {
			msg += " - awaiting pairing"
		}
	} else if pid, alive, _ := telegram.IsRunning(i.cfg.ZutHome); alive && pid > 0 {
		msg = fmt.Sprintf("telegram: background daemon running (pid %d) - /telegram connect won't work until you stop it", pid)
	} else {
		cfg, _ := telegram.LoadConfig(i.cfg.ZutHome)
		if cfg.BotToken == "" {
			msg = "telegram: not configured. run `zut telegram-bot setup` first."
		} else {
			msg = "telegram: disconnected"
			if cfg.BotUsername != "" {
				msg += " (@" + cfg.BotUsername + " ready to connect)"
			}
		}
	}
	i.mu.Lock()
	i.statusOK = msg
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

// telegramHost adapts *Interactive to telegram.Host so the bridge
// can call back into the TUI without importing modes directly.
type telegramHost struct{ iv *Interactive }

func (h *telegramHost) SubmitOrQueue(prompt string, images []provider.ImageBlock) {
	h.iv.SubmitOrQueue(prompt, images)
}

func (h *telegramHost) CancelTurn() { h.iv.CancelTurn() }

func (h *telegramHost) Status() string {
	h.iv.mu.Lock()
	providerName := h.iv.cfg.Provider
	model := h.iv.cfg.Model
	cwd := h.iv.cfg.CWD
	usage := h.iv.cumUsage
	subscription := h.iv.cfg.AuthMethod == "oauth"
	ctxUsed := h.iv.lastCtxInput
	busy := h.iv.busy
	queued := len(h.iv.queued)
	h.iv.mu.Unlock()

	ctxMax := 0
	if m, err := provider.FindModel(providerName, model); err == nil {
		ctxMax = m.ContextWindow
	}
	return telegram.FormatStatus(telegram.StatusSnapshot{
		Provider:     providerName,
		Model:        model,
		CWD:          cwd,
		Usage:        usage,
		Subscription: subscription,
		ContextUsed:  ctxUsed,
		ContextMax:   ctxMax,
		Busy:         busy,
		Queued:       queued,
	})
}

func (h *telegramHost) Notify(level, message string) {
	h.iv.mu.Lock()
	switch level {
	case "error", "warn":
		h.iv.statusErr = message
		h.iv.statusOK = ""
	default:
		h.iv.statusOK = message
		h.iv.statusErr = ""
	}
	h.iv.mu.Unlock()
	h.iv.invalidate()
}

// openSessionOpsDialog shows the picker for `/session` with no arg.
// Always offers export, import, fork, tree; the handlers bail with
// a clear status message when the precondition isn't met (empty
// transcript on fork; no parent/siblings on tree).
func (i *Interactive) openSessionOpsDialog() {
	items := []sessionOpsItem{
		{label: "export", action: "export", hint: "write the current session to a .zutsession file"},
		{label: "import", action: "import", hint: "load a .zutsession file into this directory"},
		{label: "fork", action: "fork", hint: "branch from a past user message into a new session"},
		{label: "tree", action: "tree", hint: "switch between branches in this directory"},
	}
	i.sessionOpsDialog.Open(items)
	i.invalidate()
}

// doSessionOp dispatches export, import, fork, or tree. arg is the
// optional positional argument from e.g. /session export <path>
// or /session import <path>; fork and tree ignore it.
func (i *Interactive) doSessionOp(action, arg string) {
	switch action {
	case "export":
		i.doSessionExport(arg)
	case "import":
		i.doSessionImport(arg)
	case "fork":
		i.doSessionFork()
	case "tree":
		i.doSessionTree()
	default:
		i.mu.Lock()
		i.statusErr = "unknown /session action: " + action + " (use export, import, fork, or tree)"
		i.mu.Unlock()
		i.invalidate()
	}
}

// doSessionExport writes the live session file to destination path
// dst. When dst is empty we default to ~/Downloads (falling back to
// the user's home directory if it doesn't exist). The helper
// expands a leading `~` and creates any missing parent directories.
func (i *Interactive) doSessionExport(dst string) {
	src := i.currentSessionPath()
	if src == "" {
		i.mu.Lock()
		i.statusErr = "export: no session is active (running with --no-session?)"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	// Persist any in-memory agent messages to the session file so
	// the export carries the full conversation. Without this, the
	// default lazy-flush-at-exit strategy leaves most of a running
	// session unwritten and the export ends up with only the meta.
	if i.cfg.FlushSession != nil {
		i.cfg.FlushSession()
	}
	dst = unquotePath(dst)
	if dst == "" {
		dst = defaultExportDir()
	} else {
		dst = expandTilde(dst)
	}
	out, err := core.ExportSession(src, dst)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "export: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.mu.Lock()
	i.statusOK = "exported session to " + friendlyPath(out)
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}

// doSessionImport copies the .zutsession file at src into the
// running cwd's sessions directory and loads it as the active
// session, same as `/sessions` -> pick. When src is empty we ask
// the user to pass a path (no usable default here).
func (i *Interactive) doSessionImport(src string) {
	src = unquotePath(src)
	if src == "" {
		i.mu.Lock()
		i.statusErr = "import: pass a path — e.g. /session import ~/Downloads/work.zutsession"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	src = expandTilde(src)
	if _, err := os.Stat(src); err != nil {
		i.mu.Lock()
		i.statusErr = "import: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	newPath, err := core.ImportSession(src, i.sessionsRoot(), i.cfg.CWD, i.cfg.Version)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "import: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if i.cfg.LoadSession == nil {
		i.mu.Lock()
		i.statusOK = "imported session at " + friendlyPath(newPath) + " (run /sessions to resume it)"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.markSessionTitleSwitching()
	if err := i.cfg.LoadSession(newPath); err != nil {
		i.restoreFailedSessionTitle()
		i.mu.Lock()
		i.statusErr = "import: load failed: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.restoreLoadedSessionTitle()
	state := i.restoreCurrentCompactHandoff()
	i.mu.Lock()
	i.statusOK = "imported and switched to session " + friendlyPath(newPath)
	i.statusErr = ""
	if i.agent != nil {
		i.view.Messages = filterHiddenTranscriptMessages(i.agent.Messages())
		i.cumUsage = i.agent.Cost()
		last := i.agent.LastTurnUsage()
		i.lastCtxInput = last.InputTokens + last.CacheReadTokens + last.CacheWriteTokens
		if len(i.view.Messages) > initialResumeTailLimit {
			i.view.TailLimit = initialResumeTailLimit
		} else {
			i.view.TailLimit = 0
		}
		i.view.InvalidateRenderCache()
	}
	i.mu.Unlock()
	i.invalidate()
	if state.reason != compactContinuationNone {
		i.startRestoredCompactHandoff(i.runCtx)
	}
}

// defaultExportDir returns ~/Downloads when it exists, or ~ as a
// fallback, or /tmp on exotic machines with no home dir.
func defaultExportDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return os.TempDir()
	}
	downloads := filepath.Join(home, "Downloads")
	if fi, err := os.Stat(downloads); err == nil && fi.IsDir() {
		return downloads
	}
	return home
}

// expandTilde turns a leading ~ into the user's home directory.
// Returns the input unchanged when there's no tilde or no home.
func expandTilde(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if len(p) == 1 {
		return home
	}
	if p[1] == '/' || p[1] == filepath.Separator {
		return filepath.Join(home, p[2:])
	}
	return p
}

// unquotePath strips a matching pair of surrounding single or
// double quotes. Drag-drop paste in the tui auto-quotes dropped
// file paths so the shell-like `/session import 'foo bar.zs'`
// stays well-formed; when the TUI's own slash handler consumes
// the arg, we want the raw path back.
func unquotePath(p string) string {
	p = strings.TrimSpace(p)
	if len(p) >= 2 {
		first := p[0]
		last := p[len(p)-1]
		if (first == '\'' && last == '\'') || (first == '"' && last == '"') {
			p = p[1 : len(p)-1]
		}
	}
	return p
}

// friendlyPath collapses the user's home directory to a leading ~
// so status messages read cleanly. Falls back to the raw path when
// the home dir is unknown.
func friendlyPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}

// doSessionFork opens the /jump turn picker in "fork mode". The
// next selection branches the current session at that user turn
// instead of scrolling the viewport to it.
func (i *Interactive) doSessionFork() {
	path := i.currentSessionPath()
	if path == "" {
		i.mu.Lock()
		i.statusErr = "fork: no session is active (running with --no-session?)"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.mu.Lock()
	busy := i.busy || i.shellRunning || i.compacting || i.autoCompacting || i.sessionLoading
	queued := len(i.queued) != 0
	ag := i.agent
	i.mu.Unlock()
	if busy || queued || ag == nil || ag.QueuedMessageCount() != 0 {
		i.mu.Lock()
		i.statusErr = "fork: wait until the current turn and queued messages finish"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if i.cfg.FlushSession != nil {
		i.cfg.FlushSession()
	}
	snapshot, err := core.ReadSessionSnapshot(path)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "fork: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	msgs := snapshot.Messages
	if len(msgs) == 0 {
		i.mu.Lock()
		i.statusErr = "fork: transcript is empty; nothing to fork from"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.pendingFork = true
	i.jumpDialog.Open(msgs, "")
	i.invalidate()
}

// doSessionTree opens the current session family after the shared
// fail-closed gate succeeds. It intentionally does not fall back to the
// in-memory transcript: a tree that cannot be persisted and reloaded cannot
// safely create a navigation branch.
func (i *Interactive) doSessionTree() {
	i.openSessionTree()
}

// applySessionTreeMessageSelection keeps the old scalar integration
// source-compatible for package consumers. New dialog selections use the
// structured sessionTreeTarget path below.
func (i *Interactive) applySessionTreeMessageSelection(src string, msgIdx, turnNo int, role provider.Role, prompt string) {
	target := sessionTreeTarget{
		SourcePath:        src,
		EffectiveIndex:    msgIdx,
		SelectionBoundary: msgIdx + 1,
		Role:              role,
		UserDraft:         prompt,
		Boundary:          sessionTreeMessageBoundary,
	}
	if role == provider.RoleUser {
		target.SelectionBoundary = msgIdx
	}
	i.applySessionTreeTarget(target, turnNo)
}

// applySessionTreeTarget checks out a structured dialog target. The target's
// source and effective indices come from the same preflight snapshot the
// dialog rendered, while the live read below preserves compatibility with
// the existing LoadSession(path) error callback.
func (i *Interactive) applySessionTreeTarget(target sessionTreeTarget, turnNo int) {
	// Selection is a state-changing operation too. Use the selection gate so
	// a queued/busy turn cannot race the hidden branch or accidentally submit
	// a provider turn while the session is being swapped. The open gate cannot
	// be reused here because the tree dialog itself owns the keyboard.
	if !i.canCommitSessionTreeSelection() {
		i.setSessionTreeError("tree: session branching is not available in this build")
		return
	}
	src := target.SourcePath
	if src == "" {
		src = i.currentSessionPath()
	}
	if src == "" {
		i.setSessionTreeError("tree: no session is active")
		return
	}

	// Read and validate the selected row before flushing or creating anything.
	// This gives tool-call/result pairs an atomic boundary and prevents a
	// malformed selection from changing the active session. Historical rows
	// come from a pre-compaction segment; current rows use the effective
	// snapshot that the running agent resumes from.
	var msgs []provider.Message
	var historicalSegment core.SessionHistorySegment
	if target.Historical {
		history, err := core.ReadSessionHistory(src)
		if err != nil {
			i.setSessionTreeError("tree: read selection: " + err.Error())
			return
		}
		if target.HistorySegment < 0 || target.HistorySegment >= len(history.Segments) {
			i.setSessionTreeError("tree: selected history segment is unavailable")
			return
		}
		historicalSegment = history.Segments[target.HistorySegment]
		msgs = historicalSegment.Messages
	} else {
		sess, current, err := core.OpenSession(src)
		if err != nil {
			i.setSessionTreeError("tree: read selection: " + err.Error())
			return
		}
		if err := sess.Close(); err != nil {
			i.setSessionTreeError("tree: close selection: " + err.Error())
			return
		}
		msgs = current
	}
	selection, err := sessionTreeSelection(msgs, target)
	if err != nil {
		i.setSessionTreeError("tree: " + err.Error())
		return
	}
	if i.cfg.FlushSession != nil {
		i.cfg.FlushSession()
	}
	var newPath string
	if target.Historical {
		newPath, err = core.BranchSessionHiddenFromHistory(src, i.sessionsRoot(), i.cfg.CWD, i.cfg.Version, historicalSegment, selection.upTo)
	} else {
		newPath, err = core.BranchSessionHidden(src, i.sessionsRoot(), i.cfg.CWD, i.cfg.Version, selection.upTo)
	}
	if err != nil {
		i.setSessionTreeError("tree: " + err.Error())
		return
	}
	i.markSessionTitleSwitching()
	if err := i.cfg.LoadSession(newPath); err != nil {
		i.restoreFailedSessionTitle()
		i.setSessionTreeError("tree: checkout failed: " + err.Error())
		return
	}
	state := i.restoreCurrentCompactHandoff()

	// LoadSession is intentionally the old func(string) error callback: the
	// CLI and embedders already own agent/session swapping. Refresh the view
	// from the swapped agent here instead of requiring a new callback result.
	i.resetSessionTitleForFreshBranch()
	i.scrollToBottom()
	i.mu.Lock()
	i.lastCtxInput = 0
	if i.agent != nil {
		i.view.Messages = filterHiddenTranscriptMessages(i.agent.Messages())
		i.cumUsage = i.agent.Cost()
		last := i.agent.LastTurnUsage()
		i.lastCtxInput = last.InputTokens + last.CacheReadTokens + last.CacheWriteTokens
		i.view.InvalidateRenderCache()
	}
	i.toolCalls = map[string]*tui.ToolCallView{}
	i.toolOrder = nil
	i.toolGate = map[string]int{}
	i.extNotes = nil
	if selection.restoreDraft {
		i.ed.SetValue(selection.draftText)
		i.clipboardImages = selection.images
		i.statusOK = fmt.Sprintf("checked out before turn %d; edit and send to branch", turnNo)
	} else {
		i.ed.Clear()
		i.clipboardImages = nil
		i.statusOK = fmt.Sprintf("checked out turn %d into a new branch", turnNo)
	}
	i.statusErr = ""
	i.mu.Unlock()
	i.scrollToBottom()
	// Selection does not submit, start, or queue the restored user draft; only
	// a later explicit Enter does that. An inherited compact handoff may resume
	// its hidden continuation below when this branch has the complete effective
	// transcript, without submitting the restored draft.
	i.invalidate()
	if state.reason != compactContinuationNone {
		i.startRestoredCompactHandoff(i.runCtx)
	}
}

// applyForkSelection branches the current session at msgIdx+1 (so
// the selected user message and everything before it is included
// in the new branch), then switches the running agent to the new
// file. Called from the jump-dialog handler when pendingFork=true.
func (i *Interactive) applyForkSelection(msgIdx int) {
	i.pendingFork = false
	src := i.currentSessionPath()
	if src == "" {
		i.mu.Lock()
		i.statusErr = "fork: no session is active"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if i.cfg.FlushSession != nil {
		i.cfg.FlushSession()
	}
	// msgIdx is 0-indexed message position; copy msgIdx+1 rows so
	// the selected user message is included.
	upTo := msgIdx + 1
	newPath, err := core.BranchSession(src, i.sessionsRoot(), i.cfg.CWD, i.cfg.Version, upTo)
	if err != nil {
		i.mu.Lock()
		i.statusErr = "fork: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if i.cfg.LoadSession == nil {
		i.mu.Lock()
		i.statusOK = "forked at message " + formatInt(upTo) + " (run /sessions to resume)"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.markSessionTitleSwitching()
	if err := i.cfg.LoadSession(newPath); err != nil {
		i.restoreFailedSessionTitle()
		i.mu.Lock()
		i.statusErr = "fork: switch failed: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	state := i.restoreCurrentCompactHandoff()
	i.resetSessionTitleForFreshBranch()
	i.mu.Lock()
	i.statusOK = "forked and switched to new branch at " + friendlyPath(newPath)
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
	if state.reason != compactContinuationNone {
		i.startRestoredCompactHandoff(i.runCtx)
	}
}

// formatInt is a tiny strconv.Itoa shim; keeps the handler above
// from needing a strconv import just for one call.
func formatInt(n int) string {
	return fmt.Sprintf("%d", n)
}

// assistantText returns the concatenated text of every TextBlock in
// m. Used by the streaming-view dedupe guard to tell when a live
// streamed reply has already been promoted into the transcript.
func assistantText(m provider.Message) string {
	var sb strings.Builder
	for _, c := range m.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

// resetTranscriptRenderLocked invalidates every render cache that assumes
// the previous transcript remains structurally intact. Compaction is a
// replacement, not an append: the flow renderer must repaint from the new
// transcript rather than diff it against scrollback rows from the old one.
// Must be called with i.mu held.
func (i *Interactive) resetTranscriptRenderLocked() {
	i.view.InvalidateRenderCache()
	i.chatCacheValid = false
	i.prevChatLen = 0
	i.prevChatCols = 0
	i.prevChatRows = 0
	i.prevScrollOffset = 0
	i.requestRendererInvalidate()
}

// resetStreamingStateLocked clears every piece of streaming state
// in one shot. Used by abort paths (turn cancel, compact hand-off,
// queue drain) so the pacer doesn't keep draining stale runes from
// a prior turn. Must be called with i.mu held.
func (i *Interactive) resetStreamingStateLocked() {
	i.streaming.Reset()
	i.streamPending = i.streamPending[:0]
	i.streamFlushPending = false
	i.streamOn = false
	i.openAllToolGatesLocked()
}

// openAllToolGatesLocked drops every pending tool gate so that any
// tool registered during this turn renders unconditionally from now
// on. Called when streaming finalizes (the paced text has fully
// drained and `streaming` is about to reset to length 0): without
// this, the gate comparison against a freshly-reset streaming buffer
// would wrongly re-hide tools that had already cleared their gate.
// Must be called with i.mu held.
func (i *Interactive) openAllToolGatesLocked() {
	for id := range i.toolGate {
		i.toolGate[id] = 0
	}
}

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
func (i *Interactive) gateToolLocked(id string) {
	if _, ok := i.toolGate[id]; ok {
		return
	}
	if !i.streamOn {
		i.toolGate[id] = 0
		return
	}
	i.toolGate[id] = i.streaming.Len() + len(i.streamPending)
}

// toolGateOpenLocked reports whether the gated tool block may render
// yet, i.e. the pacer has drained enough text to reach the position
// recorded when the tool call arrived. Must be called with i.mu held.
func (i *Interactive) toolGateOpenLocked(id string) bool {
	gate, ok := i.toolGate[id]
	if !ok || gate == 0 {
		return true
	}
	return i.streaming.Len() >= gate
}

// assistantMessageSideEffects runs the non-visual hooks attached to
// EvAssistantMessage: the host-provided OnAssistant callback and the
// telegram-bridge mirror. Called with i.mu held.
//
// Factored out of handleEvent because the streaming pacer may defer
// visual reset until after the last buffered rune has painted, but
// the callbacks themselves must fire on message arrival so
// downstream observers (session persistence, telegram, cost panels)
// don't wait on a UI animation to catch up.
func (i *Interactive) assistantMessageSideEffects(m provider.Message) {
	if i.cfg.OnAssistant != nil {
		i.cfg.OnAssistant(m)
	}
	if i.telegramBridge != nil && i.telegramBridge.Active() {
		var sb strings.Builder
		for _, c := range m.Content {
			if tb, ok := c.(provider.TextBlock); ok {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(tb.Text)
			}
		}
		if text := sb.String(); strings.TrimSpace(text) != "" {
			go i.telegramBridge.OnAssistantText(text)
		}
	}
}

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
func (i *Interactive) runStreamPacer(ctx context.Context) {
	t := time.NewTicker(paintPaceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			i.mu.Lock()
			if len(i.streamPending) == 0 {
				// EvAssistantMessage already fired but the pacer
				// was still draining a tick ago. Everything is now
				// painted; clear the streaming flags so the next
				// redraw shows the finalised transcript message
				// and hides the streaming overlay.
				if i.streamFlushPending {
					i.streamFlushPending = false
					i.streaming.Reset()
					i.streamOn = false
					i.openAllToolGatesLocked()
					i.mu.Unlock()
					i.invalidate()
					continue
				}
				i.mu.Unlock()
				continue
			}
			n := paintPaceRate
			if n > len(i.streamPending) {
				n = len(i.streamPending)
			}
			i.streaming.WriteString(string(i.streamPending[:n]))
			i.streamPending = i.streamPending[n:]
			i.mu.Unlock()
			i.invalidate()
		}
	}
}
