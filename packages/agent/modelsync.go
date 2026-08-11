package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnema/zut/packages/provider"
)

// ModelCachePath returns the on-disk location of the merged model cache.
func ModelCachePath() string {
	return filepath.Join(ZutHome(), "models-cache.json")
}

// UserModelsPath returns the path to the user's models.json override.
func UserModelsPath() string {
	return filepath.Join(ZutHome(), "models.json")
}

// LoadCachedModels loads the cache file and applies it to the provider
// package so FindModel / ModelsForProvider see live ids immediately.
// Safe to call before any credentials are known.
func LoadCachedModels() {
	c, err := provider.LoadCache(ModelCachePath())
	if err != nil {
		return
	}
	c = filterCacheByProviderScopes(c, currentModelProviderScopes())
	if len(c.Models) > 0 || len(c.AuthoritativeProviders) > 0 {
		provider.SetLiveModelsForProviders(c.Models, c.AuthoritativeProviders)
	}
}

// currentModelProviderScopes returns identifiers that make a provider's
// discovered catalog account-specific. The cache remains provider-neutral:
// each provider supplies an opaque scope value when it needs one.
func currentModelProviderScopes() map[string]string {
	scopes := make(map[string]string)
	if token := loadOAuthToken("openai"); token != nil && token.AccountID != "" {
		scopes["openai-codex"] = token.AccountID
	}
	return scopes
}

// filterCacheByProviderScopes removes cached entries whose provider scope no
// longer matches the active credential. It also removes the authoritative
// marker so that a failed refresh falls back to the baked-in catalog instead
// of treating another account's empty catalog as authoritative.
func filterCacheByProviderScopes(c provider.ModelCache, scopes map[string]string) provider.ModelCache {
	for name, cachedScope := range c.ProviderScopes {
		if cachedScope == scopes[name] && cachedScope != "" {
			continue
		}
		c.Models = filterModelsByProvider(c.Models, name)
		c.AuthoritativeProviders = filterProviderNames(c.AuthoritativeProviders, name)
		delete(c.ProviderScopes, name)
	}
	return c
}

func filterModelsByProvider(models []provider.Model, name string) []provider.Model {
	out := models[:0]
	for _, model := range models {
		if model.Provider != name {
			out = append(out, model)
		}
	}
	return out
}

func filterProviderNames(names []string, target string) []string {
	out := names[:0]
	for _, name := range names {
		if name != target {
			out = append(out, name)
		}
	}
	return out
}

func providerScopesEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, scope := range a {
		if b[name] != scope {
			return false
		}
	}
	return true
}

// LoadUserModels reads $ZUT_HOME/models.json and merges any user-defined
// models into the active catalog. User models take highest precedence.
// Any validation issues (bad provider id, empty model id, malformed
// JSON, negative widths) are surfaced as one warning per line on stderr;
// the well-formed entries from the rest of the file are still loaded.
func LoadUserModels() {
	models, warnings := provider.LoadUserModelsWithWarnings(UserModelsPath())
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "zut:", w)
	}
	provider.SetUserModels(models)
}

// isGatewayProvider returns true for providers whose OpenAI-compatible
// endpoint can accept routed model IDs that are not present in zut's local
// catalog. Vercel AI Gateway is intentionally not listed here: zut currently
// talks to it through the Anthropic-compatible client, which still requires
// catalog metadata for request shaping.
func isGatewayProvider(prov string) bool {
	switch prov {
	case "openrouter", "cloudflare-ai-gateway":
		return true
	default:
		return false
	}
}

// isGatewayRoutedModelID reports whether a model looks like the routed IDs
// used by gateway providers, for example "deepseek/deepseek-v4-flash".
// Non-routed typos like "deepseek-v4-flashh" should still be repaired to a
// known default instead of being silently accepted.
func isGatewayRoutedModelID(model string) bool {
	return strings.Contains(strings.TrimSpace(model), "/")
}

// ValidateAndRepairConfig checks the persisted config.json's
// (Provider, Model) pair against the active catalog and repairs any
// mismatch in-place (and on disk) before any UI renders. Three failure
// modes are handled:
//
//   - cfg.Provider is empty or unknown -> reset to "anthropic".
//   - cfg.Model is empty -> set to the provider's default.
//   - cfg.Model belongs to a different provider than cfg.Provider
//     (e.g. provider=anthropic + model=kimi-for-coding from a stale
//     half-applied switch) -> reset model to the provider's default.
//
// Gateway providers are exempt from the cross-provider model check for routed
// model IDs because those IDs can be valid even when absent from zut's catalog.
//
// Silent on success; one stderr line per repair. Errors loading or
// saving the file are non-fatal — the caller continues with defaults.
func ValidateAndRepairConfig() {
	cfg, err := LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "zut: config.json: %v (using defaults)\n", err)
		return
	}
	changed := false

	if cfg.Provider != "" && !isKnownProvider(cfg.Provider) {
		fmt.Fprintf(os.Stderr, "zut: config.json: unknown provider %q reset to \"anthropic\"\n", cfg.Provider)
		cfg.Provider = "anthropic"
		cfg.Model = ""
		changed = true
	}

	if cfg.Provider != "" && cfg.Model != "" {
		if _, err := provider.FindModel(cfg.Provider, cfg.Model); err != nil {
			// Gateway providers can serve routed model ids like
			// "deepseek/deepseek-v4-flash" even when the local catalog does not
			// know them. Preserve only routed ids; plain typos are still repaired.
			if isGatewayProvider(cfg.Provider) && isGatewayRoutedModelID(cfg.Model) {
				// Provider is a router and the id is route-qualified; preserve it.
			} else if m, err := provider.FindModel("", cfg.Model); err == nil {
				fix := defaultModelForProvider(cfg.Provider)
				fmt.Fprintf(os.Stderr,
					"zut: config.json: model %q belongs to provider %q (config has provider=%q); switched model to %q\n",
					cfg.Model, m.Provider, cfg.Provider, fix)
				cfg.Model = fix
				changed = true
			} else if cfg.Provider != "ollama" && cfg.Provider != provider.LlamaCPPProviderID {
				// Model id not in any catalog. Reset to provider's default.
				fix := defaultModelForProvider(cfg.Provider)
				fmt.Fprintf(os.Stderr,
					"zut: config.json: model %q not found in the active catalog; switched to %q\n",
					cfg.Model, fix)
				cfg.Model = fix
				changed = true
			}
		}
	}

	if changed {
		if err := SaveConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "zut: config.json: failed to persist repair: %v\n", err)
		}
	}
}

// RefreshModelsAsync kicks a background discovery for every provider
// we have credentials for. Refreshed results are merged into the
// active catalog and persisted to the on-disk cache.
//
// Silent on error: discovery is a nice-to-have. Callers can still use
// the baked-in catalog if this fails.
func RefreshModelsAsync() {
	go refreshModels()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = refreshLlamaCPPModels(ctx, apiKeyCommandSkip)
	}()
}

// RefreshLlamaCPPModels adds the router's currently loaded models to the
// active catalog. Unloaded models remain in the management UI and cannot be
// selected for inference until they are loaded.
func RefreshLlamaCPPModels(ctx context.Context) error {
	return refreshLlamaCPPModels(ctx, apiKeyCommandExecute)
}

func refreshLlamaCPPModels(ctx context.Context, commandMode apiKeyCommandMode) error {
	baseURL, apiKey, err := resolveLlamaCPPConfig(ctx, commandMode)
	if err != nil || baseURL == "" {
		return err
	}
	client, err := provider.NewLlamaCPPClient(baseURL, apiKey)
	if err != nil {
		return err
	}
	models, err := client.List(ctx, false)
	if err != nil {
		return err
	}
	provider.SetManagedModels(provider.LlamaCPPModels(models, client.ServerURL))
	return nil
}

func refreshModels() {
	cached, _ := provider.LoadCache(ModelCachePath())
	currentScopes := currentModelProviderScopes()
	if cached.IsFresh() && providerScopesEqual(cached.ProviderScopes, currentScopes) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var all []provider.Model
	var authoritativeProviders []string
	if cached.IsFresh() {
		cached = filterCacheByProviderScopes(cached, currentScopes)
		all = append(all, cached.Models...)
		authoritativeProviders = append(authoritativeProviders, cached.AuthoritativeProviders...)
	}
	providerScopes := make(map[string]string)

	if cred, method, err := resolveCredentialForBackground(ctx, "anthropic"); err == nil && method == "apikey" {
		// /v1/models on Anthropic is API-key only; OAuth tokens can
		// also list models via the bearer header, but we skip OAuth
		// here to avoid surprise rate-limit hits on subscription keys.
		if live, err := provider.DiscoverAnthropic(ctx, cred, ""); err == nil {
			all = append(all, live...)
		}
	}
	if cred, method, err := resolveCredentialForBackground(ctx, "openai"); err == nil && method == "apikey" {
		if live, err := provider.DiscoverOpenAI(ctx, cred, ""); err == nil {
			all = append(all, live...)
		}
	}
	if cred, method, accountID, err := resolveCredentialFull(ctx, "openai-codex", "", apiKeyCommandSkip); err == nil && method == "oauth" {
		if live, err := provider.DiscoverOpenAICodex(ctx, cred, accountID, ""); err == nil {
			all = append(all, live...)
			authoritativeProviders = append(authoritativeProviders, "openai-codex")
			if accountID != "" {
				providerScopes["openai-codex"] = accountID
			}
		}
	}
	if cred, method, err := resolveCredentialForBackground(ctx, "kimi"); err == nil && method == "apikey" {
		if live, err := provider.DiscoverOpenAI(ctx, cred, "https://api.kimi.com/coding/v1"); err == nil {
			for i := range live {
				live[i].Provider = "kimi"
				live[i].Source = "live"
			}
			all = append(all, live...)
		}
	}
	if cred, method, err := resolveCredentialForBackground(ctx, "google"); err == nil && method == "apikey" {
		if live, err := provider.DiscoverGoogle(ctx, cred, ""); err == nil {
			all = append(all, live...)
		}
	}
	if _, _, err := resolveCredentialForBackground(ctx, "openrouter"); err == nil {
		// /models is public; gate on a credential so the picker only
		// fills with OpenRouter's hundreds of routes for users who use it.
		if live, err := provider.DiscoverOpenRouter(ctx, ""); err == nil {
			all = append(all, live...)
		}
	}

	if len(all) == 0 && len(authoritativeProviders) == 0 {
		return
	}
	provider.SetLiveModelsForProviders(all, authoritativeProviders)
	_ = provider.SaveCache(ModelCachePath(), provider.ModelCache{
		FetchedAt:              time.Now().UTC(),
		Models:                 all,
		AuthoritativeProviders: authoritativeProviders,
		ProviderScopes:         providerScopes,
	})
}
