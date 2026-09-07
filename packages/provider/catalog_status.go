package provider

import (
	"fmt"
	"maps"
	"slices"
)

// CatalogState describes discovery, not inference entitlement. A published
// model can still be rejected by the provider at inference time.
type CatalogState uint8

const (
	CatalogUnknown CatalogState = iota
	CatalogMissingCredentials
	CatalogCredentialError
	CatalogCredentialsDeferred
	CatalogUnavailable
	CatalogRefreshing
	CatalogCached
	CatalogReady
	CatalogFailed
)

// CatalogStatus is ephemeral diagnostic information, kept separately from
// model metadata. Failure contains only sanitized discovery information.
type CatalogStatus struct {
	State   CatalogState
	Failure DiscoveryError
	// PreviouslyDiscovered identifies providers present in an on-disk or live
	// catalog, even when their cached models cannot be used with this scope.
	PreviouslyDiscovered bool
}

// Description explains catalog availability without suggesting that a failed
// discovery proves the requested model does not exist.
func (s CatalogStatus) Description() string {
	switch s.State {
	case CatalogMissingCredentials:
		return "no API key available in this process; export the provider API key before starting zut or use /login"
	case CatalogCredentialError:
		return "could not load provider credentials; check auth.json or the configured api_key_command"
	case CatalogCredentialsDeferred:
		return "credential command not executed by background discovery; run zut --list-models to refresh"
	case CatalogUnavailable:
		return "no usable model catalog for the current credentials and endpoint; discovery has not completed"
	case CatalogRefreshing:
		return "model catalog discovery in progress"
	case CatalogCached:
		return "using a cached model catalog"
	case CatalogReady:
		return "model catalog fetched successfully"
	case CatalogFailed:
		return "model catalog discovery failed: " + s.Failure.Error()
	default:
		return "model catalog availability is unknown"
	}
}

// ProviderCatalogStatus returns the last diagnostic published for a provider.
func ProviderCatalogStatus(name string) CatalogStatus {
	activeMu.RLock()
	defer activeMu.RUnlock()
	return catalogStatuses[name]
}

// ProviderCatalogStatuses returns a copy for catalog diagnostic displays.
func ProviderCatalogStatuses() map[string]CatalogStatus {
	activeMu.RLock()
	defer activeMu.RUnlock()
	return maps.Clone(catalogStatuses)
}

// SetProviderCatalogStatus publishes diagnostic state without replacing models.
func SetProviderCatalogStatus(name string, status CatalogStatus) {
	activeMu.Lock()
	defer activeMu.Unlock()
	if catalogStatuses == nil {
		catalogStatuses = make(map[string]CatalogStatus)
	}
	catalogStatuses[name] = status
}

// CatalogWarnings explains missing providers and degraded catalogs. Providers
// without credentials are mentioned only if previously discovered or named by
// the caller (for example, a saved model profile).
func CatalogWarnings(knownProviders ...string) []string {
	statuses := ProviderCatalogStatuses()
	names := slices.Sorted(maps.Keys(statuses))
	var warnings []string
	for _, name := range names {
		status := statuses[name]
		hasModels := len(ModelsForProvider(name)) > 0
		switch status.State {
		case CatalogUnknown:
			continue
		case CatalogMissingCredentials:
			if !status.PreviouslyDiscovered && !slices.Contains(knownProviders, name) {
				continue
			}
		case CatalogReady, CatalogCached:
			if hasModels {
				continue
			}
		}
		message := name + ": " + status.Description()
		if hasModels {
			message += "; previously loaded models remain available"
		} else {
			message += "; no selectable models"
		}
		warnings = append(warnings, message)
	}
	return warnings
}

// ModelLookupError distinguishes a local lookup miss from a discovery failure.
// It never claims that a provider is unreachable merely because a key is absent.
type ModelLookupError struct {
	Provider string
	Model    string
	Catalog  CatalogStatus
}

func (e *ModelLookupError) Error() string {
	switch e.Catalog.State {
	case CatalogUnknown:
		return fmt.Sprintf("unknown model %q (provider=%q)", e.Model, e.Provider)
	case CatalogReady:
		return fmt.Sprintf("model %q is absent from the published %s catalog; choose a model with /model", e.Model, e.Provider)
	case CatalogCached:
		return fmt.Sprintf("model %q is absent from the cached %s catalog; choose a listed model with /model (cached availability is not a live check)", e.Model, e.Provider)
	default:
		return fmt.Sprintf("cannot select model %q (provider=%q): %s", e.Model, e.Provider, e.Catalog.Description())
	}
}
