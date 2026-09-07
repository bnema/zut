package provider

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// ModelCache is the on-disk shape for discovered models.
type ModelCache struct {
	Version                int               `json:"version,omitempty"`
	FetchedAt              time.Time         `json:"fetched_at"`
	Models                 []Model           `json:"models"`
	AuthoritativeProviders []string          `json:"authoritative_providers,omitempty"`
	ProviderScopes         map[string]string `json:"provider_scopes,omitempty"`
}

// ModelCacheVersion invalidates caches created before OpenCode Go protocol
// routing was based on models.dev adapters. Older caches must not be loaded as
// routing metadata because they can silently select the wrong wire protocol.
const ModelCacheVersion = 4

// CacheTTL is how long a discovered list is considered fresh. Model metadata
// and availability change less often than a typical process starts, so keep
// the synchronized catalog for one day before querying its sources again.
const CacheTTL = 24 * time.Hour

// LoadCache reads the model cache from path. Returns an empty ModelCache
// (no error) if the file does not exist.
func LoadCache(path string) (ModelCache, error) {
	var c ModelCache
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	for i := range c.Models {
		if c.Models[i].Source == "" {
			c.Models[i].Source = "cache"
		}
	}
	return c, nil
}

// SaveCache writes the cache atomically.
func SaveCache(path string, c ModelCache) error {
	if c.Version == 0 {
		c.Version = ModelCacheVersion
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// IsFresh reports whether the current-format cache was fetched within CacheTTL.
func (c ModelCache) IsFresh() bool {
	if c.Version != ModelCacheVersion || c.FetchedAt.IsZero() {
		return false
	}
	return time.Since(c.FetchedAt) < CacheTTL
}
