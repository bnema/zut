package agent

import (
	"context"
	"errors"

	"github.com/bnema/zut/packages/provider"
)

// These fields share modelCatalogMu with catalog preparation/publication. A
// result from an older refresh or another credential scope cannot overwrite
// the active catalog, cache, or diagnostic for the currently loaded runtime.
var catalogDiagnosticPath string
var catalogDiagnosticScope string
var catalogDiagnosticRevision uint64

func resetCatalogDiagnostic(scopes map[string]string) {
	catalogDiagnosticPath = ModelCachePath()
	catalogDiagnosticScope = scopes[provider.ProviderOpenCodeGo]
	catalogDiagnosticRevision++
	state := provider.CatalogUnavailable
	if catalogDiagnosticScope == "" {
		state = provider.CatalogMissingCredentials
		_, _, _, err := resolveCredentialFull(context.Background(), provider.ProviderOpenCodeGo, "", apiKeyCommandAvailable)
		if err == nil {
			state = provider.CatalogCredentialsDeferred
		} else if !errors.Is(err, errNoCredential) {
			state = provider.CatalogCredentialError
		}
	}
	provider.SetProviderCatalogStatus(provider.ProviderOpenCodeGo, provider.CatalogStatus{State: state})
}

func beginCatalogDiscovery(scope string) uint64 {
	modelCatalogMu.Lock()
	defer modelCatalogMu.Unlock()
	if catalogDiagnosticPath != ModelCachePath() {
		// RefreshModelsAsync is also callable before loading a cache. Claim
		// that state directory without borrowing another runtime's scope.
		catalogDiagnosticPath = ModelCachePath()
		catalogDiagnosticScope = scope
	}
	if scope != catalogDiagnosticScope {
		return 0
	}
	catalogDiagnosticRevision++
	status := provider.ProviderCatalogStatus(provider.ProviderOpenCodeGo)
	status.State = provider.CatalogRefreshing
	status.Failure = provider.DiscoveryError{}
	provider.SetProviderCatalogStatus(provider.ProviderOpenCodeGo, status)
	return catalogDiagnosticRevision
}

func finishCatalogDiscovery(revision uint64, status provider.CatalogStatus) {
	modelCatalogMu.Lock()
	defer modelCatalogMu.Unlock()
	if revision != 0 && revision == catalogDiagnosticRevision {
		previous := provider.ProviderCatalogStatus(provider.ProviderOpenCodeGo)
		status.PreviouslyDiscovered = previous.PreviouslyDiscovered || status.State == provider.CatalogReady
		provider.SetProviderCatalogStatus(provider.ProviderOpenCodeGo, status)
	}
}
