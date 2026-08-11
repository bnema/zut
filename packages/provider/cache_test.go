package provider

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
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
}
