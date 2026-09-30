package agent

import (
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestRefreshCopilotAvailabilityWithoutCredentials(t *testing.T) {
	preserveProviderCatalog(t)
	t.Setenv("ZUT_HOME", t.TempDir())
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_COPILOT_TOKEN", "")
	provider.SetModelAvailability("github-copilot", []string{})
	refreshCopilotModelAvailability()
	if len(provider.ModelsForProvider("github-copilot")) == 0 {
		t.Fatal("account restriction was retained after credentials disappeared")
	}
}

func TestCopilotDefaultUsesSonnet5(t *testing.T) {
	id := defaultModelForProvider("github-copilot")
	if id != "claude-sonnet-5" {
		t.Fatalf("Copilot default = %q, want claude-sonnet-5", id)
	}
	if _, err := provider.FindModel("github-copilot", id); err != nil {
		t.Fatalf("Copilot default missing from catalog: %v", err)
	}
}
