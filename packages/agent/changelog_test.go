package agent

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/agent/config"
)

func TestFetchChangelogSkipsDevVersions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, version := range []string{"0.0.0", "0.0.0-dev", "0.0.0-dev (abc123, today)"} {
		info, err := FetchChangelog(ctx, version)
		if err != nil {
			t.Fatalf("FetchChangelog(%q) error = %v", version, err)
		}
		if info != (ChangelogInfo{}) {
			t.Fatalf("FetchChangelog(%q) = %+v, want empty result", version, info)
		}
	}
}

func TestShouldShowChangelogSkipsDevVersions(t *testing.T) {
	cfg := config.Config{LastChangelogShown: "0.0.1"}
	for _, version := range []string{"0.0.0", "0.0.0-dev", "0.0.0-dev (abc123, today)"} {
		if ShouldShowChangelog(version, cfg) {
			t.Errorf("ShouldShowChangelog(%q) = true, want false", version)
		}
	}
}
