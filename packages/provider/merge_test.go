package provider

import "testing"

func TestMergeCatalogForProvidersPrunesUnavailableStaticModels(t *testing.T) {
	const (
		providerID = "openai-codex"
		liveID     = "gpt-5.6-luna"
		absentID   = "gpt-5.6-sol"
	)

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

func TestSetLiveModelsForProvidersPrunesEmptyAuthoritativeCatalog(t *testing.T) {
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
