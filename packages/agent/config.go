// Package agent wires the provider, core, tools, auth, and modes into a CLI.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	providerpkg "github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/auth"
)

// QuickModelShortcut is one configured keyboard shortcut slot.
type QuickModelShortcut struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// SubagentsConfig contains the persisted supervisor policy. Duration values
// use Go strings such as "20m" so config files remain readable.
type SubagentsConfig struct {
	MaxConcurrent          int      `json:"max_concurrent,omitempty"`
	MaxConcurrentPerParent int      `json:"max_concurrent_per_parent,omitempty"`
	QueueTimeout           string   `json:"queue_timeout,omitempty"`
	StartupTimeout         string   `json:"startup_timeout,omitempty"`
	DefaultTimeout         string   `json:"default_timeout,omitempty"`
	MaxTurns               int      `json:"max_turns,omitempty"`
	MaxSteps               int      `json:"max_steps,omitempty"`
	MaxOutputBytes         int      `json:"max_output_bytes,omitempty"`
	MaxOutputLines         int      `json:"max_output_lines,omitempty"`
	AllowedTools           []string `json:"allowed_tools,omitempty"`
	AllowedRoots           []string `json:"allowed_roots,omitempty"`
	HeartbeatInterval      string   `json:"heartbeat_interval,omitempty"`
	IdleTimeout            string   `json:"idle_timeout,omitempty"`
	ReconnectTimeout       string   `json:"reconnect_timeout,omitempty"`
	CancelGracePeriod      string   `json:"cancel_grace_period,omitempty"`

	// MaxTotalSpawned is retained only to accept existing configuration.
	// Lifetime total-spawn budgets are no longer enforced.
	MaxTotalSpawned int `json:"max_total_spawned,omitempty"`
}

// Config is the persisted user configuration.
type Config struct {
	Provider    string   `json:"provider"`
	Model       string   `json:"model"`
	Reasoning   string   `json:"reasoning"`
	Temperature *float32 `json:"temperature,omitempty"`
	Theme       string   `json:"theme"`

	// FastMode requests OpenAI's fast service tier. Off by default;
	// nil/missing means disabled. Other providers reject fast-mode
	// requests at the provider boundary.
	FastMode *bool `json:"fast_mode,omitempty"`

	// ToolRender selects how tool calls are drawn in interactive mode.
	// "box" (default, or empty) wraps each call in a bordered panel;
	// "flat" drops the frame for a quiet header line plus indented,
	// frameless output. The ZUT_FLAT_TOOLS env var overrides this when
	// set ("1"/"true" forces flat, "0"/"false" forces box).
	ToolRender string `json:"tool_render,omitempty"`

	// CompactInput renders sent user messages as a single quiet gutter
	// line instead of a padded, background-tinted bubble. nil/false
	// (the default) keeps the bubble. The ZUT_COMPACT_INPUT env var
	// overrides this when set.
	CompactInput *bool `json:"compact_input,omitempty"`

	// QuickModelShortcuts maps slots 1-9 to provider/model pairs used by
	// Ctrl+1..9. Cmd+1..9 may also work on terminals that forward Super.
	QuickModelShortcuts []QuickModelShortcut `json:"quick_model_shortcuts,omitempty"`

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

	// Subagents controls manager-owned child lifecycle and resource policy.
	Subagents SubagentsConfig `json:"subagents,omitempty"`

	// AutoSubagentsEnabled lets the main agent spawn background sub-agents
	// and query their live state via built-in auto-subagent tools. Off by
	// default; nil/missing means disabled. Toggle from /settings.
	AutoSubagentsEnabled *bool `json:"auto_subagents_enabled,omitempty"`

	// PonytailEnabled controls the compact coding-guidance addendum. On by
	// default; nil/missing means enabled. Toggle from /settings.
	PonytailEnabled *bool `json:"ponytail_enabled,omitempty"`

	// WebSearchEnabled controls the built-in web_search capability for
	// normal CLI sessions. nil/missing means enabled so existing config
	// files retain the default-on behavior.
	WebSearchEnabled *bool `json:"web_search_enabled,omitempty"`

	// LSPEnabled controls the built-in lsp tool and write-time diagnostics
	// for the main session. nil/missing means enabled. Toggle from
	// /settings.
	LSPEnabled *bool `json:"lsp_enabled,omitempty"`

	// SubagentLSPEnabled controls LSP availability in subagent worker
	// processes. nil/missing means enabled. Toggle from /settings.
	SubagentLSPEnabled *bool `json:"subagent_lsp_enabled,omitempty"`

	// LSPDiagnosticsOnWrite enables bounded diagnostics after a successful
	// write. nil/missing means enabled when LSP itself is enabled.
	LSPDiagnosticsOnWrite *bool `json:"lsp_diagnostics_on_write,omitempty"`

	// LSPDiagnosticsOnEdit enables bounded diagnostics after a successful
	// edit. nil/missing means disabled to keep multi-edit sequences quiet.
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

	// CompactMode renders the interactive transcript with less chrome:
	// tool calls use flat headers instead of bordered panels, and sent
	// user messages render without padded background bubbles. Off by
	// default; nil/missing means disabled. Toggle from /settings.
	CompactMode *bool `json:"compact_mode,omitempty"`

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

	// TUISubagentPosition controls whether live subagent activity renders
	// above or below the main input. Supported values: "above_input" and
	// "below_input" (default).
	TUISubagentPosition string `json:"tui_subagent_position,omitempty"`

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

// ZutHome returns $ZUT_HOME or the OS-default data dir.
//
// All zut state (config.json, auth.json, sessions/, logs/) lives under
// this directory.
func ZutHome() string {
	if v := os.Getenv("ZUT_HOME"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "zut")
	}
	switch runtime.GOOS {
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "zut")
		}
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "zut")
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "zut")
	}
	return ".zut"
}

// ConfigPath returns the path to config.json.
func ConfigPath() string { return filepath.Join(ZutHome(), "config.json") }

// FlatToolRender reports whether tool calls should render flat (no
// bordered panel). The ZUT_FLAT_TOOLS env var takes precedence over
// the config when set: "1"/"true"/"yes"/"on" force flat, "0"/"false"/
// "no"/"off" force box. Otherwise the config's tool_render is
// consulted; "flat" is flat, anything else (including empty) is box.
func (c Config) FlatToolRender() bool {
	if v := strings.TrimSpace(strings.ToLower(os.Getenv("ZUT_FLAT_TOOLS"))); v != "" {
		switch v {
		case "1", "true", "yes", "on", "flat":
			return true
		case "0", "false", "no", "off", "box":
			return false
		}
	}
	return strings.EqualFold(strings.TrimSpace(c.ToolRender), "flat")
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
// sub-agent session. Both settings default to true so new installations
// get code intelligence without a migration or generated config file.
func (c Config) LSPEnabledFor(subagent bool) bool {
	value := c.LSPEnabled
	if subagent {
		value = c.SubagentLSPEnabled
	}
	return value == nil || *value
}

// PonytailModeEnabled reports the default-on coding-guidance preference
// without requiring a generated config entry.
func (c Config) PonytailModeEnabled() bool {
	return c.PonytailEnabled == nil || *c.PonytailEnabled
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

// LSPDiagnosticsOnEditEnabled reports the default-off edit diagnostics
// preference without requiring a generated config entry.
func (c Config) LSPDiagnosticsOnEditEnabled(subagent bool) bool {
	return c.LSPEnabledFor(subagent) && c.LSPDiagnosticsOnEdit != nil && *c.LSPDiagnosticsOnEdit
}

// AuthPath returns the path to auth.json.
func AuthPath() string { return filepath.Join(ZutHome(), "auth.json") }

// KimiCLIFallbackDisabledPath returns a sentinel that disables falling
// back to the official Kimi Code CLI token after `zut /logout kimi`.
func KimiCLIFallbackDisabledPath() string {
	return filepath.Join(ZutHome(), "kimi-cli-fallback-disabled")
}

// SessionsPath returns the directory holding session files.
func SessionsPath() string { return filepath.Join(ZutHome(), "sessions") }

// LogsPath returns the directory holding log files.
func LogsPath() string { return filepath.Join(ZutHome(), "logs") }

// LoadConfig reads the config file, returning defaults if missing.
func LoadConfig() (Config, error) {
	var c Config
	b, err := os.ReadFile(ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse config: %w", err)
	}
	if err := validateSubagentDurations(c.Subagents); err != nil {
		return c, err
	}
	return c, nil
}

func validateSubagentDurations(cfg SubagentsConfig) error {
	values := []struct {
		name  string
		value string
	}{
		{"queue_timeout", cfg.QueueTimeout},
		{"startup_timeout", cfg.StartupTimeout},
		{"default_timeout", cfg.DefaultTimeout},
		{"heartbeat_interval", cfg.HeartbeatInterval},
		{"idle_timeout", cfg.IdleTimeout},
		{"reconnect_timeout", cfg.ReconnectTimeout},
		{"cancel_grace_period", cfg.CancelGracePeriod},
	}
	for _, item := range values {
		value := strings.TrimSpace(item.value)
		if value == "" {
			continue
		}
		if _, err := time.ParseDuration(value); err != nil {
			return fmt.Errorf("parse config: subagents.%s: %w", item.name, err)
		}
	}
	return nil
}

// SaveConfig writes the config file, creating parent dirs.
func SaveConfig(c Config) error {
	if err := os.MkdirAll(ZutHome(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ConfigPath(), b, 0o644)
}

// applyConfiguredHTTPProxy makes the persisted proxy available to Go's
// standard HTTP transport and to child processes. Explicit environment
// variables take precedence over config.json.
func applyConfiguredHTTPProxy() {
	cfg, err := LoadConfig()
	if err != nil {
		return
	}
	proxy := strings.TrimSpace(cfg.HTTPProxy)
	if proxy == "" {
		return
	}
	if os.Getenv("HTTP_PROXY") == "" && os.Getenv("http_proxy") == "" {
		_ = os.Setenv("HTTP_PROXY", proxy)
	}
	if os.Getenv("HTTPS_PROXY") == "" && os.Getenv("https_proxy") == "" {
		_ = os.Setenv("HTTPS_PROXY", proxy)
	}
}

// AuthStoreFor returns the auth.Store backed by AuthPath().
func AuthStoreFor() *auth.Store { return auth.NewStore(AuthPath()) }

// ResolveLlamaCPPConfig resolves the router URL and optional API key. The
// environment overrides the credential stored through /login.
func ResolveLlamaCPPConfig() (baseURL, apiKey string, err error) {
	return resolveLlamaCPPConfig(context.Background(), apiKeyCommandExecute)
}

func resolveLlamaCPPConfig(ctx context.Context, commandMode apiKeyCommandMode) (baseURL, apiKey string, err error) {
	baseURL = strings.TrimSpace(os.Getenv("LLAMA_BASE_URL"))
	apiKey = strings.TrimSpace(os.Getenv("LLAMA_API_KEY"))
	if baseURL == "" {
		creds, loadErr := AuthStoreFor().Load()
		if loadErr != nil {
			return "", "", loadErr
		}
		if stored, ok := creds.AdditionalAPIKeyCreds[providerpkg.LlamaCPPProviderID]; ok {
			baseURL = stored.BaseURL
			if apiKey == "" {
				if stored.APIKeyCommand != nil && commandMode == apiKeyCommandSkip {
					return "", "", nil
				}
				if key, found, commandErr := resolveStoredAPIKey(ctx, providerpkg.LlamaCPPProviderID, stored, commandMode); found || commandErr != nil {
					if commandErr != nil {
						return "", "", commandErr
					}
					apiKey = key
				}
			}
		}
	}
	if baseURL == "" {
		return "", apiKey, nil
	}
	baseURL, err = providerpkg.NormalizeLlamaCPPURL(baseURL)
	return baseURL, apiKey, err
}

// ResolveCredential returns the credential (api key or oauth access
// token), the method ("apikey"/"oauth"), and an error when no
// credential is available.
//
// Lookup order:
//  1. explicit (e.g. --api-key): treated as API key
//  2. provider-specific env var: treated as API key
//  3. auth.json: api key OR oauth, whichever is present
func ResolveCredential(provider, explicit string) (cred, method string, err error) {
	cred, method, _, err = ResolveCredentialFull(provider, explicit)
	return cred, method, err
}

// ResolveCredentialContext is ResolveCredential with caller cancellation.
func ResolveCredentialContext(ctx context.Context, provider, explicit string) (cred, method string, err error) {
	cred, method, _, err = ResolveCredentialFullContext(ctx, provider, explicit)
	return cred, method, err
}

// ResolveCredentialFull is like ResolveCredential but also returns a
// provider-specific accountID when the credential is an OpenAI OAuth
// token (the ChatGPT account id extracted from the stored id_token).
// accountID is "" for API-key auth and for anthropic.
func ResolveCredentialFull(provider, explicit string) (cred, method, accountID string, err error) {
	return ResolveCredentialFullContext(context.Background(), provider, explicit)
}

// ResolveCredentialFullContext is ResolveCredentialFull with caller cancellation.
func ResolveCredentialFullContext(ctx context.Context, provider, explicit string) (cred, method, accountID string, err error) {
	return resolveCredentialFull(ctx, provider, explicit, apiKeyCommandExecute)
}

type apiKeyCommandMode uint8

const (
	apiKeyCommandExecute apiKeyCommandMode = iota
	apiKeyCommandAvailable
	apiKeyCommandSkip
)

func resolveCredentialFull(ctx context.Context, provider, explicit string, commandMode apiKeyCommandMode) (cred, method, accountID string, err error) {
	if explicit != "" {
		return explicit, "apikey", "", nil
	}
	switch provider {
	case "anthropic":
		// ANTHROPIC_OAUTH_TOKEN takes precedence over ANTHROPIC_API_KEY.
		// Useful when both are set and the user wants subscription auth
		// without editing auth.json.
		if v := os.Getenv("ANTHROPIC_OAUTH_TOKEN"); v != "" {
			return v, "oauth", "", nil
		}
		if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "openai":
		if v := os.Getenv("OPENAI_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "openai-codex":
		// ChatGPT/Codex subscription route. It intentionally ignores
		// OPENAI_API_KEY so users can keep both OpenAI API and Codex
		// subscription credentials configured and choose by provider.
	case "openai-responses":
		// Public OpenAI Responses API. Same env var as the chat-completions
		// `openai` provider; users pick the wire format by provider id.
		if v := os.Getenv("OPENAI_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "kimi":
		if v := os.Getenv("KIMI_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
		if v := os.Getenv("MOONSHOT_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "google":
		// Both env names are widely-used in the Google ecosystem;
		// GEMINI_API_KEY is the AI Studio default, GOOGLE_API_KEY
		// is the older / generic name. Either works.
		if v := os.Getenv("GEMINI_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
		if v := os.Getenv("GOOGLE_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "deepseek":
		if v := os.Getenv("DEEPSEEK_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "llama.cpp":
		baseURL, apiKey, resolveErr := resolveLlamaCPPConfig(ctx, commandMode)
		if resolveErr != nil {
			return "", "", "", resolveErr
		}
		if baseURL != "" {
			return firstNonEmpty(apiKey, "local"), "apikey", "", nil
		}
	case "moonshotai", "moonshotai-cn":
		// Moonshot direct API (separate from kimi-coding, which is the
		// Anthropic-Messages-fronted /coding endpoint with subscription OAuth).
		if v := os.Getenv("MOONSHOT_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "groq":
		if v := os.Getenv("GROQ_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "xai":
		if v := os.Getenv("XAI_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "cerebras":
		if v := os.Getenv("CEREBRAS_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "together":
		if v := os.Getenv("TOGETHER_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "huggingface":
		if v := os.Getenv("HF_TOKEN"); v != "" {
			return v, "apikey", "", nil
		}
	case "openrouter":
		if v := os.Getenv("OPENROUTER_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "mistral":
		if v := os.Getenv("MISTRAL_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "zai":
		if v := os.Getenv("ZAI_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "xiaomi", "xiaomi-token-plan-ams", "xiaomi-token-plan-cn", "xiaomi-token-plan-sgp":
		envVar := "XIAOMI_API_KEY"
		switch provider {
		case "xiaomi-token-plan-ams":
			envVar = "XIAOMI_TOKEN_PLAN_AMS_API_KEY"
		case "xiaomi-token-plan-cn":
			envVar = "XIAOMI_TOKEN_PLAN_CN_API_KEY"
		case "xiaomi-token-plan-sgp":
			envVar = "XIAOMI_TOKEN_PLAN_SGP_API_KEY"
		}
		if v := os.Getenv(envVar); v != "" {
			return v, "apikey", "", nil
		}
	case "minimax":
		if v := os.Getenv("MINIMAX_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "minimax-cn":
		if v := os.Getenv("MINIMAX_CN_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
		if v := os.Getenv("MINIMAX_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "fireworks":
		if v := os.Getenv("FIREWORKS_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "vercel-ai-gateway":
		if v := os.Getenv("AI_GATEWAY_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "opencode", "opencode-go":
		if v := os.Getenv("OPENCODE_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "github-copilot":
		if v := os.Getenv("COPILOT_GITHUB_TOKEN"); v != "" {
			return v, "apikey", "", nil
		}
		if v := os.Getenv("GITHUB_COPILOT_TOKEN"); v != "" {
			return v, "apikey", "", nil
		}
	case "cloudflare-workers-ai", "cloudflare-ai-gateway":
		if v := os.Getenv("CLOUDFLARE_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	case "amazon-bedrock":
		// Bedrock has many credential sources (AWS_PROFILE, IAM keys,
		// container creds, IRSA, bearer token). We surface a sentinel so
		// Resolve doesn't error on missing key; the real client (when
		// implemented) will resolve credentials through aws-sdk-go-v2.
		if os.Getenv("AWS_ACCESS_KEY_ID") != "" || os.Getenv("AWS_PROFILE") != "" ||
			os.Getenv("AWS_BEARER_TOKEN_BEDROCK") != "" {
			return "<aws>", "apikey", "", nil
		}
	case "google-vertex":
		if v := os.Getenv("GOOGLE_CLOUD_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
		// Check for Application Default Credentials (gcloud auth application-default login)
		// or service account JSON, matching the actual NewVertex client behavior.
		if os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "" {
			return "<adc>", "apikey", "", nil
		}
		// Check for the platform-specific default ADC path that gcloud writes to.
		if adcPath, err := auth.GoogleApplicationDefaultCredentialsPath(); err == nil {
			if _, err := os.Stat(adcPath); err == nil {
				return "<adc>", "apikey", "", nil
			}
		}
	case "azure-openai-responses":
		if v := os.Getenv("AZURE_OPENAI_API_KEY"); v != "" {
			return v, "apikey", "", nil
		}
	}
	// Generic env var fallback for custom providers: normalize the
	// provider id to a shell-friendly env var name (hyphens to
	// underscores) and check {NAME}_API_KEY before auth.json.
	if v := os.Getenv(normalizeCustomProviderEnvVar(provider) + "_API_KEY"); v != "" {
		return v, "apikey", "", nil
	}
	c, err := AuthStoreFor().Load()
	if err != nil {
		return "", "", "", err
	}
	if pc, ok := storedProviderCreds(c, provider); ok {
		if key, found, commandErr := resolveStoredAPIKey(ctx, provider, pc, commandMode); found || commandErr != nil {
			if commandErr != nil {
				return "", "", "", commandErr
			}
			return key, "apikey", "", nil
		}
		if provider == "xai" && pc.OAuth != nil && pc.OAuth.AccessToken != "" {
			tok, _ := refreshIfExpiredContext(ctx, provider, pc.OAuth)
			return tok.AccessToken, "oauth", "", nil
		}
	}
	switch provider {
	case "anthropic":
		if c.Anthropic.OAuth != nil && c.Anthropic.OAuth.AccessToken != "" {
			tok, _ := refreshIfExpiredContext(ctx, "anthropic", c.Anthropic.OAuth)
			return tok.AccessToken, "oauth", "", nil
		}
	case "openai-codex":
		if c.OpenAI.OAuth != nil && c.OpenAI.OAuth.AccessToken != "" {
			tok, _ := refreshIfExpiredContext(ctx, "openai", c.OpenAI.OAuth)
			return tok.AccessToken, "oauth", tok.AccountID, nil
		}
	case "kimi":
		if c.Kimi.OAuth != nil && c.Kimi.OAuth.AccessToken != "" {
			tok, _ := refreshIfExpiredContext(ctx, "kimi", c.Kimi.OAuth)
			return tok.AccessToken, "oauth", "", nil
		}
		if kimiCLIFallbackDisabled() {
			break
		}
		if tok := loadKimiCodeCLIToken(); tok != nil && tok.AccessToken != "" {
			tok, _ = refreshIfExpiredContext(ctx, "kimi", tok)
			return tok.AccessToken, "oauth", "", nil
		}
	case "github-copilot":
		if c.GithubCopilot.OAuth != nil && c.GithubCopilot.OAuth.AccessToken != "" {
			return c.GithubCopilot.OAuth.AccessToken, "oauth", "", nil
		}
	}
	return "", "", "", fmt.Errorf("no credential for %s", provider)
}

// CredentialAvailable reports whether a provider has a configured credential
// without executing an api_key_command.
func CredentialAvailable(provider string) bool {
	_, _, _, err := resolveCredentialFull(context.Background(), provider, "", apiKeyCommandAvailable)
	return err == nil
}

func resolveCredentialForBackground(ctx context.Context, provider string) (cred, method string, err error) {
	cred, method, _, err = resolveCredentialFull(ctx, provider, "", apiKeyCommandSkip)
	return cred, method, err
}

func storedProviderCreds(c auth.Credentials, provider string) (auth.ProviderCreds, bool) {
	if pc, ok := c.AdditionalAPIKeyCreds[provider]; ok {
		return pc, true
	}
	switch provider {
	case "anthropic":
		return c.Anthropic, true
	case "openai":
		return c.OpenAI, true
	case "kimi":
		return c.Kimi, true
	case "google":
		return c.Google, true
	case "deepseek":
		return c.DeepSeek, true
	case "github-copilot":
		return c.GithubCopilot, true
	default:
		return auth.ProviderCreds{}, false
	}
}

func resolveStoredAPIKey(ctx context.Context, provider string, creds auth.ProviderCreds, mode apiKeyCommandMode) (key string, found bool, err error) {
	if creds.APIKey != "" {
		return creds.APIKey, true, nil
	}
	if creds.APIKeyCommand == nil {
		return "", false, nil
	}
	switch mode {
	case apiKeyCommandAvailable:
		return "<api-key-command>", true, nil
	case apiKeyCommandSkip:
		return "", false, nil
	default:
		key, err := auth.ResolveAPIKeyCommand(ctx, *creds.APIKeyCommand)
		if err != nil {
			return "", true, fmt.Errorf("resolve api key for %s: %w", provider, err)
		}
		return key, true, nil
	}
}

func kimiCLIFallbackDisabled() bool {
	_, err := os.Stat(KimiCLIFallbackDisabledPath())
	return err == nil
}

func SetKimiCLIFallbackDisabled(disabled bool) error {
	path := KimiCLIFallbackDisabledPath()
	if !disabled {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("disabled\n"), 0o600)
}

// normalizeCustomProviderEnvVar converts a provider id such as
// "my-company" to "MY_COMPANY", matching the common convention for
// shell environment variables.
func normalizeCustomProviderEnvVar(provider string) string {
	provider = strings.ToUpper(provider)
	provider = strings.ReplaceAll(provider, "-", "_")
	return provider
}

func loadKimiCodeCLIToken() *auth.OAuthToken {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(home, ".kimi", "credentials", "kimi-code.json"))
	if err != nil {
		return nil
	}
	var raw struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		TokenType    string  `json:"token_type"`
		ExpiresAt    float64 `json:"expires_at"`
		Scope        string  `json:"scope"`
		ExpiresIn    float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &raw); err != nil || raw.AccessToken == "" {
		return nil
	}
	sec := int64(raw.ExpiresAt)
	nsec := int64((raw.ExpiresAt - float64(sec)) * 1e9)
	return &auth.OAuthToken{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		TokenType:    raw.TokenType,
		Scope:        raw.Scope,
		ClientID:     auth.KimiOAuth.ClientID,
		Expiry:       time.Unix(sec, nsec),
	}
}

// loadOAuthToken reads the current OAuth token from auth.json for the
// given provider. Returns nil if no token is stored.
func loadOAuthToken(providerName string) *auth.OAuthToken {
	c, err := AuthStoreFor().Load()
	if err != nil {
		return nil
	}
	switch providerName {
	case "anthropic":
		if c.Anthropic.OAuth != nil {
			return c.Anthropic.OAuth
		}
	case "openai":
		if c.OpenAI.OAuth != nil {
			return c.OpenAI.OAuth
		}
	case "kimi":
		if c.Kimi.OAuth != nil {
			return c.Kimi.OAuth
		}
		if kimiCLIFallbackDisabled() {
			return nil
		}
		return loadKimiCodeCLIToken()
	case "github-copilot":
		if c.GithubCopilot.OAuth != nil {
			return c.GithubCopilot.OAuth
		}
	}
	if pc, ok := c.AdditionalAPIKeyCreds[providerName]; ok {
		return pc.OAuth
	}
	return nil
}

// refreshIfExpired returns a usable OAuth token for the given provider,
// refreshing it synchronously when it's past (or near) expiry. The
// refreshed token is persisted to auth.json.
//
// Failures return the original token unchanged — the caller then makes
// a request with the stale access_token, which will 401. That's still
// better than crashing at credential-resolution time.
func refreshIfExpired(providerName string, tok *auth.OAuthToken) (*auth.OAuthToken, error) {
	return refreshIfExpiredContext(context.Background(), providerName, tok)
}

// refreshIfExpiredContext uses the caller's cancellation deadline while
// refreshing an expired OAuth token.
func refreshIfExpiredContext(parent context.Context, providerName string, tok *auth.OAuthToken) (*auth.OAuthToken, error) {
	if tok == nil {
		return &auth.OAuthToken{}, fmt.Errorf("nil token")
	}
	if !tok.Expired() {
		return tok, nil
	}
	if tok.RefreshToken == "" {
		return tok, fmt.Errorf("%s oauth token expired and no refresh_token available — run /login again", providerName)
	}

	if providerName == "xai" {
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		next, err := auth.RefreshXAIToken(ctx, tok.RefreshToken)
		if err != nil {
			return tok, fmt.Errorf("refresh %s: %w", providerName, err)
		}
		if next.RefreshToken == "" {
			next.RefreshToken = tok.RefreshToken
		}
		if err := AuthStoreFor().SetOAuth(providerName, *next); err != nil {
			return next, fmt.Errorf("persist refreshed token: %w", err)
		}
		return next, nil
	}

	var op auth.OAuthProvider
	switch providerName {
	case "anthropic":
		op = auth.AnthropicOAuth
	case "openai":
		op = auth.OpenAIOAuth
	case "kimi":
		op = auth.KimiOAuth
	default:
		return tok, fmt.Errorf("unknown provider %q", providerName)
	}

	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	next, err := op.Refresh(ctx, tok.RefreshToken)
	if err != nil {
		return tok, fmt.Errorf("refresh %s: %w", providerName, err)
	}
	// Preserve the refresh token if the server omitted it (Anthropic often does).
	if next.RefreshToken == "" {
		next.RefreshToken = tok.RefreshToken
	}
	// Carry over account id (openai) / id_token across refreshes.
	if next.AccountID == "" {
		next.AccountID = tok.AccountID
	}
	if next.IDToken == "" {
		next.IDToken = tok.IDToken
	}
	if err := AuthStoreFor().SetOAuth(providerName, *next); err != nil {
		return next, fmt.Errorf("persist refreshed token: %w", err)
	}
	return next, nil
}
