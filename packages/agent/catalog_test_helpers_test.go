package agent

import (
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func preserveProviderCatalog(t *testing.T) {
	t.Helper()
	snapshot := provider.SnapshotCatalog()
	t.Cleanup(func() {
		provider.RestoreCatalog(snapshot)
	})
}
