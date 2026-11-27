// Package config manages persisted zut configuration (config.json) under
// ZUT_HOME and the paths to zut state.
package config

import (
	"os"
	"strings"
)

// QuickModelShortcut is one configured model profile slot.
type QuickModelShortcut struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Reasoning string `json:"reasoning,omitempty"`
	FastMode  bool   `json:"fast_mode,omitempty"`
}

// SubagentsConfig contains the resident manager policy. max_concurrent limits
// simultaneous turns, queue_timeout bounds admission waits, allowed_tools
// limits child capabilities and allowed_roots limits eligible workspaces.
// Obsolete keys are ignored.
type SubagentsConfig struct {
	MaxConcurrent int      `json:"max_concurrent,omitempty"`
	QueueTimeout  string   `json:"queue_timeout,omitempty"`
	AllowedTools  []string `json:"allowed_tools,omitempty"`
	AllowedRoots  []string `json:"allowed_roots,omitempty"`
}

// GoalsConfig controls optional resource limits for autonomous goals. A nil
// MaxTokenBudget leaves goals unlimited; when set, it is the host-configured
// cap applied to newly created goals.
type GoalsConfig struct {
	MaxTokenBudget *uint64 `json:"max_token_budget,omitempty"`
}

// ExperimentalConfig contains opt-in capabilities that are disabled by default.
type ExperimentalConfig struct {
	Codemode         *bool `json:"codemode,omitempty"`
	CodemodeDeferMCP *bool `json:"codemode_defer_mcp,omitempty"`
}

// Config is the persisted user configuration.
type Config struct {
	Experimental *ExperimentalConfig `json:"experimental,omitempty"`
	Advisor      *AdvisorConfig      `json:"advisor,omitempty"`
	Dictation    *DictationSettings  `json:"dictation,omitempty"`
	Provider     string              `json:"provider"`
	Model        string              `json:"model"`
	Reasoning    string              `json:"reasoning"`
	Temperature  *float32            `json:"temperature,omitempty"`
	Theme        string              `json:"theme"`

	// FastMode requests OpenAI's fast service tier. Off by default;
	// nil/missing means disabled. An active model profile supplies its
	// per-profile value at startup. Other providers reject fast-mode
	// requests at the provider boundary.
	FastMode *bool `json:"fast_mode,omitempty"`

	// CompactInput renders sent user messages as a single quiet gutter
	// line instead of a padded, background-tinted bubble. nil/false
	// (the default) keeps the bubble. The ZUT_COMPACT_INPUT env var
	// overrides this when set.
	CompactInput *bool `json:"compact_input,omitempty"`

	// QuickModelShortcuts maps slots 1-9 to model profiles used by
	// Ctrl+1..9. Cmd+1..9 may also work on terminals that forward Super.
	QuickModelShortcuts []QuickModelShortcut `json:"quick_model_shortcuts,omitempty"`
	ActiveModelProfile  int                  `json:"active_model_profile,omitempty"`

	// InlineImagesEnabled controls whether zut draws screenshots inline
	// when the terminal supports an image protocol. nil/missing means
	// auto (enabled when supported); false disables; true forces the
	// detected protocol when available.
	InlineImagesEnabled *bool `json:"inline_images_enabled,omitempty"`

	// TerminalAlertsEnabled controls interactive terminal alerts from the
	// main agent and extensions. nil/missing means enabled; false disables
	// alerts. Toggle from /settings.
	TerminalAlertsEnabled *bool `json:"terminal_alerts_enabled,omitempty"`

	// TerminalTitleEnabled controls the hidden title request and OSC 0 title
	// updates in interactive mode. nil/missing means enabled; false disables
	// both so no extra provider request is made. Toggle from /settings.
	TerminalTitleEnabled *bool `json:"terminal_title_enabled,omitempty"`

	// NextPromptSuggestionsEnabled controls the predicted next user message
	// shown as ghost text after each finished interactive turn. nil/missing
	// means disabled; true opts into the extra request. Toggle from /settings.
	NextPromptSuggestionsEnabled *bool `json:"next_prompt_suggestions_enabled,omitempty"`

	// Subagents controls manager-owned child lifecycle and resource policy.
	Subagents SubagentsConfig `json:"subagents,omitempty"`

	// Goals configures optional resource limits for autonomous goals. Missing
	// configuration preserves unlimited autonomous execution.
	Goals GoalsConfig `json:"goals,omitempty"`

	// AutoSubagentsEnabled lets the interactive primary proactively delegate
	// independent sidecar work through built-in subagent tools. Off by default;
	// nil/missing means disabled. Toggle from /settings.
	AutoSubagentsEnabled *bool `json:"auto_subagents_enabled,omitempty"`

	// WebSearchEnabled controls the built-in web_search capability for
	// normal CLI sessions. nil/missing means enabled so existing config
	// files retain the default-on behavior.
	WebSearchEnabled *bool `json:"web_search_enabled,omitempty"`

	// LSPEnabled controls the built-in lsp tool and write-time diagnostics
	// for the main session. nil/missing means enabled. Toggle from
	// /settings.
	LSPEnabled *bool `json:"lsp_enabled,omitempty"`

	// SubagentLSPEnabled controls LSP availability in resident child agents.
	// nil/missing means enabled. Toggle from /settings.
	SubagentLSPEnabled *bool `json:"subagent_lsp_enabled,omitempty"`

	// LSPDiagnosticsOnWrite enables bounded diagnostics after a successful
	// write. nil/missing means enabled when LSP itself is enabled.
	LSPDiagnosticsOnWrite *bool `json:"lsp_diagnostics_on_write,omitempty"`

	// LSPDiagnosticsOnEdit enables bounded diagnostics after a successful
	// edit. nil/missing means enabled when LSP itself is enabled.
	LSPDiagnosticsOnEdit *bool `json:"lsp_diagnostics_on_edit,omitempty"`

	// AutoCompactThreshold is the percentage of the model context window
	// that triggers automatic transcript compaction in interactive mode.
	// nil/missing means 85; valid values are 0, 70, 80, 85, 90, or 95.
	// zero disables percentage-based triggers.
	AutoCompactThreshold *int `json:"auto_compact_threshold,omitempty"`

	// JailByDefault confines tools to the session working directory when
	// a new agent starts. Off by default; nil/missing means disabled.
	// The live session can still override this with /jail or /unjail.
	JailByDefault *bool `json:"jail_by_default,omitempty"`

	// RecursiveFileSuggest controls the @-mention file picker. When true
	// the picker fuzzy-searches the whole project tree below the working
	// directory; nil/missing/false keeps the default directory-by-
	// directory browse. Toggle from /settings.
	RecursiveFileSuggest *bool `json:"recursive_file_suggest,omitempty"`

	// RespectGitignore controls whether the @-mention file picker hides
	// files and directories matched by the project's root .gitignore (in
	// both flat and recursive modes). nil/missing means the default,
	// which is on; false shows ignored entries. Toggle from /settings.
	RespectGitignore *bool `json:"respect_gitignore,omitempty"`

	// ShowInstructionsAtStartup lists loaded context files, extensions,
	// and user-installed skills above the transcript. Off by default;
	// nil/missing means disabled. Toggle from /settings.
	ShowInstructionsAtStartup *bool `json:"show_instructions_at_startup,omitempty"`

	// TUIInputStyle controls the main input rendering. Supported values:
	// "plain" (default), "lines", and "block".
	TUIInputStyle string `json:"tui_input_style,omitempty"`

	// TUIStatusPosition controls whether model, usage, and cwd information
	// render above or below the main input. Supported values: "above_input"
	// (default) and "below_input".
	TUIStatusPosition string `json:"tui_status_position,omitempty"`

	// TUIWorkingPosition controls whether the busy/working spinner renders
	// above or below the main input. Supported values: "above_input"
	// (default) and "below_input".
	TUIWorkingPosition string `json:"tui_working_position,omitempty"`

	// PythonInterpreter selects the Python 3 executable for the python tool.
	// It is one executable path, not a command line: absolute paths are used
	// directly, relative paths resolve against the session working directory.
	// Empty means automatic discovery (VIRTUAL_ENV, ./.venv, then PATH).
	// Zut never installs Python or packages; the environment must exist.
	PythonInterpreter string `json:"python_interpreter,omitempty"`

	// Insecure skips TLS verification for custom inference endpoints.
	Insecure bool `json:"insecure,omitempty"`

	// HTTPProxy is a global proxy URL used for HTTP and HTTPS requests when
	// the corresponding standard proxy environment variable is not already set.
	HTTPProxy string `json:"http_proxy,omitempty"`

	// LastChangelogShown is the version whose release-notes
	// dialog the user has already seen. When the running binary's
	// version differs, the next interactive run shows the
	// changelog (fetched from the GitHub release page) once and
	// updates this field. Empty means "never shown".
	LastChangelogShown string `json:"last_changelog_shown,omitempty"`
}

// CodemodeEnabled reports whether codemode was explicitly enabled.
func (c Config) CodemodeEnabled() bool {
	return c.Experimental != nil && c.Experimental.Codemode != nil && *c.Experimental.Codemode
}

// CodemodeDeferMCP defaults to true only when codemode is enabled.
func (c Config) CodemodeDeferMCP() bool {
	return c.CodemodeEnabled() && (c.Experimental.CodemodeDeferMCP == nil || *c.Experimental.CodemodeDeferMCP)
}

// CompactUserInput reports whether sent user messages should render as
// a single quiet gutter line instead of a padded, background-tinted
// bubble. The ZUT_COMPACT_INPUT env var takes precedence over the
// config when set: "1"/"true"/"yes"/"on" force compact, "0"/"false"/
// "no"/"off" force the bubble. Otherwise the config's compact_input
// is consulted (nil/false means the bubble).
func (c Config) CompactUserInput() bool {
	if v := strings.TrimSpace(strings.ToLower(os.Getenv("ZUT_COMPACT_INPUT"))); v != "" {
		switch v {
		case "1", "true", "yes", "on", "compact":
			return true
		case "0", "false", "no", "off", "bubble":
			return false
		}
	}
	return c.CompactInput != nil && *c.CompactInput
}

// LSPEnabledFor reports whether LSP is enabled for a main or subagent
// session. Both settings default to true so new installations
// get code intelligence without a migration or generated config file.
func (c Config) LSPEnabledFor(subagent bool) bool {
	value := c.LSPEnabled
	if subagent {
		value = c.SubagentLSPEnabled
	}
	return value == nil || *value
}

// WebSearchEnabledForCLI reports the default-on web-search preference
// without requiring a generated config entry.
func (c Config) WebSearchEnabledForCLI() bool {
	return c.WebSearchEnabled == nil || *c.WebSearchEnabled
}

// LSPDiagnosticsOnWriteEnabled reports the default-on write diagnostics
// preference without requiring a generated config entry.
func (c Config) LSPDiagnosticsOnWriteEnabled(subagent bool) bool {
	return c.LSPEnabledFor(subagent) && (c.LSPDiagnosticsOnWrite == nil || *c.LSPDiagnosticsOnWrite)
}

// LSPDiagnosticsOnEditEnabled reports the default-on edit diagnostics
// preference without requiring a generated config entry.
func (c Config) LSPDiagnosticsOnEditEnabled(subagent bool) bool {
	return c.LSPEnabledFor(subagent) && (c.LSPDiagnosticsOnEdit == nil || *c.LSPDiagnosticsOnEdit)
}
