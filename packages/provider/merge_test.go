package provider

import "testing"

func TestMergeCatalogForProvidersPrunesUnavailableStaticModels(t *testing.T) {
	const (
		providerID = "openai-codex"
		liveID     = "gpt-5.6-luna"
		absentID   = "gpt-5.6-sol"
	)

	staticPresent := false
	for _, model := range Catalog {
		if model.Provider == providerID && model.ID == absentID {
			staticPresent = true
			break
		}
	}
	if !staticPresent {
		t.Fatalf("fixture drift: static model %s/%s is no longer in Catalog", providerID, absentID)
	}

	merged := MergeCatalogForProviders([]Model{{
		Provider: providerID,
		ID:       liveID,
		Source:   "live",
	}}, []string{providerID})

	foundLive := false
	for _, model := range merged {
		if model.Provider != providerID {
			continue
		}
		if model.ID == absentID {
			t.Fatalf("unavailable static model %s/%s remains in authoritative catalog", providerID, absentID)
		}
		if model.ID == liveID {
			foundLive = true
		}
	}
	if !foundLive {
		t.Fatalf("live model %s/%s missing from authoritative catalog", providerID, liveID)
	}
}

func TestMergeCatalogForProvidersPreservesStaticMetadata(t *testing.T) {
	merged := MergeCatalogForProviders([]Model{{
		Provider: "openai-codex",
		ID:       "gpt-5.6-luna",
		Source:   "live",
	}}, []string{"openai-codex"})
	for _, model := range merged {
		if model.Provider == "openai-codex" && model.ID == "gpt-5.6-luna" {
			if model.ContextWindow == 0 || model.MaxOutput == 0 || model.PriceInput == 0 || model.PriceOutput == 0 {
				t.Fatalf("live model did not retain static metadata: %+v", model)
			}
			return
		}
	}
	t.Fatal("live Codex model missing from merged catalog")
}

func TestSetLiveModelsForProvidersPrunesEmptyAuthoritativeCatalog(t *testing.T) {
	if _, err := FindModel("openai-codex", "gpt-5.6-luna"); err != nil {
		t.Fatalf("fixture drift: static Codex model missing: %v", err)
	}
	activeMu.RLock()
	previousActive := active
	previousSet := activeSet
	activeMu.RUnlock()
	t.Cleanup(func() {
		activeMu.Lock()
		active = previousActive
		activeSet = previousSet
		activeMu.Unlock()
	})

	SetLiveModelsForProviders(nil, []string{"openai-codex"})
	if _, err := FindModel("openai-codex", "gpt-5.6-luna"); err == nil {
		t.Fatal("empty authoritative catalog retained a static Codex model")
	}
}
