package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	zutdocs "github.com/bnema/zut"
	"github.com/bnema/zut/packages/agent/lsp"
	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// ContextFile is an instruction file loaded into the system prompt.
type ContextFile struct {
	Path    string
	Content string
}

// Resolved is the effective configuration after merging CLI, config, defaults.
type Resolved struct {
	Provider        string
	Model           string
	Credential      string // api key or oauth access token
	AuthMethod      string // "apikey" | "oauth" | "" (no credential yet)
	AccountID       string // ChatGPT account id (for openai oauth), "" otherwise
	BaseURL         string
	InsecureTLS     bool
	CWD             string
	Reasoning       string
	Temperature     *float32
	FastMode        bool
	WebSearchPolicy subagents.WebSearchPolicy

	ToolRegistry core.Registry
	ToolSummary  []ToolSummary
	SystemPrompt string
	MaxSteps     int
	Sandbox      *tools.Sandbox

	// ContextWindow and MaxOutput retain the effective model metadata,
	// including metadata synthesized for valid open-catalog local and routed
	// models that provider.FindModel cannot rediscover later.
	ContextWindow int

	// MaxOutput is the resolved model's maximum output-token budget
	// (from the catalog). Passed to the agent so each turn requests
	// the model's full output capacity instead of the provider's
	// conservative default (e.g. Bedrock's 4096, which truncates
	// long writes/edits with stopReason=length).
	MaxOutput int

	// SkillTool is the on-demand skill loader registered with the
	// agent's tool registry, or nil if no SKILL.md files were
	// discovered. Exposed so the tui can list / preview skills.
	SkillTool *skills.Tool

	// ContextFiles records the AGENTS.md files appended to SystemPrompt,
	// in effective load order. Interactive mode uses this metadata to make
	// otherwise invisible startup context inspectable without adding fake
	// messages to the provider transcript.
	ContextFiles []ContextFile

	// skillsEnabled is true when discovery ran, even when it found no
	// skills. It is false only for an explicit clean-room disable.
	skillsEnabled bool

	// Bookkeeping for MergeExtensionTools. Captured at Resolve time
	// so the system prompt can be rebuilt later without re-running
	// resolve.
	systemAppend     []string
	systemCustom     string
	skillAddendum    string
	toolDescriptions map[string]string
}

// HasCredential reports whether a credential was resolved.
func (r Resolved) HasCredential() bool { return r.Credential != "" }

// MergeExtensionTools folds every tool registered by an extension
// into r's ToolRegistry and re-renders the system prompt's tool
// summary so the model sees both built-in and extension tools.
//
// Idempotent: calling twice with the same manager state has no
// effect on the second pass (existing names are preserved). Built-in
// tools always win on conflict.
func (r *Resolved) MergeExtensionTools(mgr ExtensionToolSource) {
	if mgr == nil {
		return
	}
	changed := false
	for _, info := range mgr.Tools() {
		// web_search remains a reserved native-tool name even when its
		// capability policy excludes the current session. An extension must
		// not turn a normal CLI opt-out into a differently implemented search
		// capability with the same model-visible name.
		if info.Name == "web_search" {
			continue
		}
		if _, exists := r.ToolRegistry[info.Name]; exists {
			continue
		}
		r.ToolRegistry[info.Name] = mgr.NewExtensionTool(info)
		changed = true
	}

	// A nil SkillTool can mean discovery ran and found nothing, so use the
	// explicit disable flag rather than the discovery result as the gate.
	if source, ok := mgr.(ExtensionSkillSource); ok && r.skillsEnabled {
		if bundled := source.Skills(); len(bundled) > 0 {
			merged := mergeExtensionSkills(r.SkillTool, bundled)
			if r.SkillTool == nil {
				r.SkillTool = skills.NewTool(merged)
				r.ToolRegistry[r.SkillTool.Name()] = r.SkillTool
			} else {
				r.SkillTool.SetSkills(merged)
			}
			append_ := append([]string(nil), r.systemAppend...)
			if r.skillAddendum != "" {
				filtered := append_[:0]
				for _, item := range append_ {
					if item != r.skillAddendum {
						filtered = append(filtered, item)
					}
				}
				append_ = filtered
			}
			r.skillAddendum = skills.SystemPromptAddendum(merged)
			if r.skillAddendum != "" {
				append_ = append(append_, r.skillAddendum)
			}
			r.systemAppend = append_
			changed = true
		}
	}
	if !changed {
		return
	}
	// Re-render the system prompt with the merged tool and skill lists.
	r.ToolSummary = toolSummariesFromRegistry(r.ToolRegistry, r.toolDescriptions)
	r.SystemPrompt = BuildSystemPrompt(SystemPromptOpts{
		CWD:        r.CWD,
		Tools:      r.ToolSummary,
		Custom:     r.systemCustom,
		Append:     r.systemAppend,
		ZutDocsDir: filepath.Join(ZutHome(), "docs"),
	})
}

func mergeExtensionSkills(tool *skills.Tool, bundled []*skills.Skill) []*skills.Skill {
	var existing []*skills.Skill
	if tool != nil {
		existing = tool.Skills()
	}
	merged := make([]*skills.Skill, 0, len(existing)+len(bundled))
	seen := map[string]bool{}
	add := func(list []*skills.Skill) {
		for _, skill := range list {
			if skill == nil || skill.Name == "" || seen[skill.Name] {
				continue
			}
			seen[skill.Name] = true
			merged = append(merged, skill)
		}
	}
	var user, builtins []*skills.Skill
	for _, skill := range existing {
		if skill == nil || strings.HasPrefix(skill.Source, "extension ") {
			continue
		}
		if skill.Builtin {
			builtins = append(builtins, skill)
		} else {
			user = append(user, skill)
		}
	}
	add(user)
	add(bundled)
	add(builtins)
	return merged
}

// ExtensionToolSource is the slice of the extension manager that
// MergeExtensionTools needs. Lives here as an interface so the
// build package doesn't import packages/agent/extensions (which
// imports core, which imports... avoid the cycle).
type ExtensionToolSource interface {
	Tools() []ExtensionToolInfo
	NewExtensionTool(info ExtensionToolInfo) core.Tool
}

// ExtensionSkillSource exposes the optional bundled-skill surface without
// forcing older ExtensionToolSource implementations to add it.
type ExtensionSkillSource interface {
	Skills() []*skills.Skill
}

// ExtensionToolInfo mirrors extensions.ToolInfo so we can declare
// ExtensionToolSource here without importing the extensions
// package. The cli wires a tiny adapter to bridge them.
type ExtensionToolInfo struct {
	Extension   string
	Name        string
	Description string
	Schema      []byte
	Deferred    bool
}

// toolSummariesFromRegistry rebuilds the system-prompt tool list
// from a (possibly extended) registry, using cached descriptions for
// the human-readable summary text.
func toolSummariesFromRegistry(reg core.Registry, cached map[string]string) []ToolSummary {
	out := make([]ToolSummary, 0, len(reg))
	seen := make(map[string]bool, len(nativeToolSummaryOrder))
	appendSummary := func(name string) {
		t, ok := reg[name]
		if !ok {
			return
		}
		seen[name] = true
		if d, ok := t.(interface{ Deferred() bool }); ok && d.Deferred() {
			return
		}
		desc := t.Description()
		if d, ok := cached[name]; ok && d != "" {
			desc = d
		}
		out = append(out, ToolSummary{Name: name, Description: desc})
	}
	for _, name := range nativeToolSummaryOrder {
		appendSummary(name)
	}
	remaining := make([]string, 0, len(reg)-len(seen))
	for name := range reg {
		if !seen[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	for _, name := range remaining {
		appendSummary(name)
	}
	return out
}

// defaultModelForProvider returns the model id zut prefers when the
// caller didn't pick one. Mirrors the per-provider switch used at
// multiple points in Resolve; centralised so the unknown-model
// recovery path and the no-model-configured path can't drift.
//
// Returns the empty string for "ollama", which has no built-in
// default — the caller is expected to special-case ollama and
// error or use whatever the user passed.
func defaultModelForProvider(prov string) string {
	switch prov {
	case "openai":
		return "gpt-5"
	case "openai-codex":
		return "gpt-5.5"
	case "openai-responses":
		return "gpt-5"
	case "kimi":
		return "kimi-for-coding"
	case "deepseek":
		return "deepseek-v4-pro"
	case "google":
		return "gemini-2.5-pro"
	case "ollama", provider.LlamaCPPProviderID:
		return ""
	case "moonshotai", "moonshotai-cn":
		return "kimi-k2.6"
	case "cerebras":
		return "qwen-3-235b-a22b-instruct-2507"
	case "groq":
		return "llama-3.3-70b-versatile"
	case "xai":
		return "grok-4.5"
	case "together":
		return "Qwen/Qwen3-Coder-480B-A35B-Instruct"
	case "huggingface":
		return "moonshotai/Kimi-K2-Instruct"
	case "openrouter":
		return "anthropic/claude-sonnet-4.5"
	case "mistral":
		return "mistral-large-latest"
	case "zai":
		return "glm-4.7"
	case "xiaomi", "xiaomi-token-plan-ams", "xiaomi-token-plan-cn", "xiaomi-token-plan-sgp":
		return "mimo-v2.5"
	case "minimax", "minimax-cn":
		return "MiniMax-M2.7"
	case "fireworks":
		return "accounts/fireworks/models/kimi-k2p6"
	case "vercel-ai-gateway":
		return "anthropic/claude-sonnet-4.5"
	case "opencode":
		return "claude-sonnet-4-5"
	case "opencode-go":
		return "kimi-k2.6"
	case "amazon-bedrock":
		return "anthropic.claude-sonnet-4-5-20250929-v1:0"
	case "google-vertex":
		return "gemini-2.5-pro"
	case "azure-openai-responses":
		return "gpt-5"
	case "github-copilot":
		return "claude-sonnet-4.5"
	default:
		// Custom providers: pick the first model from the catalog for
		// that provider, or fall back to the global default.
		if models := provider.ModelsForProvider(prov); len(models) > 0 {
			return models[0].ID
		}
		return provider.DefaultModel.ID
	}
}

// knownProviders is the set of provider ids zut recognises. Used by
// Resolve to validate args.Provider, by extension-callers, and by the
// auto-fallback logic that picks any logged-in provider when the user's
// preferred one has no credentials.
var knownProviders = []string{
	"anthropic", "openai", "openai-codex", "openai-responses", "kimi", "deepseek", "google", "ollama", provider.LlamaCPPProviderID,
	"moonshotai", "moonshotai-cn",
	"cerebras", "groq", "xai", "together", "huggingface", "openrouter",
	"mistral", "zai",
	"xiaomi", "xiaomi-token-plan-ams", "xiaomi-token-plan-cn", "xiaomi-token-plan-sgp",
	"minimax", "minimax-cn",
	"fireworks", "vercel-ai-gateway",
	"opencode", "opencode-go",
	"amazon-bedrock", "google-vertex", "azure-openai-responses",
	"github-copilot", "cloudflare-workers-ai", "cloudflare-ai-gateway",
}

func isKnownProvider(name string) bool {
	for _, p := range knownProviders {
		if p == name {
			return true
		}
	}
	_, ok := provider.CustomProviders()[name]
	return ok
}

// isBuiltinProvider reports whether name is in the hardcoded
// knownProviders list (not a user-defined custom provider).
func isBuiltinProvider(name string) bool {
	for _, p := range knownProviders {
		if p == name {
			return true
		}
	}
	return false
}

// providerAliases maps common short / alternate provider names to the
// canonical id in knownProviders. Users (and other agents) reach for
// "bedrock" or "vertex" far more naturally than the fully-qualified
// "amazon-bedrock" / "google-vertex"; without this mapping an alias is
// treated as an unknown provider and Resolve silently falls back to
// anthropic, producing a misleading "no credential for anthropic" error
// after the user explicitly picked, say, bedrock.
var providerAliases = map[string]string{
	"bedrock":      "amazon-bedrock",
	"aws-bedrock":  "amazon-bedrock",
	"amazon":       "amazon-bedrock",
	"vertex":       "google-vertex",
	"gcp-vertex":   "google-vertex",
	"gemini":       "google",
	"googleai":     "google",
	"google-ai":    "google",
	"azure":        "azure-openai-responses",
	"azure-openai": "azure-openai-responses",
	"copilot":      "github-copilot",
	"github":       "github-copilot",
	"codex":        "openai-codex",
	"moonshot":     "moonshotai",
	"kimi-code":    "kimi",
	"ai-gateway":   "vercel-ai-gateway",
	"vercel":       "vercel-ai-gateway",
	"cloudflare":   "cloudflare-workers-ai",
	"workers-ai":   "cloudflare-workers-ai",
	"hf":           "huggingface",
	"llama":        provider.LlamaCPPProviderID,
	"llamacpp":     provider.LlamaCPPProviderID,
}

// canonicalProvider normalises a user-supplied provider name: trims
// surrounding whitespace, lower-cases it, and resolves any known alias
// to its canonical id. Unknown names are returned trimmed/lower-cased
// and unchanged so the existing unknown-provider handling still runs.
func canonicalProvider(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return n
	}
	if canon, ok := providerAliases[n]; ok {
		return canon
	}
	return n
}

func findSubagentProfile(cwd, name string) (*subagents.Profile, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	home, _ := os.UserHomeDir()
	profiles, discoveryErrors := subagents.Discover(cwd, home)
	if len(discoveryErrors) > 0 {
		return nil, fmt.Errorf("discover subagent profiles: %d profile discovery error(s)", len(discoveryErrors))
	}
	profile := subagents.Find(profiles, name)
	if profile == nil {
		return nil, fmt.Errorf("subagent profile %q not found (looked in ~/.agents/agents and compatibility locations)", name)
	}
	return profile, nil
}

func applySubagentProfile(args *Args, profile *subagents.Profile) {
	if args == nil || profile == nil {
		return
	}
	profileProvider, profileModel := profile.ModelSelection()
	if args.Provider == "" && profileProvider != "" {
		args.Provider = profileProvider
	}
	if args.Model == "" && profileModel != "" {
		args.Model = profileModel
	}
	if args.Reasoning == "" && profile.Thinking != "" {
		// Keep an explicit "off" value non-empty so it overrides a
		// persisted global reasoning setting when the child resolves.
		args.Reasoning = strings.TrimSpace(profile.Thinking)
	}
	if !args.NoTools && !args.ToolsSet && len(args.Tools) == 0 && len(profile.Tools) > 0 {
		args.Tools = append([]string(nil), profile.Tools...)
	}
	if profile.InheritSkills != nil && !*profile.InheritSkills {
		args.NoSkill = true
	}
}

// resolveWebSearchPolicy applies capability precedence at the owning resolver
// boundary. Permission, tool-list, and named-profile restrictions are ceilings
// on every caller-provided policy. Workers additionally require an explicit,
// valid propagated decision rather than falling back to their local config.
func resolveWebSearchPolicy(args Args, cfg Config, cfgErr error, profile *subagents.Profile) subagents.WebSearchPolicy {
	if args.NoTools || args.PermissionSet != nil {
		return subagents.WebSearchDeny
	}
	if args.ToolsSet && !toolListContains(args.Tools, "web_search") {
		return subagents.WebSearchDeny
	}
	if profile != nil && subagents.NamedWebSearchPolicy(profile.Tools) != subagents.WebSearchAllow {
		return subagents.WebSearchDeny
	}

	switch args.WebSearchPolicy {
	case subagents.WebSearchDeny:
		return subagents.WebSearchDeny
	case subagents.WebSearchAllow:
		return subagents.WebSearchAllow
	case subagents.WebSearchInherit:
		// Continue through normal-mode defaults below. Inherit is not a valid
		// propagated worker decision.
	default:
		return subagents.WebSearchDeny
	}
	if args.Mode == ModeSubagentWorker {
		return subagents.WebSearchDeny
	}
	if args.ToolsSet {
		return subagents.WebSearchAllow
	}
	if cfgErr != nil {
		return subagents.WebSearchDeny
	}
	if profile != nil {
		return subagents.WebSearchAllow
	}
	if cfg.WebSearchEnabledForCLI() {
		return subagents.WebSearchAllow
	}
	return subagents.WebSearchDeny
}

func toolListContains(names []string, target string) bool {
	for _, name := range names {
		if name == target {
			return true
		}
	}
	return false
}

// webSearchAllowedForRegistry is kept separate from the resolver so direct
// registry tests and callers that build a registry without persisted config
// still get the normal default-on behavior. Worker registries fail closed if
// they have not first received a resolved allow decision.
func webSearchAllowedForRegistry(args Args) bool {
	if args.NoTools || args.PermissionSet != nil {
		return false
	}
	if args.ToolsSet && !toolListContains(args.Tools, "web_search") {
		return false
	}
	switch args.WebSearchPolicy {
	case subagents.WebSearchAllow:
		return true
	case subagents.WebSearchDeny:
		return false
	default:
		return args.Mode != ModeSubagentWorker
	}
}

// Resolve merges args, config, and env into a Resolved set.
//
// Unlike the earlier version, Resolve NEVER returns an error for
// missing credentials: the TUI can start without them and launch a
// login flow. requireCred controls whether missing credentials are a
// hard error (used by print/json modes).
func Resolve(args Args, requireCred bool) (Resolved, error) {
	cfg, cfgErr := LoadConfig()
	// Programmatic callers historically supplied Tools without the parser's
	// provenance bit. Treat a non-empty list as explicit before a named profile
	// can contribute its defaults.
	if len(args.Tools) > 0 {
		args.ToolsSet = true
	}
	var selectedProfile *subagents.Profile
	if strings.TrimSpace(args.Subagent) != "" {
		var err error
		selectedProfile, err = findSubagentProfile(args.CWD, args.Subagent)
		if err != nil {
			return Resolved{}, err
		}
		applySubagentProfile(&args, selectedProfile)
	}
	webSearchPolicy := resolveWebSearchPolicy(args, cfg, cfgErr, selectedProfile)
	args.WebSearchPolicy = webSearchPolicy

	// User-requested provider (explicit > config > default).
	// Normalise common aliases (e.g. "bedrock" -> "amazon-bedrock")
	// before validation so an alias is never mistaken for an unknown
	// provider and silently downgraded to anthropic.
	argProvider := canonicalProvider(args.Provider)
	provName := firstNonEmpty(argProvider, canonicalProvider(cfg.Provider), "anthropic")
	if !isKnownProvider(provName) {
		// Unknown provider (maybe removed or renamed). Fall back to
		// the first provider that has credentials, or anthropic.
		// Custom providers (from models.json) are already accepted
		// by isKnownProvider, so we only reach here for truly unknown
		// names.
		provName = "anthropic"
		if CredentialAvailable("openai") {
			provName = "openai"
		}
		if CredentialAvailable("openai-codex") {
			provName = "openai-codex"
		}
		if CredentialAvailable("kimi") {
			provName = "kimi"
		}
		if CredentialAvailable("deepseek") {
			provName = "deepseek"
		}
		if CredentialAvailable("anthropic") {
			provName = "anthropic"
		}
		// Reset the saved config so this doesn't keep happening.
		cfg.Provider = provName
		cfg.Model = ""
		_ = SaveConfig(cfg)
	}

	var (
		cred      string
		method    string
		accountID string
		credErr   error
	)
	if args.inheritedCredential != "" {
		cred = args.inheritedCredential
		method = args.inheritedAuthMethod
		accountID = args.inheritedAccountID
	} else if provName == "ollama" {
		cred = firstNonEmpty(args.APIKey, "ollama")
		method = "apikey"
	} else {
		cred, method, accountID, credErr = ResolveCredentialFull(provName, args.APIKey)
	}

	// Persist --api-key for custom providers so subsequent runs don't
	// need to pass it again.
	if !isBuiltinProvider(provName) && args.APIKey != "" {
		if store := AuthStoreFor(); store != nil {
			_ = store.SetAPIKey(provName, args.APIKey)
		}
	}

	// If the user did NOT explicitly pick a provider (neither via --provider
	// nor by saving one in config.json) and the default one has no
	// credentials, auto-fall-back to whichever provider is actually logged
	// in. That way running plain `zut` after `/login` (any provider) never
	// shows a "not logged in" banner.
	//
	// Critical: when the user HAS saved a provider in config.json (e.g.
	// "deepseek" with "deepseek-v4-flash"), that choice is respected even
	// if no credential is found — the tui will show the login dialog or
	// surface a clear credential error rather than silently switching to
	// a completely different provider and model.
	// autoFellBack tracks whether the provider was changed by the
	// auto-fallback loop (no explicit provider from CLI or config,
	// and the default had no credentials). When false, the provider
	// came from the user's explicit choice (--provider or config.json)
	// and must be respected, including any model id that may belong
	// to a different provider in the catalog — for example when using
	// a gateway/router like OpenRouter with "deepseek/deepseek-v4-flash".
	var autoFellBack bool
	userPickedProvider := args.Provider != "" || cfg.Provider != ""
	if credErr != nil && !userPickedProvider && provName != "ollama" {
		autoFellBack = true
		// Scan every known provider (not a hardcoded subset) so any
		// env-based credential is discovered, e.g. an env-only
		// amazon-bedrock setup (AWS_BEARER_TOKEN_BEDROCK / AWS_PROFILE /
		// IAM keys) when no config.json pins the provider, such as after
		// pointing ZUT_HOME at a fresh home dir. Iteration order of
		// knownProviders defines fallback priority. Local providers without a
		// default model are skipped because selecting either one here would
		// fail before the user can choose a model.
		for _, other := range knownProviders {
			if other == provName || other == "ollama" || other == provider.LlamaCPPProviderID {
				continue
			}
			if !CredentialAvailable(other) {
				continue
			}
			// An explicit launch key belongs to the initially selected provider;
			// automatic fallback must never reuse it for another provider.
			c, m, a, err := ResolveCredentialFull(other, "")
			provName = other
			cred, method, accountID, credErr = c, m, a, err
			break
		}
	}

	model := firstNonEmpty(args.Model, cfg.Model)
	if model == "" {
		if provName == "ollama" || provName == provider.LlamaCPPProviderID {
			return Resolved{}, fmt.Errorf("%s requires --model or a model selected from its manager", provName)
		}
		model = defaultModelForProvider(provName)
	}
	// If the resolved model belongs to a different provider (e.g. config
	// says gpt-5 but we auto-fell back to anthropic), pick that provider's
	// default. This override only fires when the provider was auto-fallback-
	// changed, NOT when the user explicitly configured the pair — gateway/
	// router providers (openrouter, vercel-ai-gateway, etc.) can serve
	// models from any provider in the catalog.
	if autoFellBack && provName != "ollama" {
		if m, err := provider.FindModel("", model); err == nil && m.Provider != provName {
			model = defaultModelForProvider(provName)
		}
	}
	resolvedModel, err := provider.FindModel(provName, model)
	if err != nil && (provName == "ollama" || provName == provider.LlamaCPPProviderID) {
		// Local providers are intentionally open-catalogue: any model id the
		// configured server understands is valid, even if it is not cached.
		resolvedModel = provider.Model{
			Provider:      provName,
			ID:            model,
			DisplayName:   model,
			ContextWindow: 128000,
			MaxOutput:     16384,
			BaseURL:       args.BaseURL,
			Source:        provName,
		}
		err = nil
	}
	// Custom providers are open-catalogue like ollama: any model id the
	// endpoint understands is valid. Use the provider-level base URL.
	if err != nil {
		if cfg, ok := provider.CustomProviders()[provName]; ok {
			resolvedModel = provider.Model{
				Provider:      provName,
				ID:            model,
				DisplayName:   model,
				ContextWindow: 128000,
				MaxOutput:     16384,
				BaseURL:       cfg.BaseURL,
				Source:        "user",
			}
			err = nil
		}
	}
	if cfg, ok := provider.CustomProviders()[provName]; ok && err == nil && resolvedModel.BaseURL == "" && cfg.BaseURL != "" {
		// Fall back to the provider-level base URL when the model does
		// not define its own endpoint.
		resolvedModel.BaseURL = cfg.BaseURL
	}
	if err != nil {
		// The model the user (or persisted config) asked for is no
		// longer in the active catalogue — they probably removed it
		// from their models.json or upgraded zut and the id changed.
		// Refusing to launch is the wrong move: it strands the user
		// with no way to even open the TUI and pick a new model.
		// Fall back to the provider's default, warn on stderr, and,
		// when the stale id came from the persisted config (not an
		// explicit --model flag), repair the config so the warning
		// doesn't repeat on every launch.

		// Gateway providers can accept route-qualified ids that are not in
		// zut's local catalog yet, for example OpenRouter's
		// "deepseek/deepseek-v4-flash". Preserve only route-qualified ids;
		// plain unknown values are likely typos and should still fall back.
		if isGatewayProvider(provName) && isGatewayRoutedModelID(model) {
			resolvedModel = provider.Model{
				Provider:      provName,
				ID:            model,
				DisplayName:   model,
				ContextWindow: 1000000,
				MaxOutput:     64000,
				BaseURL:       "",
				Source:        "gateway",
			}
			// Don't repair config — the routed model id may be valid upstream.
		} else {
			fallback := defaultModelForProvider(provName)
			fm, ferr := provider.FindModel(provName, fallback)
			if ferr != nil {
				// Even the provider default is gone (catastrophic
				// catalogue trim). Last resort: any model on this
				// provider, then the global DefaultModel.
				if candidates := provider.ModelsForProvider(provName); len(candidates) > 0 {
					fm = candidates[0]
				} else {
					fm = provider.DefaultModel
				}
			}
			fmt.Fprintf(os.Stderr,
				"zut: model %q is not in the active catalogue; using %q instead. Pick a different model with --model or /model.\n",
				model, fm.ID)
			if args.Model == "" && cfg.Model == model {
				cfg.Model = fm.ID
				_ = SaveConfig(cfg)
			}
			resolvedModel = fm
			model = fm.ID
		}
	}

	explicitBaseURL := args.BaseURL != "" || (resolvedModel.Source == "user" && resolvedModel.BaseURL != "")

	// If the model defines a base URL (e.g. local ollama) and the
	// user didn't pass --base-url, use the model's URL. For ollama,
	// keep http://localhost:11434 as a fallback only after the model
	// metadata has had a chance to provide a custom baseUrl.
	if args.BaseURL == "" && resolvedModel.BaseURL != "" {
		args.BaseURL = resolvedModel.BaseURL
	}
	if args.BaseURL == "" && provName == "ollama" {
		args.BaseURL = "http://localhost:11434"
	}
	if args.BaseURL == "" && provName == provider.LlamaCPPProviderID {
		managementURL, _, configErr := ResolveLlamaCPPConfig()
		if configErr != nil {
			return Resolved{}, configErr
		}
		var baseErr error
		args.BaseURL, baseErr = provider.LlamaCPPInferenceURL(managementURL)
		if baseErr != nil {
			return Resolved{}, baseErr
		}
	}

	// Insecure TLS is intentionally scoped to explicit custom endpoints.
	// Built-in provider base URLs, auth calls, and model discovery keep normal
	// certificate verification even when --insecure is present.
	insecureTLS := (args.InsecureTLS || cfg.Insecure) && explicitBaseURL

	// If the model has a base URL, credentials are optional (local
	// models like ollama don't need real API keys).
	if resolvedModel.BaseURL != "" && credErr != nil {
		cred = "ollama"
		credErr = nil
		requireCred = false
	}

	if credErr != nil && requireCred {
		return Resolved{}, fmt.Errorf("%w; set %s_API_KEY, pass --api-key, or run `zut` and /login",
			credErr, envVarName(provName))
	}

	sandbox := tools.NewSandbox(args.CWD)
	if cfg.JailByDefault != nil && *cfg.JailByDefault {
		sandbox.Lock()
	}
	if args.PermissionSet != nil {
		sandbox.SetPermissions(args.PermissionSet)
	}
	subagentSession := args.Mode == ModeSubagentWorker || strings.TrimSpace(args.Subagent) != ""
	lspEnabled := !args.NoLSP && cfg.LSPEnabledFor(subagentSession)
	reg := buildToolRegistry(args, args.CWD, sandbox, lspEnabled, cfg.LSPDiagnosticsOnWriteEnabled(subagentSession), cfg.LSPDiagnosticsOnEditEnabled(subagentSession))

	docsDir, _ := zutdocs.EnsureInstalled(ZutHome())

	// Skill discovery: scan project + global locations + built-in
	// skills shipped with the binary. If any are found, register
	// the on-demand `skill` loader tool plus a system-prompt
	// manifest so the model knows what's available.
	//
	// --no-skill bypasses the entire mechanism: no manifest in the
	// system prompt, no `skill` tool in the registry. Useful for a
	// clean-room run with zero extra context biasing the model.
	var (
		discovered    []*skills.Skill
		skillTool     *skills.Tool
		skillAddendum string
		skillsEnabled bool
	)
	if !args.NoSkill && (selectedProfile == nil || selectedProfile.InheritSkills == nil || *selectedProfile.InheritSkills) {
		skillsEnabled = true
		homeDir, _ := os.UserHomeDir()
		discovered, _ = skills.Discover(ZutHome(), args.CWD, homeDir, args.WithSkills)
		if len(discovered) > 0 {
			skillTool = skills.NewTool(discovered)
			reg[skillTool.Name()] = skillTool
			skillAddendum = skills.SystemPromptAddendum(discovered)
		}
	}
	_ = skillTool

	summaries := toolSummaries(reg, args)

	contextFiles := []ContextFile(nil)
	if selectedProfile == nil || selectedProfile.InheritProjectContext == nil || *selectedProfile.InheritProjectContext {
		contextFiles = loadAgentsContext(args.CWD, ZutHome())
	}
	append_ := []string(nil)
	if cfg.PonytailModeEnabled() {
		if ponytailAddendum := PonytailSystemAddendum(); ponytailAddendum != "" {
			append_ = append(append_, ponytailAddendum)
		}
	}
	append_ = append(append_, args.AppendSystemPrompt...)
	if agentsAddendum := formatAgentsContext(contextFiles); agentsAddendum != "" {
		append_ = append(append_, agentsAddendum)
	}
	if skillAddendum != "" {
		append_ = append(append_, skillAddendum)
	}
	interactiveMode := args.Mode == "" || args.Mode == ModeInteractive
	primaryInteractive := selectedProfile == nil && interactiveMode
	primaryOrchestrator := selectedProfile == nil && args.Orchestrate
	if (primaryInteractive || primaryOrchestrator) && autoSubagentsToolAllowed(args) {
		homeDir, _ := os.UserHomeDir()
		profiles, _ := subagents.Discover(args.CWD, homeDir)
		if subagentsAddendum := subagents.SystemPromptAddendum(profiles); subagentsAddendum != "" {
			append_ = append(append_, subagentsAddendum)
		}
	}
	if primaryOrchestrator {
		// Headless orchestration owns its strict prompt contract rather than
		// inheriting the interactive setting.
		append_ = append(append_, AutoSubagentsSystemAddendumFor(
			autoSubagentsToolAllowed(args),
			autoSubagentsStopToolAllowed(args),
			autoSubagentsResumeToolAllowed(args),
		))
	} else if primaryInteractive && cfg.AutoSubagentsEnabled != nil && *cfg.AutoSubagentsEnabled {
		append_ = append(append_, AutoSubagentsSystemAddendumFor(
			autoSubagentsToolAllowed(args),
			autoSubagentsStopToolAllowed(args),
			autoSubagentsResumeToolAllowed(args),
		))
	} else if primaryInteractive && autoSubagentsAnyToolAllowed(args) {
		append_ = append(append_, OnDemandSubagentsSystemAddendum)
	}
	if selectedProfile != nil && selectedProfile.SystemPromptMode != "replace" && selectedProfile.SystemPrompt != "" {
		append_ = append(append_, selectedProfile.SystemPrompt)
	}

	// Custom system prompt resolution order:
	//   1. --system-prompt flag (highest priority; ad-hoc per run)
	//   2. $ZUT_HOME/SYSTEM.md (persistent user override)
	//   3. built-in default (defaultIdentity + defaultGuidelines)
	custom := args.SystemPrompt
	if selectedProfile != nil && selectedProfile.SystemPromptMode == "replace" {
		custom = selectedProfile.SystemPrompt
	} else if custom == "" {
		custom = readUserSystemPrompt(ZutHome())
	}

	sys := BuildSystemPrompt(SystemPromptOpts{
		CWD:        args.CWD,
		Tools:      summaries,
		Custom:     custom,
		Append:     append_,
		ZutDocsDir: docsDir,
	})

	reasoning := provider.NormalizeReasoning(firstNonEmpty(args.Reasoning, cfg.Reasoning))
	temperature := args.Temperature
	if temperature == nil {
		temperature = cfg.Temperature
	}
	fastMode := cfg.FastMode != nil && *cfg.FastMode
	if args.FastModeSet {
		fastMode = args.FastMode
	}
	// A profile's explicit fastMode setting overrides the global setting
	// for this child. This preserves fastMode: false as an opt-out while
	// allowing fastMode: true to request the fast tier for a focused worker.
	if selectedProfile != nil && selectedProfile.FastMode != nil {
		fastMode = *selectedProfile.FastMode
	}

	max := args.MaxSteps // 0 = unlimited

	return Resolved{
		Provider:         provName,
		Model:            model,
		Credential:       cred,
		AuthMethod:       method,
		AccountID:        accountID,
		BaseURL:          args.BaseURL,
		InsecureTLS:      insecureTLS,
		CWD:              args.CWD,
		Reasoning:        reasoning,
		Temperature:      temperature,
		FastMode:         fastMode,
		WebSearchPolicy:  webSearchPolicy,
		ToolRegistry:     reg,
		ToolSummary:      summaries,
		SystemPrompt:     sys,
		MaxSteps:         max,
		ContextWindow:    resolvedModel.ContextWindow,
		MaxOutput:        resolvedModel.MaxOutput,
		Sandbox:          sandbox,
		SkillTool:        skillTool,
		skillsEnabled:    skillsEnabled,
		ContextFiles:     contextFiles,
		systemAppend:     append_,
		systemCustom:     custom,
		skillAddendum:    skillAddendum,
		toolDescriptions: descMapFromSummaries(summaries),
	}, nil
}

// readUserSystemPrompt looks for $ZUT_HOME/SYSTEM.md and returns its
// trimmed contents, or "" when the file is missing / unreadable /
// empty. Errors are intentionally swallowed: the file is optional,
// and any failure to read it should fall back to the built-in
// default system prompt rather than crash the run.
func readUserSystemPrompt(zutHome string) string {
	if zutHome == "" {
		return ""
	}
	path := filepath.Join(zutHome, "SYSTEM.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// loadAgentsContext loads optional AGENTS.md instruction files. No
// default file is created or required: zut only includes files that
// already exist. Global instructions ($ZUT_HOME/AGENTS.md) come first,
// followed by project instructions from the filesystem root down to cwd.
func loadAgentsContext(cwd, zutHome string) []ContextFile {
	var files []ContextFile
	seen := map[string]bool{}
	add := func(path string) {
		if path == "" {
			return
		}
		abs, err := filepath.Abs(path)
		if err == nil {
			path = abs
		}
		if seen[path] {
			return
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		content := strings.TrimSpace(string(raw))
		if content == "" {
			return
		}
		seen[path] = true
		files = append(files, ContextFile{Path: path, Content: content})
	}
	addFirstFromDir := func(dir string) {
		if dir == "" {
			return
		}
		for _, name := range []string{"AGENTS.md", "AGENTS.MD"} {
			path := filepath.Join(dir, name)
			if _, err := os.Stat(path); err == nil {
				add(path)
				return
			}
		}
	}

	addFirstFromDir(zutHome)

	if cwd != "" {
		abs, err := filepath.Abs(cwd)
		if err == nil {
			cwd = abs
		}
		var dirs []string
		for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
			dirs = append(dirs, dir)
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
		}
		for i := len(dirs) - 1; i >= 0; i-- {
			addFirstFromDir(dirs[i])
		}
	}

	return files
}

func formatAgentsContext(files []ContextFile) string {
	if len(files) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Project context instructions loaded from AGENTS.md files. Follow them when working in this repository. Later files are more specific and may override earlier ones.\n")
	for _, f := range files {
		fmt.Fprintf(&sb, "\n## %s\n\n%s\n", f.Path, f.Content)
	}
	return strings.TrimSpace(sb.String())
}

// descMapFromSummaries indexes the human-readable descriptions for
// the renderToolsSection rebuild path.
func descMapFromSummaries(summaries []ToolSummary) map[string]string {
	out := make(map[string]string, len(summaries))
	for _, s := range summaries {
		out[s.Name] = s.Description
	}
	return out
}

// NewClient returns a provider.Client for r, choosing the auth mode
// based on r.AuthMethod. Panics if no credential is present; callers
// must check HasCredential() first.
func (r Resolved) NewClient() provider.Client {
	if !r.HasCredential() {
		panic("NewClient called without credential; check HasCredential first")
	}
	wrap := r.withHTTPClient
	switch r.Provider {
	case "ollama":
		return wrap(provider.NewOpenAICompat("ollama", r.Credential, r.BaseURL, ""))
	case provider.LlamaCPPProviderID:
		credential := r.Credential
		if credential == "local" {
			credential = ""
		}
		return wrap(provider.NewOpenAICompat(provider.LlamaCPPProviderID, credential, r.BaseURL, ""))
	case "kimi":
		// kimi-coding speaks anthropic-messages on api.kimi.com/coding.
		// Subscription OAuth (refreshed) wraps the same Anthropic-shaped client.
		inner := wrap(provider.NewKimiCodingWithHeaders(r.Credential, r.BaseURL, kimiCodeHeaders()))
		if r.AuthMethod == "oauth" {
			return r.wrapWithRefresh(inner)
		}
		return inner
	case "moonshotai":
		return wrap(provider.NewMoonshot(r.Credential, r.BaseURL))
	case "moonshotai-cn":
		return wrap(provider.NewMoonshotCN(r.Credential, r.BaseURL))
	case "deepseek":
		return wrap(provider.NewDeepSeek(r.Credential, r.BaseURL))
	case "openai":
		return provider.NewModelRouter("openai",
			wrap(provider.NewOpenAI(r.Credential, r.BaseURL)),
			map[string]provider.Client{
				provider.APIResponses: wrap(provider.NewOpenAIResponsesNamed(r.Credential, r.BaseURL, "openai")),
			})
	case "openai-codex":
		inner := wrap(provider.NewOpenAICodex(r.Credential, r.AccountID, r.BaseURL))
		return r.wrapWithRefresh(inner)
	case "openai-responses":
		// Public OpenAI Responses API (api.openai.com/v1/responses) via
		// API key. Separate provider from `openai` (Chat Completions) and
		// from `openai-codex` (ChatGPT subscription OAuth).
		return wrap(provider.NewOpenAIResponses(r.Credential, r.BaseURL))
	case "google":
		return wrap(provider.NewGemini(r.Credential, r.BaseURL))
	case "cerebras":
		return wrap(provider.NewCerebras(r.Credential, r.BaseURL))
	case "groq":
		return wrap(provider.NewGroq(r.Credential, r.BaseURL))
	case "xai":
		inner := r.newXAIClient(r.Credential)
		if r.AuthMethod == "oauth" {
			return r.wrapWithRefresh(inner)
		}
		return inner
	case "together":
		return wrap(provider.NewTogether(r.Credential, r.BaseURL))
	case "huggingface":
		return wrap(provider.NewHuggingFace(r.Credential, r.BaseURL))
	case "openrouter":
		return wrap(provider.NewOpenRouter(r.Credential, r.BaseURL))
	case "zai":
		return wrap(provider.NewZAI(r.Credential, r.BaseURL))
	case "xiaomi":
		return wrap(provider.NewXiaomi(r.Credential, r.BaseURL))
	case "xiaomi-token-plan-ams":
		return wrap(provider.NewXiaomiTokenPlan("ams", r.Credential, r.BaseURL))
	case "xiaomi-token-plan-cn":
		return wrap(provider.NewXiaomiTokenPlan("cn", r.Credential, r.BaseURL))
	case "xiaomi-token-plan-sgp":
		return wrap(provider.NewXiaomiTokenPlan("sgp", r.Credential, r.BaseURL))
	case "opencode":
		return wrap(provider.NewOpenCode(r.Credential, r.BaseURL))
	case "opencode-go":
		return wrap(provider.NewOpenCodeGo(r.Credential, r.BaseURL))
	case "minimax":
		return wrap(provider.NewMinimaxAnthropic(r.Credential, r.BaseURL))
	case "minimax-cn":
		return wrap(provider.NewMinimaxCNAnthropic(r.Credential, r.BaseURL))
	case "fireworks":
		return wrap(provider.NewFireworksAnthropic(r.Credential, r.BaseURL))
	case "vercel-ai-gateway":
		return wrap(provider.NewVercelGatewayAnthropic(r.Credential, r.BaseURL))
	case "mistral":
		return wrap(provider.NewMistral(r.Credential, r.BaseURL))
	case "amazon-bedrock":
		return wrap(provider.NewBedrock(r.Credential, r.BaseURL))
	case "google-vertex":
		return wrap(provider.NewGoogleVertex(r.Credential, r.BaseURL))
	case "azure-openai-responses":
		return wrap(provider.NewAzureOpenAIResponses(r.Credential, r.BaseURL))
	case "github-copilot":
		return wrap(provider.NewGithubCopilot(r.Credential, r.BaseURL))
	case "cloudflare-workers-ai":
		return wrap(provider.NewCloudflareWorkersAI(r.Credential, r.BaseURL))
	case "cloudflare-ai-gateway":
		return wrap(provider.NewCloudflareAIGateway(r.Credential, r.BaseURL))
	default:
		// Custom providers: choose wire format from the models.json api field.
		if cfg, ok := provider.CustomProviders()[r.Provider]; ok {
			switch cfg.API {
			case provider.APIResponses:
				return wrap(provider.NewOpenAIResponsesNamed(r.Credential, r.BaseURL, r.Provider))
			case "anthropic":
				return wrap(provider.NewAnthropicCompat(r.Provider, r.Credential, r.BaseURL))
			default: // "openai"
				return wrap(provider.NewOpenAICompat(r.Provider, r.Credential, r.BaseURL, ""))
			}
		}
		if r.AuthMethod == "oauth" {
			inner := wrap(provider.NewAnthropicOAuth(r.Credential, r.BaseURL))
			return r.wrapWithRefresh(inner)
		}
		return wrap(provider.NewAnthropic(r.Credential, r.BaseURL))
	}
}

func (r Resolved) withHTTPClient(c provider.Client) provider.Client {
	if !r.InsecureTLS {
		return c
	}
	return provider.WithHTTPClient(c, provider.NewHTTPClient(true))
}

func (r Resolved) newXAIClient(token string) provider.Client {
	return provider.NewModelRouter("xai",
		r.withHTTPClient(provider.NewXAI(token, r.BaseURL)),
		map[string]provider.Client{
			provider.APIResponses: r.withHTTPClient(provider.NewOpenAIResponsesNamed(token, r.BaseURL, "xai")),
		})
}

// wrapWithRefresh wraps an OAuth client so the access token is
// refreshed automatically before each API call. Without this, long
// sessions (hours) silently fail when the 1-hour token expires.
func (r Resolved) wrapWithRefresh(inner provider.Client) provider.Client {
	provName := r.Provider
	tokenProvider := provName
	if provName == "openai-codex" {
		tokenProvider = "openai"
	}
	baseURL := r.BaseURL
	accountID := r.AccountID

	refreshFn := func(ctx context.Context) (string, error) {
		tok, err := refreshIfExpired(tokenProvider, loadOAuthToken(tokenProvider))
		if err != nil {
			return "", err
		}
		return tok.AccessToken, nil
	}

	factory := func(token string) provider.Client {
		switch provName {
		case "openai-codex":
			return r.withHTTPClient(provider.NewOpenAICodex(token, accountID, baseURL))
		case "kimi":
			// anthropic-messages on api.kimi.com/coding.
			return r.withHTTPClient(provider.NewKimiCodingWithHeaders(token, baseURL, kimiCodeHeaders()))
		case "xai":
			return r.newXAIClient(token)
		default:
			return r.withHTTPClient(provider.NewAnthropicOAuth(token, baseURL))
		}
	}

	return provider.NewRefreshingClient(inner, refreshFn, factory)
}

// UseSandbox replaces the sandbox pointer that every tool in r's
// registry references. Used to keep the /jail state stable across
// agent rebuilds (e.g. /login, /model switching providers).
func (r *Resolved) UseSandbox(s *tools.Sandbox) {
	if s == nil || r == nil {
		return
	}
	if r.Sandbox != nil && r.Sandbox.Permissions != nil {
		s.Permissions = r.Sandbox.Permissions
	}
	r.Sandbox = s
	for name, t := range r.ToolRegistry {
		switch v := t.(type) {
		case *tools.ReadTool:
			v.Sandbox = s
		case *tools.WriteTool:
			v.Sandbox = s
		case *tools.EditTool:
			v.Sandbox = s
		case *tools.BashTool:
			v.Sandbox = s
		case *tools.CreateWorktreeTool:
			v.Sandbox = s
		case *tools.LSPTool:
			v.Sandbox = s
		}
		_ = name
	}
}

// NewAgent constructs a core.Agent from r. Requires a credential.
func (r Resolved) NewAgent() *core.Agent {
	a := core.NewAgent(r.NewClient(), r.Model, r.SystemPrompt, r.ToolRegistry)
	a.MaxSteps = r.MaxSteps
	a.ContextWindow = r.ContextWindow
	a.MaxTokens = r.MaxOutput
	a.Reasoning = r.Reasoning
	a.Temperature = r.Temperature
	a.FastMode = r.FastMode
	return a
}

func buildToolRegistry(args Args, cwd string, sandbox *tools.Sandbox, lspEnabled, diagnosticsOnWrite, diagnosticsOnEdit bool) core.Registry {
	if args.NoTools {
		return core.Registry{}
	}
	var manager *lsp.Manager
	if lspEnabled && lspManagerNeeded(args, diagnosticsOnWrite, diagnosticsOnEdit) {
		options := lsp.ManagerOptions{}
		if sandbox != nil {
			options.CheckWritePath = sandbox.CheckWritePath
		}
		manager = lsp.NewManagerWithOptions(options)
	}
	all := map[string]core.Tool{
		"read":            &tools.ReadTool{CWD: cwd, Sandbox: sandbox},
		"write":           &tools.WriteTool{CWD: cwd, Sandbox: sandbox, LSP: manager, LSPDiagnostics: diagnosticsOnWrite},
		"edit":            &tools.EditTool{CWD: cwd, Sandbox: sandbox, LSP: manager, LSPDiagnostics: diagnosticsOnEdit},
		"bash":            &tools.BashTool{CWD: cwd, Sandbox: sandbox},
		"create_worktree": &tools.CreateWorktreeTool{CWD: cwd, Sandbox: sandbox},
	}
	if webSearchAllowedForRegistry(args) {
		all["web_search"] = tools.NewWebSearchTool()
	}
	if manager != nil {
		lspTool := tools.NewLSPTool(cwd, manager)
		lspTool.Sandbox = sandbox
		all["lsp"] = lspTool
	}
	reg := core.Registry{}
	if len(args.Tools) == 0 {
		for _, t := range all {
			reg[t.Name()] = t
		}
		return reg
	}
	for _, name := range args.Tools {
		if t, ok := all[name]; ok {
			reg[name] = t
		}
	}
	return reg
}

func lspManagerNeeded(args Args, diagnosticsOnWrite, diagnosticsOnEdit bool) bool {
	if len(args.Tools) == 0 {
		return true
	}
	for _, name := range args.Tools {
		switch name {
		case "lsp":
			return true
		case "write":
			if diagnosticsOnWrite {
				return true
			}
		case "edit":
			if diagnosticsOnEdit {
				return true
			}
		}
	}
	return false
}

func autoSubagentsToolAllowed(args Args) bool {
	return autoSubagentsToolAllowedFor(args, "subagent_spawn")
}

func autoSubagentsStatusToolAllowed(args Args) bool {
	return autoSubagentsToolAllowedFor(args, "subagent_status")
}

func autoSubagentsStopToolAllowed(args Args) bool {
	return autoSubagentsToolAllowedFor(args, "subagent_stop")
}

func autoSubagentsResumeToolAllowed(args Args) bool {
	return autoSubagentsToolAllowedFor(args, "subagent_resume")
}

func autoSubagentsAnyToolAllowed(args Args) bool {
	return autoSubagentsToolAllowed(args) ||
		autoSubagentsStatusToolAllowed(args) ||
		autoSubagentsStopToolAllowed(args) ||
		autoSubagentsResumeToolAllowed(args)
}

func autoSubagentsToolAllowedFor(args Args, toolName string) bool {
	if args.NoTools || args.PermissionSet != nil {
		return false
	}
	if args.ToolsSet && len(args.Tools) == 0 {
		return false
	}
	if len(args.Tools) == 0 {
		return true
	}
	for _, name := range args.Tools {
		if name == toolName {
			return true
		}
	}
	return false
}

var nativeToolSummaryOrder = []string{"read", "write", "edit", "bash", "create_worktree", "lsp", "web_search"}

func toolSummaries(reg core.Registry, args Args) []ToolSummary {
	var out []ToolSummary
	for _, name := range nativeToolSummaryOrder {
		if t, ok := reg[name]; ok {
			out = append(out, ToolSummary{Name: t.Name(), Description: t.Description()})
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func kimiCodeHeaders() map[string]string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	deviceID := ""
	if home, err := os.UserHomeDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(home, ".kimi", "device_id")); err == nil {
			deviceID = strings.TrimSpace(string(b))
		}
	}
	if deviceID == "" {
		deviceID = "zut"
	}
	return map[string]string{
		"User-Agent":         "KimiCLI/1.41.0",
		"X-Msh-Platform":     "kimi_cli",
		"X-Msh-Version":      "1.41.0",
		"X-Msh-Device-Name":  host,
		"X-Msh-Device-Model": runtime.GOOS + "-" + runtime.GOARCH,
		"X-Msh-Os-Version":   runtime.GOOS,
		"X-Msh-Device-Id":    deviceID,
	}
}

func envVarName(provider string) string {
	switch provider {
	case "openai", "openai-codex", "openai-responses":
		return "OPENAI"
	case "kimi":
		return "KIMI"
	case "deepseek":
		return "DEEPSEEK"
	case "google":
		return "GEMINI"
	case "ollama":
		return "OLLAMA"
	case "llama.cpp":
		return "LLAMA"
	case "moonshotai", "moonshotai-cn":
		return "MOONSHOT"
	case "groq":
		return "GROQ"
	case "xai":
		return "XAI"
	case "cerebras":
		return "CEREBRAS"
	case "together":
		return "TOGETHER"
	case "huggingface":
		return "HF"
	case "openrouter":
		return "OPENROUTER"
	case "mistral":
		return "MISTRAL"
	case "zai":
		return "ZAI"
	case "xiaomi":
		return "XIAOMI"
	case "xiaomi-token-plan-ams":
		return "XIAOMI_TOKEN_PLAN_AMS"
	case "xiaomi-token-plan-cn":
		return "XIAOMI_TOKEN_PLAN_CN"
	case "xiaomi-token-plan-sgp":
		return "XIAOMI_TOKEN_PLAN_SGP"
	case "minimax":
		return "MINIMAX"
	case "minimax-cn":
		return "MINIMAX_CN"
	case "fireworks":
		return "FIREWORKS"
	case "vercel-ai-gateway":
		return "AI_GATEWAY"
	case "opencode", "opencode-go":
		return "OPENCODE"
	case "github-copilot":
		return "COPILOT_GITHUB_TOKEN"
	case "cloudflare-workers-ai", "cloudflare-ai-gateway":
		return "CLOUDFLARE"
	case "amazon-bedrock":
		return "AWS"
	case "google-vertex":
		return "GOOGLE_CLOUD"
	case "azure-openai-responses":
		return "AZURE_OPENAI"
	default:
		return "ANTHROPIC"
	}
}
