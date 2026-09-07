package provider

import (
	"fmt"
	"maps"
	"sync"
)

// Deliberate working budget, not the models' advertised maximum context.
const openAIContextWindowTarget = 500_000

// ModelPriceTier describes the pricing that applies once the prompt reaches
// InputTokens. Tiers are retained in model metadata so cost calculation can
// select the highest applicable threshold.
type ModelPriceTier struct {
	InputTokens     int
	PriceInput      float64
	PriceOutput     float64
	PriceCacheRead  float64
	PriceCacheWrite float64
}

// Model describes a single LLM we know about.
type Model struct {
	Provider          string // "anthropic" | "openai"
	ID                string // API id
	DisplayName       string
	API               string // wire API override for providers that support multiple protocols
	ContextWindow     int
	MaxOutput         int
	Reasoning         bool              // supports reasoning
	ReasoningLevelMap map[string]string // optional level overrides; empty values remove a level
	// ReasoningEffortMap translates normalized zut reasoning levels to the
	// provider's exact effort values when a model-specific catalog supplies
	// them (for example, minimum -> minimal).
	ReasoningEffortMap map[string]string

	// AdaptiveThinking marks Anthropic models that only support the
	// adaptive thinking mode (Opus 4.7+). These reject explicit
	// thinking budgets (thinking:{type:"enabled",budget_tokens:N} -> 400)
	// and also reject non-default sampling params (temperature/top_p/
	// top_k). The Anthropic client sends thinking:{type:"adaptive"} plus
	// output_config.effort and omits temperature for these models.
	AdaptiveThinking bool

	// AdaptiveThinkingCompat marks Anthropic-compatible models that require
	// thinking:{type:"adaptive"} but still accept sampling parameters and do
	// not support Anthropic's output_config.effort extension.
	AdaptiveThinkingCompat bool

	// Prices are USD per 1M tokens. Price*Above fields apply to all token
	// classes when the total prompt size reaches PriceTierInputTokens.
	PriceInput      float64
	PriceOutput     float64
	PriceCacheRead  float64
	PriceCacheWrite float64
	// PriceTiers contains all context-pricing thresholds, ordered by
	// InputTokens. The legacy Price*Above fields retain the first tier for
	// compatibility with callers that only understand one threshold.
	PriceTiers           []ModelPriceTier
	PriceTierInputTokens int
	PriceInputAbove      float64
	PriceOutputAbove     float64
	PriceCacheReadAbove  float64
	PriceCacheWriteAbove float64

	// Speculative marks models whose ids are known from the upstream
	// vendor's CLI but not yet live on their public API. They'll 404
	// today but start working the moment the provider flips the switch.
	Speculative bool

	// BaseURL overrides the provider's default API endpoint for this
	// model. Optional; when empty the provider's default (or the
	// --base-url flag) is used. Useful for local models served by
	// ollama, vLLM, LM Studio, etc.
	BaseURL string

	// Source is where this model entry came from: "catalog" (baked in),
	// "live" (discovered via /v1/models), or "cache" (loaded from the
	// on-disk cache). Informational.
	Source string
}

// Catalog is the hardcoded, read-only list of supported models.
// Prices are USD per 1M tokens. The list is curated to what zut's
// clients (Anthropic Messages, OpenAI Chat Completions, and OpenAI Responses)
// can actually talk to; models that are only reachable through an unsupported
// provider-specific protocol are omitted. OpenCode Go is populated
// separately by runtime discovery from its provider API and models.dev.
var Catalog = []Model{
	// ---- Anthropic / Claude 4.x ----
	{
		Provider: "anthropic", ID: "claude-sonnet-4-5", DisplayName: "Claude Sonnet 4.5 (latest)",
		ContextWindow: 200000, MaxOutput: 64000, Reasoning: true,
		PriceInput: 3, PriceOutput: 15, PriceCacheRead: 0.3, PriceCacheWrite: 3.75,
	},
	{
		Provider: "anthropic", ID: "claude-opus-4-1", DisplayName: "Claude Opus 4.1 (latest)",
		ContextWindow: 200000, MaxOutput: 32000, Reasoning: true,
		PriceInput: 15, PriceOutput: 75, PriceCacheRead: 1.5, PriceCacheWrite: 18.75,
	},
	{
		Provider: "anthropic", ID: "claude-opus-4-0", DisplayName: "Claude Opus 4 (latest)",
		ContextWindow: 200000, MaxOutput: 32000, Reasoning: true,
		PriceInput: 15, PriceOutput: 75, PriceCacheRead: 1.5, PriceCacheWrite: 18.75,
	},
	{
		Provider: "anthropic", ID: "claude-sonnet-4-0", DisplayName: "Claude Sonnet 4 (latest)",
		ContextWindow: 200000, MaxOutput: 64000, Reasoning: true,
		PriceInput: 3, PriceOutput: 15, PriceCacheRead: 0.3, PriceCacheWrite: 3.75,
	},
	{
		Provider: "anthropic", ID: "claude-haiku-4-5", DisplayName: "Claude Haiku 4.5 (latest)",
		ContextWindow: 200000, MaxOutput: 64000, Reasoning: true,
		PriceInput: 1, PriceOutput: 5, PriceCacheRead: 0.1, PriceCacheWrite: 1.25,
	},

	// ---- Anthropic / Claude 3.x (legacy) ----
	{
		Provider: "anthropic", ID: "claude-3-7-sonnet-20250219", DisplayName: "Claude Sonnet 3.7",
		ContextWindow: 200000, MaxOutput: 64000, Reasoning: true,
		PriceInput: 3, PriceOutput: 15, PriceCacheRead: 0.3, PriceCacheWrite: 3.75,
	},
	{
		Provider: "anthropic", ID: "claude-3-5-sonnet-20241022", DisplayName: "Claude Sonnet 3.5 v2",
		ContextWindow: 200000, MaxOutput: 8192, Reasoning: false,
		PriceInput: 3, PriceOutput: 15, PriceCacheRead: 0.3, PriceCacheWrite: 3.75,
	},
	{
		Provider: "anthropic", ID: "claude-3-5-haiku-latest", DisplayName: "Claude Haiku 3.5 (latest)",
		ContextWindow: 200000, MaxOutput: 8192, Reasoning: false,
		PriceInput: 0.8, PriceOutput: 4, PriceCacheRead: 0.08, PriceCacheWrite: 1,
	},
	{
		Provider: "anthropic", ID: "claude-3-opus-20240229", DisplayName: "Claude Opus 3",
		ContextWindow: 200000, MaxOutput: 4096, Reasoning: false,
		PriceInput: 15, PriceOutput: 75, PriceCacheRead: 1.5, PriceCacheWrite: 18.75,
	},

	// ---- DeepSeek ----
	// The current public DeepSeek API exposes the V4 family on
	// api.deepseek.com/v1. Pro is the flagship reasoning model;
	// Flash is the cheaper/faster sibling. Both accept image inputs
	// (multimodal parts: image_url) in addition to text.
	{
		Provider: "deepseek", ID: "deepseek-v4-pro", DisplayName: "DeepSeek V4 Pro",
		ContextWindow: 1000000, MaxOutput: 384000, Reasoning: true,
		PriceInput: 0.435, PriceOutput: 0.87, PriceCacheRead: 0.003625,
		BaseURL: "https://api.deepseek.com",
	},
	{
		Provider: "deepseek", ID: "deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash",
		ContextWindow: 1000000, MaxOutput: 384000, Reasoning: true,
		PriceInput: 0.14, PriceOutput: 0.28, PriceCacheRead: 0.0028,
		BaseURL: "https://api.deepseek.com",
	},

	// ---- Kimi / Kimi Code ----
	// Anthropic-messages on https://api.kimi.com/coding (no /v1 suffix;
	// the Anthropic client appends /v1/messages itself).
	{
		Provider: "kimi", ID: "kimi-for-coding", DisplayName: "Kimi For Coding",
		ContextWindow: 262144, MaxOutput: 32768, Reasoning: true,
		PriceInput: 0, PriceOutput: 0, PriceCacheRead: 0,
		BaseURL: "https://api.kimi.com/coding",
	},

	// ---- OpenAI / GPT-5 family ----
	{
		Provider: "openai", ID: "gpt-5", DisplayName: "GPT-5",
		ContextWindow: 400000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 1.25, PriceOutput: 10, PriceCacheRead: 0.125,
	},
	{
		Provider: "openai", ID: "gpt-5-mini", DisplayName: "GPT-5 Mini",
		ContextWindow: 400000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 0.25, PriceOutput: 2, PriceCacheRead: 0.025,
	},
	{
		Provider: "openai", ID: "gpt-5-nano", DisplayName: "GPT-5 Nano",
		ContextWindow: 400000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 0.05, PriceOutput: 0.4, PriceCacheRead: 0.005,
	},

	// ---- OpenAI / GPT-4.1 family ----
	{
		Provider: "openai", ID: "gpt-4.1", DisplayName: "GPT-4.1",
		ContextWindow: 1047576, MaxOutput: 32768, Reasoning: false,
		PriceInput: 2, PriceOutput: 8, PriceCacheRead: 0.5,
	},
	{
		Provider: "openai", ID: "gpt-4.1-mini", DisplayName: "GPT-4.1 mini",
		ContextWindow: 1047576, MaxOutput: 32768, Reasoning: false,
		PriceInput: 0.4, PriceOutput: 1.6, PriceCacheRead: 0.1,
	},
	{
		Provider: "openai", ID: "gpt-4.1-nano", DisplayName: "GPT-4.1 nano",
		ContextWindow: 1047576, MaxOutput: 32768, Reasoning: false,
		PriceInput: 0.1, PriceOutput: 0.4, PriceCacheRead: 0.03,
	},

	// ---- OpenAI / GPT-4o family ----
	{
		Provider: "openai", ID: "gpt-4o", DisplayName: "GPT-4o",
		ContextWindow: 128000, MaxOutput: 16384, Reasoning: false,
		PriceInput: 2.5, PriceOutput: 10, PriceCacheRead: 1.25,
	},
	{
		Provider: "openai", ID: "gpt-4o-mini", DisplayName: "GPT-4o mini",
		ContextWindow: 128000, MaxOutput: 16384, Reasoning: false,
		PriceInput: 0.15, PriceOutput: 0.6, PriceCacheRead: 0.08,
	},

	// ---- OpenAI / reasoning models ----
	{
		Provider: "openai", ID: "o4-mini", DisplayName: "o4-mini",
		ContextWindow: 200000, MaxOutput: 100000, Reasoning: true,
		PriceInput: 1.1, PriceOutput: 4.4, PriceCacheRead: 0.28,
	},
	{
		Provider: "openai", ID: "o3", DisplayName: "o3",
		ContextWindow: 200000, MaxOutput: 100000, Reasoning: true,
		PriceInput: 2, PriceOutput: 8, PriceCacheRead: 0.5,
	},
	{
		Provider: "openai", ID: "o3-mini", DisplayName: "o3-mini",
		ContextWindow: 200000, MaxOutput: 100000, Reasoning: true,
		PriceInput: 1.1, PriceOutput: 4.4, PriceCacheRead: 0.55,
	},
	{
		Provider: "openai", ID: "o1", DisplayName: "o1",
		ContextWindow: 200000, MaxOutput: 100000, Reasoning: true,
		PriceInput: 15, PriceOutput: 60, PriceCacheRead: 7.5,
	},

	// ---- Google / Gemini ----
	{
		Provider: "google", ID: "gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro",
		ContextWindow: 1048576, MaxOutput: 65536, Reasoning: true,
		PriceInput: 1.25, PriceOutput: 10, PriceCacheRead: 0.125,
	},
	{
		Provider: "google", ID: "gemini-2.5-flash", DisplayName: "Gemini 2.5 Flash",
		ContextWindow: 1048576, MaxOutput: 65536, Reasoning: true,
		PriceInput: 0.3, PriceOutput: 2.5, PriceCacheRead: 0.03,
	},
	{
		Provider: "google", ID: "gemini-2.5-flash-lite", DisplayName: "Gemini 2.5 Flash-Lite",
		ContextWindow: 1048576, MaxOutput: 65536, Reasoning: true,
		PriceInput: 0.1, PriceOutput: 0.4, PriceCacheRead: 0.01,
	},
	{
		Provider: "google", ID: "gemini-2.0-flash", DisplayName: "Gemini 2.0 Flash",
		ContextWindow: 1048576, MaxOutput: 8192, Reasoning: false,
		PriceInput: 0.1, PriceOutput: 0.4, PriceCacheRead: 0.025,
	},
	{
		Provider: "google", ID: "gemini-2.0-flash-lite", DisplayName: "Gemini 2.0 Flash-Lite",
		ContextWindow: 1048576, MaxOutput: 8192, Reasoning: false,
		PriceInput: 0.075, PriceOutput: 0.3, PriceCacheRead: 0,
	},

	// ---- OpenRouter ----
	// Seed entry only: the default model returned by defaultModelForProvider
	// so it resolves offline. The full OpenRouter catalog is discovered live
	// (DiscoverOpenRouter) and overlays this on refresh.
	{
		Provider: "openrouter", ID: "anthropic/claude-sonnet-4.5", DisplayName: "Claude Sonnet 4.5 (OpenRouter)",
		ContextWindow: 1000000, MaxOutput: 64000, Reasoning: true,
		PriceInput: 3, PriceOutput: 15, PriceCacheRead: 0.3, PriceCacheWrite: 3.75,
		BaseURL: openrouterDefaultBaseURL,
	},
	{
		Provider: "openrouter", ID: "anthropic/claude-sonnet-5", DisplayName: "Claude Sonnet 5 (OpenRouter)",
		ContextWindow: 1000000, MaxOutput: 128000, Reasoning: true, AdaptiveThinking: true,
		PriceInput: 2, PriceOutput: 10, PriceCacheRead: 0.2, PriceCacheWrite: 2.5,
		BaseURL: openrouterDefaultBaseURL,
	},

	// ---- Speculative: Anthropic ----
	{
		Provider: "anthropic", ID: "claude-opus-4-5", DisplayName: "Claude Opus 4.5 (latest)",
		ContextWindow: 200000, MaxOutput: 64000, Reasoning: true,
		PriceInput: 5, PriceOutput: 25, PriceCacheRead: 0.5, PriceCacheWrite: 6.25,
		Speculative: true,
	},
	{
		Provider: "anthropic", ID: "claude-opus-4-6", DisplayName: "Claude Opus 4.6",
		ContextWindow: 1000000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 5, PriceOutput: 25, PriceCacheRead: 0.5, PriceCacheWrite: 6.25,
		Speculative: true,
	},
	{
		Provider: "anthropic", ID: "claude-opus-4-7", DisplayName: "Claude Opus 4.7",
		ContextWindow: 1000000, MaxOutput: 128000, Reasoning: true, AdaptiveThinking: true,
		PriceInput: 5, PriceOutput: 25, PriceCacheRead: 0.5, PriceCacheWrite: 6.25,
		Speculative: true,
	},
	{
		Provider: "anthropic", ID: "claude-opus-4-8", DisplayName: "Claude Opus 4.8",
		ContextWindow: 1000000, MaxOutput: 128000, Reasoning: true, AdaptiveThinking: true,
		PriceInput: 5, PriceOutput: 25, PriceCacheRead: 0.5, PriceCacheWrite: 6.25,
		Speculative: true,
	},
	{
		Provider: "anthropic", ID: "claude-opus-5", DisplayName: "Claude Opus 5",
		ContextWindow: 1000000, MaxOutput: 128000, Reasoning: true, AdaptiveThinking: true,
		PriceInput: 5, PriceOutput: 25, PriceCacheRead: 0.5, PriceCacheWrite: 6.25,
	},
	{
		Provider: "anthropic", ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6",
		ContextWindow: 1000000, MaxOutput: 64000, Reasoning: true,
		PriceInput: 3, PriceOutput: 15, PriceCacheRead: 0.3, PriceCacheWrite: 3.75,
		Speculative: true,
	},
	{
		Provider: "anthropic", ID: "claude-sonnet-5", DisplayName: "Claude Sonnet 5",
		ContextWindow: 1000000, MaxOutput: 128000, Reasoning: true, AdaptiveThinking: true,
		PriceInput: 2, PriceOutput: 10, PriceCacheRead: 0.2, PriceCacheWrite: 2.5,
	},
	{
		Provider: "anthropic", ID: "claude-fable-5", DisplayName: "Claude Fable 5",
		ContextWindow: 1000000, MaxOutput: 128000, Reasoning: true, AdaptiveThinking: true,
		PriceInput: 10, PriceOutput: 50, PriceCacheRead: 0.5, PriceCacheWrite: 6.25,
		Speculative: true,
	},

	// ---- Speculative: OpenAI ----
	// Public OpenAI API route. The ChatGPT/Codex subscription route is
	// represented separately below as provider "openai-codex".
	{
		Provider: "openai", ID: "gpt-5.1", DisplayName: "GPT-5.1",
		ContextWindow: 400000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 1.25, PriceOutput: 10, PriceCacheRead: 0.13,
		Speculative: true,
	},
	{
		Provider: "openai", ID: "gpt-5.2", DisplayName: "GPT-5.2",
		ContextWindow: 400000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 1.75, PriceOutput: 14, PriceCacheRead: 0.175,
		Speculative: true,
	},
	{
		Provider: "openai", ID: "gpt-5.4", DisplayName: "GPT-5.4",
		ContextWindow: 272000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 2.5, PriceOutput: 15, PriceCacheRead: 0.25,
		Speculative: true,
	},
	{
		Provider: "openai", ID: "gpt-5.4-mini", DisplayName: "GPT-5.4 mini",
		ContextWindow: 400000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 0.75, PriceOutput: 4.5, PriceCacheRead: 0.075,
		Speculative: true,
	},
	{
		Provider: "openai", ID: "gpt-5.5", DisplayName: "GPT-5.5",
		ContextWindow: 272000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 5, PriceOutput: 30, PriceCacheRead: 0.5,
		Speculative: true,
	},
	{
		Provider: "openai", ID: "gpt-5.6-luna", DisplayName: "GPT-5.6 Luna", API: APIResponses,
		ContextWindow: openAIContextWindowTarget, MaxOutput: 128000, Reasoning: true,
		PriceInput: 1, PriceOutput: 6, PriceCacheRead: 0.1, PriceCacheWrite: 1.25,
		Speculative: true,
	},
	{
		Provider: "openai", ID: "gpt-5.6-sol", DisplayName: "GPT-5.6 Sol", API: APIResponses,
		ContextWindow: openAIContextWindowTarget, MaxOutput: 128000, Reasoning: true,
		PriceInput: 5, PriceOutput: 30, PriceCacheRead: 0.5, PriceCacheWrite: 6.25,
		Speculative: true,
	},
	{
		Provider: "openai", ID: "gpt-5.6-terra", DisplayName: "GPT-5.6 Terra", API: APIResponses,
		ContextWindow: openAIContextWindowTarget, MaxOutput: 128000, Reasoning: true,
		PriceInput: 2.5, PriceOutput: 15, PriceCacheRead: 0.25, PriceCacheWrite: 3.125,
		Speculative: true,
	},
	{
		Provider: "openai", ID: "gpt-6-astra", DisplayName: "GPT-6 Astra", API: APIResponses,
		ContextWindow: openAIContextWindowTarget, MaxOutput: 128000, Reasoning: true,
		PriceInput: 10, PriceOutput: 50, PriceCacheRead: 1, PriceCacheWrite: 12.5,
		PriceTierInputTokens: 272000,
		PriceInputAbove:      20, PriceOutputAbove: 75, PriceCacheReadAbove: 2, PriceCacheWriteAbove: 25,
	},
	// ---- OpenAI Codex / ChatGPT subscription backend ----
	// Same model ids as the OpenAI family, but routed through the
	// ChatGPT Codex OAuth backend rather than api.openai.com.
	{
		Provider: "openai-codex", ID: "gpt-5.3-codex-spark", DisplayName: "GPT-5.3 Codex Spark",
		ContextWindow: 272000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 1.75, PriceOutput: 14, PriceCacheRead: 0.175,
		Speculative: true,
	},
	{
		Provider: "openai-codex", ID: "gpt-5.4", DisplayName: "GPT-5.4",
		ContextWindow: 272000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 2.5, PriceOutput: 15, PriceCacheRead: 0.25,
		Speculative: true,
	},
	{
		Provider: "openai-codex", ID: "gpt-5.4-mini", DisplayName: "GPT-5.4 mini",
		ContextWindow: 272000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 0.75, PriceOutput: 4.5, PriceCacheRead: 0.075,
		Speculative: true,
	},
	{
		Provider: "openai-codex", ID: "gpt-5.5", DisplayName: "GPT-5.5",
		ContextWindow: 272000, MaxOutput: 128000, Reasoning: true,
		PriceInput: 5, PriceOutput: 30, PriceCacheRead: 0.5,
		Speculative: true,
	},
	{
		Provider: "openai-codex", ID: "gpt-5.6-luna", DisplayName: "GPT-5.6 Luna",
		ContextWindow: openAIContextWindowTarget, MaxOutput: 128000, Reasoning: true,
		PriceInput: 1, PriceOutput: 6, PriceCacheRead: 0.1, PriceCacheWrite: 1.25,
	},
	{
		Provider: "openai-codex", ID: "gpt-5.6-sol", DisplayName: "GPT-5.6 Sol",
		ContextWindow: openAIContextWindowTarget, MaxOutput: 128000, Reasoning: true,
		PriceInput: 5, PriceOutput: 30, PriceCacheRead: 0.5, PriceCacheWrite: 6.25,
	},
	{
		Provider: "openai-codex", ID: "gpt-5.6-terra", DisplayName: "GPT-5.6 Terra",
		ContextWindow: openAIContextWindowTarget, MaxOutput: 128000, Reasoning: true,
		PriceInput: 2.5, PriceOutput: 15, PriceCacheRead: 0.25, PriceCacheWrite: 3.125,
	},
	{
		Provider: "openai-codex", ID: "gpt-6-astra", DisplayName: "GPT-6 Astra",
		ContextWindow: openAIContextWindowTarget, MaxOutput: 128000, Reasoning: true,
		PriceInput: 10, PriceOutput: 50, PriceCacheRead: 1, PriceCacheWrite: 12.5,
		PriceTierInputTokens: 272000,
		PriceInputAbove:      20, PriceOutputAbove: 75, PriceCacheReadAbove: 2, PriceCacheWriteAbove: 25,
	},
}

// DefaultModel is used when the user does not specify one.
var DefaultModel = Catalog[0] // claude-sonnet-4-5

// ----- active (merged) catalog -----
//
// Callers should use Active() / FindModel / ModelsForProvider for
// lookups. They return the baked-in Catalog merged with any live
// models loaded via SetLiveModels.

var (
	activeMu                 sync.RWMutex
	active                   []Model // live overlay merged in via SetLiveModels; nil = none yet
	activeSet                bool    // true once SetLiveModels has run (even with empty live)
	authoritativeProviderSet map[string]struct{}
	managedModels            []Model // ephemeral models exposed by local model managers
	userModels               []Model // highest-precedence models loaded from models.json
)

// CatalogSnapshot captures the mutable catalog overlays so a caller can
// restore provider state after temporarily installing test or runtime data.
// Use SnapshotCatalog and RestoreCatalog instead of replacing the live
// overlay with nil, which can discard another caller's active catalog.
type CatalogSnapshot struct {
	active                   []Model
	activeSet                bool
	authoritativeProviderSet map[string]struct{}
	managedModels            []Model
	userModels               []Model
}

// SnapshotCatalog returns a deep copy of the mutable provider catalog state.
func SnapshotCatalog() CatalogSnapshot {
	activeMu.RLock()
	defer activeMu.RUnlock()

	snapshot := CatalogSnapshot{
		activeSet:                activeSet,
		authoritativeProviderSet: maps.Clone(authoritativeProviderSet),
	}
	if active != nil {
		snapshot.active = cloneModels(active)
	}
	if managedModels != nil {
		snapshot.managedModels = cloneModels(managedModels)
	}
	if userModels != nil {
		snapshot.userModels = cloneModels(userModels)
	}
	return snapshot
}

// RestoreCatalog restores a snapshot returned by SnapshotCatalog.
func RestoreCatalog(snapshot CatalogSnapshot) {
	activeMu.Lock()
	defer activeMu.Unlock()

	if snapshot.active != nil {
		active = cloneModels(snapshot.active)
	} else {
		active = nil
	}
	activeSet = snapshot.activeSet
	authoritativeProviderSet = maps.Clone(snapshot.authoritativeProviderSet)
	if snapshot.managedModels != nil {
		managedModels = cloneModels(snapshot.managedModels)
	} else {
		managedModels = nil
	}
	if snapshot.userModels != nil {
		userModels = cloneModels(snapshot.userModels)
	} else {
		userModels = nil
	}
}

func cloneModel(model Model) Model {
	model.ReasoningLevelMap = maps.Clone(model.ReasoningLevelMap)
	model.ReasoningEffortMap = maps.Clone(model.ReasoningEffortMap)
	if model.PriceTiers != nil {
		model.PriceTiers = append([]ModelPriceTier(nil), model.PriceTiers...)
	}
	return model
}

func cloneModels(models []Model) []Model {
	out := make([]Model, len(models))
	for i, model := range models {
		out[i] = cloneModel(model)
	}
	return out
}

func mergeUserModels(active, users []Model) []Model {
	if len(users) == 0 {
		return active
	}
	byKey := func(provider, id string) string { return provider + "\x00" + id }
	index := make(map[string]int, len(active))
	for i, model := range active {
		index[byKey(model.Provider, model.ID)] = i
	}
	for _, user := range users {
		user = cloneModel(user)
		key := byKey(user.Provider, user.ID)
		if idx, ok := index[key]; ok {
			existing := active[idx]
			// Preserve catalog values when an override leaves an optional
			// field at its zero value, matching models.json semantics.
			// A positive price override replaces tiered pricing too;
			// otherwise ComputeCost would retain discovered tier rates.
			if user.PriceInput > 0 || user.PriceOutput > 0 || user.PriceCacheRead > 0 || user.PriceCacheWrite > 0 {
				existing.PriceTiers = nil
				existing.PriceTierInputTokens = 0
				existing.PriceInputAbove, existing.PriceOutputAbove = 0, 0
				existing.PriceCacheReadAbove, existing.PriceCacheWriteAbove = 0, 0
			}
			if user.PriceInput > 0 {
				existing.PriceInput = user.PriceInput
			}
			if user.PriceOutput > 0 {
				existing.PriceOutput = user.PriceOutput
			}
			if user.PriceCacheRead > 0 {
				existing.PriceCacheRead = user.PriceCacheRead
			}
			if user.PriceCacheWrite > 0 {
				existing.PriceCacheWrite = user.PriceCacheWrite
			}
			if user.DisplayName != "" {
				existing.DisplayName = user.DisplayName
			}
			if user.ContextWindow > 0 {
				existing.ContextWindow = user.ContextWindow
			}
			if user.MaxOutput > 0 {
				existing.MaxOutput = user.MaxOutput
			}
			existing.Reasoning = user.Reasoning
			if user.ReasoningLevelMap != nil {
				existing.ReasoningLevelMap = user.ReasoningLevelMap
			}
			if user.ReasoningEffortMap != nil {
				existing.ReasoningEffortMap = user.ReasoningEffortMap
			}
			if user.API != "" {
				existing.API = user.API
			}
			if user.BaseURL != "" {
				existing.BaseURL = user.BaseURL
			}
			existing.Source = "user"
			existing.Speculative = false
			active[idx] = existing
			continue
		}
		index[key] = len(active)
		active = append(active, user)
	}
	return active
}

// SetLiveModels replaces the "live" overlay used by the active catalog.
// Typically called after a successful /v1/models discovery or on load
// from the on-disk cache.
func SetLiveModels(live []Model) {
	SetLiveModelsForProviders(live, nil)
}

// SetLiveModelsForProviders replaces the live overlay, treating the listed
// providers' live catalogs as authoritative. For those providers, static
// entries omitted by discovery are removed from the active catalog.
func SetLiveModelsForProviders(live []Model, authoritativeProviders []string) {
	activeMu.Lock()
	defer activeMu.Unlock()
	activeSet = true
	authoritativeProviderSet = make(map[string]struct{}, len(authoritativeProviders))
	for _, name := range authoritativeProviders {
		authoritativeProviderSet[name] = struct{}{}
	}
	if len(live) == 0 && len(authoritativeProviders) == 0 {
		active = nil
		return
	}
	active = MergeCatalogForProviders(live, authoritativeProviders)
}

// ClearLiveModelsForProvider removes a provider's live overlay without
// disturbing other providers. User models remain separate and continue to
// be applied by Active.
func ClearLiveModelsForProvider(name string) {
	activeMu.Lock()
	defer activeMu.Unlock()
	delete(authoritativeProviderSet, name)
	if !activeSet || len(active) == 0 {
		return
	}
	out := active[:0]
	for _, model := range active {
		if model.Provider != name {
			out = append(out, model)
		}
	}
	active = out
}

// Active returns the current merged catalog.
//
// When no live overlay has been set it returns the fully-assembled
// static Catalog. Reading Catalog here (rather than capturing it into
// a package-level var initializer) is load-bearing: the extended
// catalog in catalog_builtin.go / extra_models.go is appended from
// init() functions, which run AFTER package-level var initializers.
// Snapshotting Catalog at var-init time would freeze the picker to the
// curated seed list and drop every extra provider (openrouter, groq,
// xai, ...). Deferring the read to call time avoids that ordering trap.
func Active() []Model {
	activeMu.RLock()
	defer activeMu.RUnlock()
	src := active
	if !activeSet || src == nil {
		src = Catalog
	}
	out := cloneModels(src)
	if len(managedModels) > 0 {
		index := make(map[string]int, len(out))
		for i, model := range out {
			index[model.Provider+"\x00"+model.ID] = i
		}
		for _, model := range managedModels {
			model = cloneModel(model)
			key := model.Provider + "\x00" + model.ID
			if i, ok := index[key]; ok {
				out[i] = model
				continue
			}
			index[key] = len(out)
			out = append(out, model)
		}
	}
	// User models are a durable, highest-precedence overlay. Apply them
	// after the live and managed overlays so a catalog refresh cannot
	// discard overrides or user-only entries.
	return mergeUserModels(out, userModels)
}

// SetManagedModels replaces the ephemeral catalog entries supplied by local
// model managers. Unlike SetLiveModels, this does not disturb provider model
// discovery or its on-disk cache.
func SetManagedModels(models []Model) {
	activeMu.Lock()
	defer activeMu.Unlock()
	managedModels = append([]Model(nil), models...)
}

// FindModel returns a Model by id, optionally constrained by provider.
// If provider is empty, the first matching id is returned. Looks up
// against the merged active catalog.
func FindModel(provider, id string) (Model, error) {
	for _, m := range Active() {
		if m.ID == id && (provider == "" || m.Provider == provider) {
			return m, nil
		}
	}
	return Model{}, fmt.Errorf("unknown model %q (provider=%q)", id, provider)
}

// IsProviderCatalogAuthoritative reports whether a successful live catalog
// has declared the provider's available model IDs authoritative.
func IsProviderCatalogAuthoritative(provider string) bool {
	activeMu.RLock()
	defer activeMu.RUnlock()
	_, ok := authoritativeProviderSet[provider]
	return ok
}

// AcceptsUnlistedModels reports whether a provider may be queried with an
// ID that is not in the local catalog. OpenCode Go permits this only before
// a successful authoritative live catalog has been loaded.
func AcceptsUnlistedModels(provider string) bool {
	return provider == ProviderOpenCodeGo && !IsProviderCatalogAuthoritative(provider)
}

// ModelsForProvider returns all models for the given provider, from the
// merged active catalog.
func ModelsForProvider(provider string) []Model {
	var out []Model
	for _, m := range Active() {
		if m.Provider == provider {
			out = append(out, m)
		}
	}
	return out
}

// ComputeCost returns the USD cost for the given usage on model m.
func ComputeCost(m Model, u Usage) float64 {
	inputPrice := m.PriceInput
	outputPrice := m.PriceOutput
	cacheReadPrice := m.PriceCacheRead
	cacheWritePrice := m.PriceCacheWrite
	promptTokens := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
	if len(m.PriceTiers) > 0 {
		selected := -1
		for i, tier := range m.PriceTiers {
			if tier.InputTokens <= promptTokens && (selected < 0 || tier.InputTokens > m.PriceTiers[selected].InputTokens) {
				selected = i
			}
		}
		if selected >= 0 {
			tier := m.PriceTiers[selected]
			inputPrice = tier.PriceInput
			outputPrice = tier.PriceOutput
			cacheReadPrice = tier.PriceCacheRead
			cacheWritePrice = tier.PriceCacheWrite
		}
	} else if m.PriceTierInputTokens > 0 && promptTokens >= m.PriceTierInputTokens {
		// Keep the legacy single-tier fields inclusive at the threshold.
		inputPrice = m.PriceInputAbove
		outputPrice = m.PriceOutputAbove
		cacheReadPrice = m.PriceCacheReadAbove
		cacheWritePrice = m.PriceCacheWriteAbove
	}

	const per = 1_000_000.0
	return float64(u.InputTokens)*inputPrice/per +
		float64(u.OutputTokens)*outputPrice/per +
		float64(u.CacheReadTokens)*cacheReadPrice/per +
		float64(u.CacheWriteTokens)*cacheWritePrice/per
}
