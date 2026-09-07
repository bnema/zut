package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/provider"
)

var (
	discoverOpenCodeGoFn = provider.DiscoverOpenCodeGo
	modelCatalogMu       sync.Mutex
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
	scopes := currentModelProviderScopes()
	modelCatalogMu.Lock()
	defer modelCatalogMu.Unlock()
	loadCachedModels(scopes)
}

func synchronousOpenCodeGoAPIKey(ctx context.Context, explicitProvider, explicitAPIKey string) string {
	providerName := effectiveCatalogProvider(explicitProvider)
	if explicitAPIKey != "" || (providerName != "" && providerName != provider.ProviderOpenCodeGo) {
		return explicitAPIKey
	}
	key, method, _, err := resolveCredentialFull(ctx, provider.ProviderOpenCodeGo, "", apiKeyCommandExecute)
	if err != nil || method != "apikey" {
		return ""
	}
	return key
}

// ResolveSDK prepares the credential-scoped catalog and resolves an SDK
// runtime against a private model snapshot. Preparation and snapshot capture
// are coordinated with background publication, while credential resolution
// itself remains outside the process-wide catalog lock.
func ResolveSDK(ctx context.Context, args Args) (Resolved, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Resolved{}, err
	}
	if args.APIKey == "" && effectiveCatalogProvider(args.Provider) == provider.ProviderOpenCodeGo {
		args.APIKey = synchronousOpenCodeGoAPIKey(ctx, args.Provider, args.APIKey)
		if err := ctx.Err(); err != nil {
			return Resolved{}, err
		}
	}
	modelCatalogMu.Lock()
	userModels := LoadUserModels()
	_, _ = prepareRuntimeCatalog(args.Provider, args.APIKey, args.BaseURL, userModels, args.Model)
	// Keep an explicit empty snapshot as well: if Resolve later falls back
	// to OpenCode Go because only that provider has credentials, it must not
	// reread a concurrently changing global overlay.
	args.modelCatalog = append([]provider.Model{}, provider.ModelsForProvider(provider.ProviderOpenCodeGo)...)
	args.modelCatalogAuthoritative = provider.IsProviderCatalogAuthoritative(provider.ProviderOpenCodeGo)
	modelCatalogMu.Unlock()
	if err := ctx.Err(); err != nil {
		return Resolved{}, err
	}

	resolved, err := Resolve(args, true)
	if err == nil && resolved.Provider == provider.ProviderOpenCodeGo {
		refreshModelsAsyncForProvider(ctx, resolved.Provider, resolved.Credential, resolved.BaseURL, provider.ProviderOpenCodeGo)
	}
	return resolved, err
}

// loadCachedModels requires modelCatalogMu in production callers.
func loadCachedModels(scopes map[string]string) {
	resetCatalogDiagnostic(scopes)
	// A process can construct SDK runtimes for different credentials in
	// sequence. Remove the previous OpenCode Go live overlay before applying
	// the newly scoped cache, including cache-miss and read-error paths.
	provider.ClearLiveModelsForProvider(provider.ProviderOpenCodeGo)
	c, err := provider.LoadCache(ModelCachePath())
	status := provider.ProviderCatalogStatus(provider.ProviderOpenCodeGo)
	for _, model := range c.Models {
		if model.Provider == provider.ProviderOpenCodeGo {
			status.PreviouslyDiscovered = true
			break
		}
	}
	provider.SetProviderCatalogStatus(provider.ProviderOpenCodeGo, status)
	if err != nil || c.Version != provider.ModelCacheVersion {
		return
	}
	c = filterCacheByProviderScopes(c, scopes)
	if len(c.Models) > 0 || len(c.AuthoritativeProviders) > 0 {
		provider.SetLiveModelsForProviders(c.Models, c.AuthoritativeProviders)
	}
	if c.ProviderScopes[provider.ProviderOpenCodeGo] != "" {
		status.State = provider.CatalogCached
		status.PreviouslyDiscovered = true
		provider.SetProviderCatalogStatus(provider.ProviderOpenCodeGo, status)
	}
}

// currentModelProviderScopes returns identifiers that make a provider's
// discovered catalog account-specific. The cache remains provider-neutral:
// each provider supplies an opaque scope value when it needs one.
func currentModelProviderScopes() map[string]string {
	return currentModelProviderScopesForBaseURL("")
}

func currentModelProviderScopesForBaseURL(openCodeGoBaseURL string) map[string]string {
	scopes := make(map[string]string)
	if token := loadOAuthToken("openai"); token != nil && token.AccountID != "" {
		scopes["openai-codex"] = token.AccountID
	}
	if key, method, _, err := resolveCredentialFull(context.Background(), provider.ProviderOpenCodeGo, "", apiKeyCommandSkip); err == nil && method == "apikey" && key != "" {
		scopes[provider.ProviderOpenCodeGo] = credentialScopeForEndpoint(key, openCodeGoBaseURL)
	}
	return scopes
}

// credentialScope lets a cache follow a credential without storing the
// credential itself. API keys are high-entropy values, and the cache is also
// protected with restrictive file permissions.
func credentialScope(credential string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(credential)))
}

func credentialScopeForEndpoint(credential, baseURL string) string {
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if endpoint == "" {
		endpoint = provider.OpenCodeGoDefaultBaseURL
	}
	return credentialScope(credential + "\x00" + endpoint)
}

func modelProviderScopes(explicitProvider, explicitAPIKey, explicitBaseURL string) map[string]string {
	openCodeGoBaseURL := ""
	if explicitProvider == provider.ProviderOpenCodeGo {
		openCodeGoBaseURL = explicitBaseURL
	}
	scopes := currentModelProviderScopesForBaseURL(openCodeGoBaseURL)
	if explicitProvider == provider.ProviderOpenCodeGo && explicitAPIKey != "" {
		scopes[provider.ProviderOpenCodeGo] = credentialScopeForEndpoint(explicitAPIKey, explicitBaseURL)
	}
	return scopes
}

// filterCacheByProviderScopes removes cached entries whose provider scope no
// longer matches the active credential. It also removes the authoritative
// marker so that a failed refresh falls back to the baked-in catalog instead
// of treating another account's empty catalog as authoritative.
func filterCacheByProviderScopes(c provider.ModelCache, scopes map[string]string) provider.ModelCache {
	return filterCacheByProviderScopesExcept(c, scopes, "")
}

func filterCacheByProviderScopesExcept(c provider.ModelCache, scopes map[string]string, keepProvider string) provider.ModelCache {
	out := provider.ModelCache{
		Version:                c.Version,
		FetchedAt:              c.FetchedAt,
		Models:                 append([]provider.Model(nil), c.Models...),
		AuthoritativeProviders: append([]string(nil), c.AuthoritativeProviders...),
		ProviderScopes:         make(map[string]string, len(c.ProviderScopes)),
	}
	for name, scope := range c.ProviderScopes {
		out.ProviderScopes[name] = scope
	}

	scopedProviders := map[string]struct{}{
		provider.ProviderOpenAICodex: {},
		provider.ProviderOpenCodeGo:  {},
	}
	for name := range c.ProviderScopes {
		scopedProviders[name] = struct{}{}
	}
	for name := range scopes {
		scopedProviders[name] = struct{}{}
	}
	for name := range scopedProviders {
		if name == keepProvider {
			continue
		}
		if cachedScope := c.ProviderScopes[name]; cachedScope != "" && cachedScope == scopes[name] {
			continue
		}
		out.Models = filterModelsByProvider(out.Models, name)
		out.AuthoritativeProviders = filterProviderNames(out.AuthoritativeProviders, name)
		delete(out.ProviderScopes, name)
	}
	return out
}

func filterModelsByProvider(models []provider.Model, name string) []provider.Model {
	out := make([]provider.Model, 0, len(models))
	for _, model := range models {
		if model.Provider != name {
			out = append(out, model)
		}
	}
	return out
}

func filterProviderNames(names []string, target string) []string {
	out := make([]string, 0, len(names))
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
func LoadUserModels() []provider.Model {
	models, warnings := provider.LoadUserModelsWithWarnings(UserModelsPath())
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "zut:", w)
	}
	provider.SetUserModels(models)
	return models
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
// OpenCode Go is also exempt from the unknown-model repair because its
// provider catalog is authoritative but live-only.
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

	modelID := strings.TrimSpace(cfg.Model)
	if cfg.Provider != "" && modelID != "" {
		if _, err := provider.FindModel(cfg.Provider, modelID); err != nil {
			// OpenCode Go is authoritative at runtime and intentionally has no
			// baked-in entries, so an uncached id must survive startup repair.
			if provider.AcceptsUnlistedModels(cfg.Provider) {
				// The live refresh will validate the id when it is available.
			} else if isGatewayProvider(cfg.Provider) && isGatewayRoutedModelID(modelID) {
				// Provider is a router and the id is route-qualified; preserve it.
			} else if m, err := provider.FindModel("", modelID); err == nil {
				fix := defaultModelForProvider(cfg.Provider)
				fmt.Fprintf(os.Stderr,
					"zut: config.json: model %q belongs to provider %q (config has provider=%q); switched model to %q\n",
					modelID, m.Provider, cfg.Provider, fix)
				cfg.Model = fix
				changed = true
			} else if cfg.Provider != "ollama" && cfg.Provider != provider.LlamaCPPProviderID && !provider.AcceptsUnlistedModels(cfg.Provider) {
				// Model id not in any catalog. Reset to provider's default.
				fix := defaultModelForProvider(cfg.Provider)
				fmt.Fprintf(os.Stderr,
					"zut: config.json: model %q not found in the active catalog; switched to %q\n",
					modelID, fix)
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
func RefreshModelsAsync(explicitProvider, explicitAPIKey string, explicitBaseURL ...string) {
	baseURL := ""
	if len(explicitBaseURL) > 0 {
		baseURL = explicitBaseURL[0]
	}
	go refreshModels(explicitProvider, explicitAPIKey, baseURL, "")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = refreshLlamaCPPModels(ctx, apiKeyCommandSkip)
	}()
}

func refreshModelsAsyncForProvider(ctx context.Context, explicitProvider, explicitAPIKey, explicitBaseURL, onlyProvider string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	go refreshModelsWithContext(ctx, explicitProvider, explicitAPIKey, explicitBaseURL, onlyProvider, apiKeyCommandSkip)
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

// needsOpenCodeGoRefresh handles a credential added after a fresh cache was
// written. Without this check, a cache created before the first OpenCode Go
// login would hide the provider until the 24-hour TTL expired.
func needsOpenCodeGoRefresh(c provider.ModelCache) bool {
	if !CredentialAvailable(provider.ProviderOpenCodeGo) {
		return false
	}
	for _, model := range c.Models {
		if model.Provider == provider.ProviderOpenCodeGo {
			return false
		}
	}
	for _, name := range c.AuthoritativeProviders {
		if name == provider.ProviderOpenCodeGo {
			return false
		}
	}
	return true
}

func cacheSnapshotsEqual(a provider.ModelCache, aErr error, b provider.ModelCache, bErr error) bool {
	if (aErr != nil) != (bErr != nil) {
		return false
	}
	if aErr != nil {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func eligibleProviderDiscoveryIncomplete(eligible map[string]struct{}, discovered map[string][]provider.Model) bool {
	for name := range eligible {
		if _, ok := discovered[name]; !ok {
			return true
		}
	}
	return false
}

func refreshModels(explicitProvider, explicitAPIKey, explicitBaseURL, onlyProvider string) {
	refreshModelsWithMode(explicitProvider, explicitAPIKey, explicitBaseURL, onlyProvider, apiKeyCommandSkip)
}

func refreshModelsWithMode(explicitProvider, explicitAPIKey, explicitBaseURL, onlyProvider string, commandMode apiKeyCommandMode) {
	refreshModelsWithContext(context.Background(), explicitProvider, explicitAPIKey, explicitBaseURL, onlyProvider, commandMode)
}

func resolveCredentialForCatalog(ctx context.Context, providerName string, mode apiKeyCommandMode) (cred, method string, err error) {
	cred, method, _, err = resolveCredentialFull(ctx, providerName, "", mode)
	return cred, method, err
}

func refreshModelsWithContext(parent context.Context, explicitProvider, explicitAPIKey, explicitBaseURL, onlyProvider string, commandMode apiKeyCommandMode) {
	if parent == nil {
		parent = context.Background()
	}
	modelCatalogMu.Lock()
	publicationRevision := catalogDiagnosticRevision
	modelCatalogMu.Unlock()
	explicitProvider = effectiveCatalogProvider(explicitProvider)
	cached, cachedErr := provider.LoadCache(ModelCachePath())
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()

	openCodeGoCred, openCodeGoMethod := "", ""
	var openCodeGoCredentialErr error
	if explicitProvider == provider.ProviderOpenCodeGo && explicitAPIKey != "" {
		openCodeGoCred, openCodeGoMethod = explicitAPIKey, "apikey"
	} else {
		openCodeGoCred, openCodeGoMethod, _, openCodeGoCredentialErr = resolveCredentialFull(ctx, provider.ProviderOpenCodeGo, "", commandMode)
	}
	if (onlyProvider == "" || onlyProvider == provider.ProviderOpenCodeGo) && openCodeGoCredentialErr != nil && !errors.Is(openCodeGoCredentialErr, errNoCredential) {
		publicationRevision = beginCatalogDiscovery("", publicationRevision)
		finishCatalogDiscovery(publicationRevision, provider.CatalogStatus{State: provider.CatalogCredentialError})
	}
	currentScopes := modelProviderScopes(explicitProvider, explicitAPIKey, explicitBaseURL)
	openCodeGoBaseURL := ""
	if explicitProvider == provider.ProviderOpenCodeGo {
		openCodeGoBaseURL = explicitBaseURL
	}
	if openCodeGoMethod == "apikey" && openCodeGoCred != "" {
		currentScopes[provider.ProviderOpenCodeGo] = credentialScopeForEndpoint(openCodeGoCred, openCodeGoBaseURL)
		if onlyProvider == "" || onlyProvider == provider.ProviderOpenCodeGo {
			publicationRevision = adoptResolvedCatalogScope(currentScopes, publicationRevision)
		}
	}
	if cached.IsFresh() &&
		cached.Version == provider.ModelCacheVersion &&
		providerScopesEqual(cached.ProviderScopes, currentScopes) &&
		((onlyProvider != "" && onlyProvider != provider.ProviderOpenCodeGo) || !needsOpenCodeGoRefresh(cached)) {
		return
	}

	var diagnosticRevision uint64
	diagnostic := provider.CatalogStatus{State: provider.CatalogUnavailable}
	if (onlyProvider == "" || onlyProvider == provider.ProviderOpenCodeGo) && openCodeGoMethod == "apikey" {
		diagnosticRevision = beginCatalogDiscovery(currentScopes[provider.ProviderOpenCodeGo], publicationRevision)
		publicationRevision = diagnosticRevision
		defer func() {
			if diagnostic.State != provider.CatalogReady && diagnostic.State != provider.CatalogFailed && ctx.Err() != nil {
				diagnostic = provider.CatalogStatus{State: provider.CatalogFailed, Failure: *provider.ClassifyDiscoveryError(ctx.Err())}
			}
			finishCatalogDiscovery(diagnosticRevision, diagnostic)
		}()
	}

	discovered := make(map[string][]provider.Model)
	authoritativeDiscovered := make(map[string]bool)
	discoveredScopes := make(map[string]string)
	eligibleProviders := make(map[string]struct{}, len(currentScopes))
	for name := range currentScopes {
		eligibleProviders[name] = struct{}{}
	}
	markEligible := func(name string) {
		eligibleProviders[name] = struct{}{}
	}
	recordDiscovery := func(name string, models []provider.Model, authoritative bool, scope string) {
		discovered[name] = append([]provider.Model(nil), models...)
		if authoritative {
			authoritativeDiscovered[name] = true
		}
		if scope != "" {
			discoveredScopes[name] = scope
		}
	}
	if onlyProvider == "" || onlyProvider == provider.ProviderAnthropic {
		if cred, method, err := resolveCredentialForCatalog(ctx, provider.ProviderAnthropic, commandMode); err == nil && method == "apikey" {
			markEligible(provider.ProviderAnthropic)
			// /v1/models on Anthropic is API-key only; OAuth tokens can
			// also list models via the bearer header, but we skip OAuth
			// here to avoid surprise rate-limit hits on subscription keys.
			if live, err := provider.DiscoverAnthropic(ctx, cred, ""); err == nil {
				recordDiscovery(provider.ProviderAnthropic, live, false, "")
			}
		}
	}
	if onlyProvider == "" || onlyProvider == provider.ProviderOpenAI {
		if cred, method, err := resolveCredentialForCatalog(ctx, provider.ProviderOpenAI, commandMode); err == nil && method == "apikey" {
			markEligible(provider.ProviderOpenAI)
			if live, err := provider.DiscoverOpenAI(ctx, cred, ""); err == nil {
				recordDiscovery(provider.ProviderOpenAI, live, false, "")
			}
		}
	}
	if onlyProvider == "" || onlyProvider == provider.ProviderOpenAICodex {
		if cred, method, accountID, err := resolveCredentialFull(ctx, provider.ProviderOpenAICodex, "", commandMode); err == nil && method == "oauth" {
			markEligible(provider.ProviderOpenAICodex)
			if live, err := provider.DiscoverOpenAICodex(ctx, cred, accountID, ""); err == nil {
				recordDiscovery(provider.ProviderOpenAICodex, live, accountID != "", accountID)
			}
		}
	}
	if onlyProvider == "" || onlyProvider == provider.ProviderOpenCodeGo {
		if openCodeGoMethod == "apikey" {
			markEligible(provider.ProviderOpenCodeGo)
			if live, err := discoverOpenCodeGoFn(ctx, openCodeGoCred, openCodeGoBaseURL); err == nil {
				recordDiscovery(provider.ProviderOpenCodeGo, live, true, credentialScopeForEndpoint(openCodeGoCred, openCodeGoBaseURL))
			} else {
				diagnostic = provider.CatalogStatus{State: provider.CatalogFailed, Failure: *provider.ClassifyDiscoveryError(err)}
			}
		}
	}
	if onlyProvider == "" || onlyProvider == provider.ProviderKimi {
		if cred, method, err := resolveCredentialForCatalog(ctx, provider.ProviderKimi, commandMode); err == nil && method == "apikey" {
			markEligible(provider.ProviderKimi)
			if live, err := provider.DiscoverOpenAI(ctx, cred, "https://api.kimi.com/coding/v1"); err == nil {
				for i := range live {
					live[i].Provider = provider.ProviderKimi
					live[i].Source = "live"
				}
				recordDiscovery(provider.ProviderKimi, live, false, "")
			}
		}
	}
	if onlyProvider == "" || onlyProvider == provider.ProviderGoogle {
		if cred, method, err := resolveCredentialForCatalog(ctx, provider.ProviderGoogle, commandMode); err == nil && method == "apikey" {
			markEligible(provider.ProviderGoogle)
			if live, err := provider.DiscoverGoogle(ctx, cred, ""); err == nil {
				recordDiscovery(provider.ProviderGoogle, live, false, "")
			}
		}
	}
	if onlyProvider == "" || onlyProvider == provider.ProviderOpenRouter {
		if _, _, err := resolveCredentialForCatalog(ctx, provider.ProviderOpenRouter, commandMode); err == nil {
			markEligible(provider.ProviderOpenRouter)
			// /models is public; gate on a credential so the picker only
			// fills with OpenRouter's hundreds of routes for users who use it.
			if live, err := provider.DiscoverOpenRouter(ctx, ""); err == nil {
				recordDiscovery(provider.ProviderOpenRouter, live, false, "")
			}
		}
	}

	if len(discovered) == 0 || ctx.Err() != nil {
		return
	}

	modelCatalogMu.Lock()
	defer modelCatalogMu.Unlock()
	if ctx.Err() != nil || publicationRevision != catalogDiagnosticRevision {
		// Reloading credentials can leave the cache file unchanged. Its
		// snapshot alone cannot authorize an older refresh to republish
		// models or rewrite the cache after login/logout or an SDK reload.
		return
	}
	latestSnapshot, latestErr := provider.LoadCache(ModelCachePath())
	latest := latestSnapshot
	if latest.Version != provider.ModelCacheVersion {
		// Routing metadata from an older cache format is not safe to merge
		// into a newly discovered catalog. Treat it as empty, but compare the
		// raw snapshot below so a concurrent refresh is still detected.
		latest = provider.ModelCache{}
	}
	preserveFullRefreshFreshness := onlyProvider == "" && eligibleProviderDiscoveryIncomplete(eligibleProviders, discovered)
	var all []provider.Model
	var authoritativeProviders []string
	var providerScopes map[string]string
	_, latestHasOpenCodeGo := latestSnapshot.ProviderScopes[provider.ProviderOpenCodeGo]
	keepOpenCodeGo := !cacheSnapshotsEqual(cached, cachedErr, latestSnapshot, latestErr) && latestHasOpenCodeGo
	if keepOpenCodeGo {
		// Another runtime replaced the cache while this discovery was in
		// flight and owns newer OpenCode Go scope data. Keep its scope and
		// still publish any unrelated providers that this refresh
		// discovered. When the newer snapshot carries no OpenCode Go scope,
		// this refresh's own discovery is the only such data and is
		// published below instead of being discarded.
		delete(discovered, provider.ProviderOpenCodeGo)
		delete(authoritativeDiscovered, provider.ProviderOpenCodeGo)
		delete(discoveredScopes, provider.ProviderOpenCodeGo)
	}
	if len(discovered) == 0 {
		return
	}
	if keepOpenCodeGo {
		latest = filterCacheByProviderScopesExcept(latest, currentScopes, provider.ProviderOpenCodeGo)
	} else {
		latest = filterCacheByProviderScopes(latest, currentScopes)
	}
	all = append([]provider.Model(nil), latest.Models...)
	authoritativeProviders = append([]string(nil), latest.AuthoritativeProviders...)
	providerScopes = make(map[string]string, len(latest.ProviderScopes))
	for name, scope := range latest.ProviderScopes {
		providerScopes[name] = scope
	}
	for name, live := range discovered {
		all = filterModelsByProvider(all, name)
		authoritativeProviders = filterProviderNames(authoritativeProviders, name)
		delete(providerScopes, name)
		all = append(all, live...)
		if authoritativeDiscovered[name] {
			authoritativeProviders = append(authoritativeProviders, name)
		}
		if scope := discoveredScopes[name]; scope != "" {
			providerScopes[name] = scope
		}
	}
	if len(all) == 0 && len(authoritativeProviders) == 0 {
		return
	}
	if ctx.Err() != nil {
		return
	}
	provider.SetLiveModelsForProviders(all, authoritativeProviders)
	if _, ok := discovered[provider.ProviderOpenCodeGo]; ok {
		diagnostic = provider.CatalogStatus{State: provider.CatalogReady}
	}
	fetchedAt := time.Now().UTC()
	if onlyProvider != "" {
		if latest.Version == provider.ModelCacheVersion {
			// A partial refresh must not make an otherwise stale full
			// catalog look fresh to the next CLI refresh.
			fetchedAt = latest.FetchedAt
		} else {
			fetchedAt = time.Time{}
		}
	} else if preserveFullRefreshFreshness {
		// A failed eligible discovery must not make stale metadata appear
		// fresh merely because another provider refreshed successfully.
		fetchedAt = latest.FetchedAt
	}
	if ctx.Err() != nil {
		return
	}
	_ = provider.SaveCache(ModelCachePath(), provider.ModelCache{
		Version:                provider.ModelCacheVersion,
		FetchedAt:              fetchedAt,
		Models:                 all,
		AuthoritativeProviders: authoritativeProviders,
		ProviderScopes:         providerScopes,
	})
}
