package agent

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/provider"
)

func isolateCatalogDiagnostics(t *testing.T) {
	t.Helper()
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")
	snapshot := provider.SnapshotCatalog()
	modelCatalogMu.Lock()
	path, scope, revision := catalogDiagnosticPath, catalogDiagnosticScope, catalogDiagnosticRevision
	modelCatalogMu.Unlock()
	discover := discoverOpenCodeGoFn
	t.Cleanup(func() {
		discoverOpenCodeGoFn = discover
		modelCatalogMu.Lock()
		catalogDiagnosticPath, catalogDiagnosticScope, catalogDiagnosticRevision = path, scope, revision
		provider.RestoreCatalog(snapshot)
		modelCatalogMu.Unlock()
	})
}

func saveDiagnosticCache(t *testing.T, key string, version int) {
	t.Helper()
	if err := provider.SaveCache(ModelCachePath(), provider.ModelCache{
		Version:                version,
		FetchedAt:              time.Now().Add(-2 * provider.CacheTTL),
		Models:                 []provider.Model{{Provider: provider.ProviderOpenCodeGo, ID: "saved-test-model"}},
		AuthoritativeProviders: []string{provider.ProviderOpenCodeGo},
		ProviderScopes:         map[string]string{provider.ProviderOpenCodeGo: credentialScopeForEndpoint(key, "")},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogDiagnosticMissingKeyExplainsFilteredCache(t *testing.T) {
	isolateCatalogDiagnostics(t)
	saveDiagnosticCache(t, "synthetic-key", provider.ModelCacheVersion)
	discoverOpenCodeGoFn = func(context.Context, string, string) ([]provider.Model, error) {
		t.Fatal("discovery attempted without a credential")
		return nil, nil
	}
	LoadCachedModels()
	refreshModels("", "", "", provider.ProviderOpenCodeGo)
	_, err := provider.FindModel(provider.ProviderOpenCodeGo, "saved-test-model")
	var lookup *provider.ModelLookupError
	if !errors.As(err, &lookup) || lookup.Catalog.State != provider.CatalogMissingCredentials {
		t.Fatalf("lookup = %v", err)
	}
	if !strings.Contains(err.Error(), "no API key available in this process") {
		t.Fatalf("wrong action: %v", err)
	}
	if warnings := provider.CatalogWarnings(); len(warnings) != 1 || !strings.Contains(warnings[0], "opencode-go") {
		t.Fatalf("cached provider vanished: %v", warnings)
	}
	cache, err := provider.LoadCache(ModelCachePath())
	if err != nil || len(cache.Models) != 1 {
		t.Fatalf("diagnosis destroyed cache: %v", err)
	}
}

func TestCatalogDiagnosticUnavailableCacheIsNotMissingCredentials(t *testing.T) {
	for _, tc := range []struct {
		name    string
		key     string
		version int
	}{
		{"old format", "synthetic-key", provider.ModelCacheVersion - 1},
		{"other credential", "other-key", provider.ModelCacheVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateCatalogDiagnostics(t)
			t.Setenv("OPENCODE_API_KEY", "synthetic-key")
			saveDiagnosticCache(t, tc.key, tc.version)
			LoadCachedModels()
			if got := provider.ProviderCatalogStatus(provider.ProviderOpenCodeGo); got.State != provider.CatalogUnavailable {
				t.Fatalf("status = %+v", got)
			}
			if _, err := provider.FindModel(provider.ProviderOpenCodeGo, "saved-test-model"); err == nil {
				t.Fatal("unusable cache became selectable")
			}
		})
	}
}

func TestCatalogDiagnosticFailedRefreshPreservesModelsAndRecovers(t *testing.T) {
	isolateCatalogDiagnostics(t)
	t.Setenv("OPENCODE_API_KEY", "synthetic-key")
	saveDiagnosticCache(t, "synthetic-key", provider.ModelCacheVersion)
	LoadCachedModels()
	discoverOpenCodeGoFn = func(context.Context, string, string) ([]provider.Model, error) {
		if status := provider.ProviderCatalogStatus(provider.ProviderOpenCodeGo); status.State != provider.CatalogRefreshing {
			t.Fatalf("in-flight state = %+v", status)
		}
		return nil, &net.DNSError{Err: "synthetic-secret", Name: "private-host"}
	}
	refreshModels("", "", "", provider.ProviderOpenCodeGo)
	status := provider.ProviderCatalogStatus(provider.ProviderOpenCodeGo)
	if status.State != provider.CatalogFailed || status.Failure.Kind != provider.DiscoveryDNS {
		t.Fatalf("status = %+v", status)
	}
	if _, err := provider.FindModel(provider.ProviderOpenCodeGo, "saved-test-model"); err != nil {
		t.Fatal(err)
	}
	if warnings := strings.Join(provider.CatalogWarnings(), "\n"); strings.Contains(warnings, "synthetic-secret") || !strings.Contains(warnings, "previously loaded models remain available") {
		t.Fatalf("warnings = %s", warnings)
	}
	discoverOpenCodeGoFn = func(context.Context, string, string) ([]provider.Model, error) {
		return []provider.Model{{Provider: provider.ProviderOpenCodeGo, ID: "new-test-model"}}, nil
	}
	refreshModels("", "", "", provider.ProviderOpenCodeGo)
	if got := provider.ProviderCatalogStatus(provider.ProviderOpenCodeGo); got.State != provider.CatalogReady {
		t.Fatalf("recovery status = %+v", got)
	}
	if _, err := provider.FindModel(provider.ProviderOpenCodeGo, "new-test-model"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.FindModel(provider.ProviderOpenCodeGo, "saved-test-model"); err == nil || !strings.Contains(err.Error(), "absent from the published") {
		t.Fatalf("removed model diagnosis = %v", err)
	}
}

func TestCatalogDiagnosticOldRefreshCannotOverwriteNewScope(t *testing.T) {
	isolateCatalogDiagnostics(t)
	t.Setenv("OPENCODE_API_KEY", "synthetic-key")
	LoadCachedModels()
	old := beginCatalogDiscovery(credentialScopeForEndpoint("synthetic-key", ""), catalogDiagnosticRevision)
	if old == 0 {
		t.Fatal("refresh not registered")
	}
	t.Setenv("OPENCODE_API_KEY", "other-key")
	LoadCachedModels()
	current := beginCatalogDiscovery(credentialScopeForEndpoint("other-key", ""), catalogDiagnosticRevision)
	finishCatalogDiscovery(current, provider.CatalogStatus{State: provider.CatalogReady})
	finishCatalogDiscovery(old, provider.CatalogStatus{State: provider.CatalogFailed})
	if got := provider.ProviderCatalogStatus(provider.ProviderOpenCodeGo); got.State != provider.CatalogReady {
		t.Fatalf("old refresh overwrote current state: %+v", got)
	}
	if revision := beginCatalogDiscovery(credentialScopeForEndpoint("synthetic-key", ""), catalogDiagnosticRevision); revision != 0 {
		t.Fatal("other scope changed active diagnostic")
	}
	if revision := beginCatalogDiscovery(credentialScopeForEndpoint("other-key", ""), old); revision != 0 {
		t.Fatal("stale credential resolution claimed the newer preparation")
	}
}

func TestCatalogReloadRejectsInFlightDiscoveryPublication(t *testing.T) {
	for _, nextKey := range []string{"", "other-key"} {
		t.Run("next-key-"+nextKey, func(t *testing.T) {
			isolateCatalogDiagnostics(t)
			t.Setenv("OPENCODE_API_KEY", "synthetic-key")
			saveDiagnosticCache(t, "synthetic-key", provider.ModelCacheVersion)
			LoadCachedModels()
			before, err := os.ReadFile(ModelCachePath())
			if err != nil {
				t.Fatal(err)
			}
			started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			discoverOpenCodeGoFn = func(ctx context.Context, _, _ string) ([]provider.Model, error) {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return []provider.Model{{Provider: provider.ProviderOpenCodeGo, ID: "obsolete-in-flight-model", BaseURL: "https://old.example/v1"}}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(func() { cancel(); <-done })
			go func() {
				defer close(done)
				refreshModelsWithContext(ctx, "", "", "", provider.ProviderOpenCodeGo, apiKeyCommandSkip)
			}()
			<-started
			t.Setenv("OPENCODE_API_KEY", nextKey)
			LoadCachedModels()
			close(release)
			<-done
			if models := provider.ModelsForProvider(provider.ProviderOpenCodeGo); len(models) != 0 {
				t.Fatalf("old scope republished models after reload: %+v", models)
			}
			after, err := os.ReadFile(ModelCachePath())
			if err != nil || string(after) != string(before) {
				t.Fatalf("old scope rewrote cache after reload: %v", err)
			}
		})
	}
}

func TestCatalogRefreshForOldScopeCannotPublish(t *testing.T) {
	isolateCatalogDiagnostics(t)
	t.Setenv("OPENCODE_API_KEY", "current-key")
	LoadCachedModels()
	discoverOpenCodeGoFn = func(context.Context, string, string) ([]provider.Model, error) {
		return []provider.Model{{Provider: provider.ProviderOpenCodeGo, ID: "obsolete-queued-model"}}, nil
	}
	// An SDK refresh can be queued before a reload and start afterwards.
	refreshModels(provider.ProviderOpenCodeGo, "old-key", "", provider.ProviderOpenCodeGo)
	if models := provider.ModelsForProvider(provider.ProviderOpenCodeGo); len(models) != 0 {
		t.Fatalf("queued old scope published models: %+v", models)
	}
	if _, err := os.Stat(ModelCachePath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("queued old scope wrote cache: %v", err)
	}
}

func TestCatalogDiagnosticInvalidCredentialStore(t *testing.T) {
	isolateCatalogDiagnostics(t)
	if err := os.WriteFile(filepath.Join(ZutHome(), "auth.json"), []byte("invalid synthetic credential file"), 0o600); err != nil {
		t.Fatal(err)
	}
	LoadCachedModels()
	if got := provider.ProviderCatalogStatus(provider.ProviderOpenCodeGo); got.State != provider.CatalogCredentialError {
		t.Fatalf("credential read error mislabeled as missing key: %+v", got)
	}
}
