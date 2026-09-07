package modes

import (
	"errors"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/auth"
	"github.com/bnema/zut/packages/tui"
	"github.com/mattn/go-runewidth"
)

func TestModelPickerExplainsMissingSavedProvider(t *testing.T) {
	snapshot := provider.SnapshotCatalog()
	t.Cleanup(func() { provider.RestoreCatalog(snapshot) })
	provider.ClearLiveModelsForProvider(provider.ProviderOpenCodeGo)
	provider.SetProviderCatalogStatus(provider.ProviderOpenCodeGo, provider.CatalogStatus{State: provider.CatalogMissingCredentials})
	i := NewInteractive(InteractiveConfig{
		Theme:               tui.Dark,
		Provider:            "openai-codex",
		QuickModelShortcuts: []QuickModelShortcut{{Provider: provider.ProviderOpenCodeGo, Model: "saved-test-model"}},
		LoggedInProviders:   func() []string { return []string{"openai-codex"} },
	})
	i.openModelPickerAfterRefresh(nil)
	text := stripANSIBytes(strings.Join(i.modelDialog.Render(tui.Dark, 120), "\n"))
	if !strings.Contains(text, "opencode-go:") || !strings.Contains(text, "no API key available in this process") {
		t.Fatalf("picker hid unavailable saved provider: %s", text)
	}
	for _, model := range i.modelDialog.view {
		if model.Provider == provider.ProviderOpenCodeGo {
			t.Fatal("diagnostic fabricated a selectable model")
		}
	}
	i.applyQuickModelShortcut(1)
	if !strings.Contains(i.statusErr, "no API key available in this process") {
		t.Fatalf("profile selection lost cause: %s", i.statusErr)
	}
	if i.cfg.Provider != "openai-codex" || i.cfg.ActiveModelProfile != 0 {
		t.Fatal("failed selection changed current profile")
	}
}

func TestModelPickerDoesNotConfuseDiscoveryFailureWithMissingKey(t *testing.T) {
	snapshot := provider.SnapshotCatalog()
	t.Cleanup(func() { provider.RestoreCatalog(snapshot) })
	provider.ClearLiveModelsForProvider(provider.ProviderOpenCodeGo)
	provider.SetProviderCatalogStatus(provider.ProviderOpenCodeGo, provider.CatalogStatus{State: provider.CatalogFailed, Failure: provider.DiscoveryError{Kind: provider.DiscoveryDNS}})
	d := newModelDialog()
	d.Open("", []string{provider.ProviderOpenCodeGo})
	text := stripANSIBytes(strings.Join(d.Render(tui.Dark, 120), "\n"))
	if !strings.Contains(text, "DNS resolution failed") || strings.Contains(text, "no credentials found") {
		t.Fatalf("wrong empty catalog diagnosis: %s", text)
	}
	// Only the new diagnostic block is checked: pre-existing picker headers
	// and hints have their own width contracts.
	warning := provider.CatalogWarnings()[0]
	for _, width := range []int{1, 12, 40} {
		for _, line := range tui.WrapANSILine(warning, width) {
			if got := runewidth.StringWidth(stripANSIBytes(line)); got > width {
				t.Fatalf("warning width %d exceeds %d", got, width)
			}
		}
		_ = d.Render(tui.Dark, width)
	}
}

func TestLoginReloadsCatalogBeforeBuildingAgent(t *testing.T) {
	reloaded := false
	i := NewInteractive(InteractiveConfig{
		Theme:              tui.Dark,
		ReloadModelCatalog: func() { reloaded = true },
		BuildAgent: func() (*core.Agent, string, string, error) {
			if !reloaded {
				t.Fatal("build used pre-login catalog state")
			}
			return nil, "", "", errors.New("synthetic build failure")
		},
	})
	i.handleAuthEvent(auth.Event{Kind: "success", Provider: provider.ProviderOpenCodeGo, Method: "apikey"})
	if !reloaded {
		t.Fatal("login did not refresh catalog diagnostics")
	}
}
