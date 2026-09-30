package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bnema/zut/packages/provider"
)

// discoverableCustomProviders returns the models.json providers that opted
// into live discovery, sorted for deterministic refresh order.
func discoverableCustomProviders() []string {
	var out []string
	for name := range provider.CustomProviders() {
		if isDiscoverableCustomProvider(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// isDiscoverableCustomProvider reports whether name is a custom provider
// with live discovery enabled. Its catalog entries are transient, so config
// repair must not treat a missing model as stale.
func isDiscoverableCustomProvider(name string) bool {
	cfg, ok := provider.CustomProviders()[name]
	return ok && cfg.Discover && strings.TrimSpace(cfg.BaseURL) != "" && !isBuiltinProvider(name)
}

// CustomDiscoveryConfigured reports whether any custom provider has live
// discovery enabled. It reads only loaded configuration, never the network.
func CustomDiscoveryConfigured() bool {
	return len(discoverableCustomProviders()) > 0
}

// customProviderKey resolves the optional bearer key for a custom provider.
// A missing key is not an error because local servers commonly accept
// unauthenticated requests. skip is true when the only key is command-backed
// and mode forbids running the command. A provider configured with
// auth "none" never consults the environment, auth.json, or key commands.
func customProviderKey(ctx context.Context, name string, mode apiKeyCommandMode) (key string, skip bool, err error) {
	if cfg, ok := provider.CustomProviders()[name]; ok && cfg.NoAuth && !isBuiltinProvider(name) {
		return "", false, nil
	}
	if v := os.Getenv(normalizeCustomProviderEnvVar(name) + "_API_KEY"); v != "" {
		return v, false, nil
	}
	creds, err := AuthStoreFor().Load()
	if err != nil {
		return "", false, err
	}
	stored, ok := creds.AdditionalAPIKeyCreds[name]
	if !ok {
		return "", false, nil
	}
	if stored.APIKey == "" && stored.APIKeyCommand != nil && mode == apiKeyCommandSkip {
		return "", true, nil
	}
	key, _, err = resolveStoredAPIKey(ctx, name, stored, mode)
	return key, false, err
}

// customDiscoveryHTTPClient keeps normal TLS verification for discovery,
// independent of the inference-only insecure setting, and honors proxy envs.
func customDiscoveryHTTPClient() *http.Client {
	return provider.NewHTTPClient(false)
}

// RefreshCustomProviderModels lists models from every discovery-enabled
// custom provider and merges them into the transient managed catalog.
// A failing provider keeps its previous snapshot; errors are joined after
// all providers were attempted. It may run a stored api-key command, so
// only call it for an explicit user action or model launch.
func RefreshCustomProviderModels(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return refreshCustomProviderModels(ctx, apiKeyCommandExecute)
}

// refreshCustomProvidersBackground is the startup variant: it never runs an
// api-key command and skips providers whose only key is command-backed.
func refreshCustomProvidersBackground() {
	if !CustomDiscoveryConfigured() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = refreshCustomProviderModels(ctx, apiKeyCommandSkip)
}

func refreshCustomProviderModels(ctx context.Context, mode apiKeyCommandMode) error {
	var errs []error
	var client *http.Client
	for _, name := range discoverableCustomProviders() {
		if client == nil {
			client = customDiscoveryHTTPClient()
		}
		if err := refreshCustomProviderModelWithClient(ctx, client, name, mode); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// refreshCustomProviderModel refreshes exactly one discovery-enabled provider.
// Other providers' endpoints and key commands are never touched. A provider
// that did not opt in is a no-op.
func refreshCustomProviderModel(ctx context.Context, name string, mode apiKeyCommandMode) error {
	if !isDiscoverableCustomProvider(name) {
		return nil
	}
	return refreshCustomProviderModelWithClient(ctx, customDiscoveryHTTPClient(), name, mode)
}

func refreshCustomProviderModelWithClient(ctx context.Context, client *http.Client, name string, mode apiKeyCommandMode) error {
	cfg := provider.CustomProviders()[name]
	key, skip, err := customProviderKey(ctx, name, mode)
	if err != nil || skip {
		return err
	}
	models, err := provider.DiscoverCustomProviderWithClient(ctx, client, name, cfg.BaseURL, key)
	if err != nil {
		return err // keep the previous snapshot
	}
	provider.SetManagedModelsForProvider(name, models)
	return nil
}
