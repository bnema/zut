package provider

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSaveCacheUsesRestrictivePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce POSIX file permissions")
	}
	path := filepath.Join(t.TempDir(), "models.json")
	if err := SaveCache(path, ModelCache{ProviderScopes: map[string]string{"openai-codex": "account"}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("cache permissions = %04o, want 0600", got)
	}
	loaded, err := LoadCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != ModelCacheVersion {
		t.Fatalf("cache version = %d, want %d", loaded.Version, ModelCacheVersion)
	}
}

func TestModelCacheUsesTwentyFourHourTTL(t *testing.T) {
	if !(ModelCache{Version: ModelCacheVersion, FetchedAt: time.Now().Add(-23 * time.Hour)}).IsFresh() {
		t.Fatal("cache from 23 hours ago should be fresh")
	}
	if (ModelCache{Version: ModelCacheVersion, FetchedAt: time.Now().Add(-25 * time.Hour)}).IsFresh() {
		t.Fatal("cache from 25 hours ago should be stale")
	}
	if (ModelCache{Version: ModelCacheVersion - 1, FetchedAt: time.Now()}).IsFresh() {
		t.Fatal("old-version cache should not be fresh")
	}
}
