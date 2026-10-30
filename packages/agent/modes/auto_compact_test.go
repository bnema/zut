package modes

import (
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestNormalizeAutoCompactThreshold(t *testing.T) {
	tests := []struct {
		name  string
		value *int
		want  int
	}{
		{name: "missing", value: nil, want: 85},
		{name: "off", value: autoCompactIntPtr(0), want: 0},
		{name: "seventy", value: autoCompactIntPtr(70), want: 70},
		{name: "eighty", value: autoCompactIntPtr(80), want: 80},
		{name: "eighty five", value: autoCompactIntPtr(85), want: 85},
		{name: "ninety", value: autoCompactIntPtr(90), want: 90},
		{name: "ninety five", value: autoCompactIntPtr(95), want: 95},
		{name: "invalid low", value: autoCompactIntPtr(42), want: 85},
		{name: "invalid high", value: autoCompactIntPtr(100), want: 85},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeAutoCompactThreshold(tt.value); got != tt.want {
				t.Fatalf("NormalizeAutoCompactThreshold(%v) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}
}

func TestAutoCompactContinuationIsHiddenFromTranscriptButPersisted(t *testing.T) {
	hidden := provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "legacy compact continuation"}},
		Meta:    map[string]string{autoCompactContinueMetaKey: "true"},
	}
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "visible"}}},
		hidden,
	}
	if !isHiddenTranscriptMessage(hidden) {
		t.Fatal("auto-compaction continuation should be hidden from transcript views")
	}
	filtered := filterHiddenTranscriptMessages(msgs)
	if len(filtered) != 1 || filtered[0].Content[0].(provider.TextBlock).Text != "visible" {
		t.Fatalf("filtered transcript = %#v, want only visible message", filtered)
	}
	if got := hidden.Meta[autoCompactContinueMetaKey]; got != "true" {
		t.Fatalf("persisted continuation metadata = %q, want true", got)
	}
}

func TestShouldAutoCompactUsesConfiguredThreshold(t *testing.T) {
	tests := []struct {
		name      string
		used      int
		window    int
		threshold int
		want      bool
	}{
		{name: "below threshold", used: 84, window: 100, threshold: 85, want: false},
		{name: "at threshold", used: 85, window: 100, threshold: 85, want: true},
		{name: "above threshold", used: 86, window: 100, threshold: 85, want: true},
		{name: "seventy percent preset", used: 70, window: 100, threshold: 70, want: true},
		{name: "eighty percent preset", used: 80, window: 100, threshold: 80, want: true},
		{name: "ninety percent preset", used: 90, window: 100, threshold: 90, want: true},
		{name: "ninety five percent preset", used: 95, window: 100, threshold: 95, want: true},
		{name: "off", used: 100, window: 100, threshold: 0, want: false},
		{name: "missing usage", used: 0, window: 100, threshold: 85, want: false},
		{name: "missing window", used: 85, window: 0, threshold: 85, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldAutoCompact(tt.used, tt.window, tt.threshold); got != tt.want {
				t.Fatalf("ShouldAutoCompact(%d, %d, %d) = %t, want %t", tt.used, tt.window, tt.threshold, got, tt.want)
			}
		})
	}
}

func TestSettingsDialogOffersAutoCompactThresholdPresets(t *testing.T) {
	interactive := NewInteractive(InteractiveConfig{})
	interactive.openSettingsDialog()

	var thresholdItem *settingsItem
	for idx := range interactive.settingsDialog.items {
		item := &interactive.settingsDialog.items[idx]
		if item.key == "auto_compact_threshold" {
			thresholdItem = item
			break
		}
	}
	if thresholdItem == nil {
		t.Fatal("settings dialog is missing auto-compact threshold")
	}

	wantValues := []string{"0", "70", "80", "85", "90", "95"}
	if len(thresholdItem.options) != len(wantValues) {
		t.Fatalf("auto-compact options = %d, want %d", len(thresholdItem.options), len(wantValues))
	}
	for idx, want := range wantValues {
		if got := thresholdItem.options[idx].value; got != want {
			t.Fatalf("auto-compact option %d = %q, want %q", idx, got, want)
		}
	}
	if got := thresholdItem.options[thresholdItem.choice].value; got != "85" {
		t.Fatalf("default auto-compact choice = %q, want 85", got)
	}
}

func TestApplyAutoCompactThresholdSynchronizesLiveUpdate(t *testing.T) {
	initialThreshold := 85
	releaseStore := make(chan struct{})
	store := &blockingAutoCompactSettingsStore{
		entered: make(chan int, 1),
		release: releaseStore,
	}
	interactive := NewInteractive(InteractiveConfig{
		AutoCompactThreshold: &initialThreshold,
		SettingsStore:        store,
	})

	interactive.mu.Lock()
	locked := true
	released := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		interactive.applyAutoCompactThresholdSetting("70")
	}()
	defer func() {
		if !released {
			close(releaseStore)
		}
		if locked {
			interactive.mu.Unlock()
		}
		<-done
	}()

	if got := <-store.entered; got != 70 {
		t.Fatalf("persisted threshold = %d, want 70", got)
	}
	if got := *interactive.cfg.AutoCompactThreshold; got != 85 {
		t.Fatalf("threshold changed without holding Interactive.mu: got %d, want 85", got)
	}

	close(releaseStore)
	released = true
	interactive.mu.Unlock()
	locked = false
	<-done
	if got := *interactive.cfg.AutoCompactThreshold; got != 70 {
		t.Fatalf("threshold after setting completes = %d, want 70", got)
	}
}

type blockingAutoCompactSettingsStore struct {
	SettingsStore
	entered chan int
	release <-chan struct{}
}

func (s *blockingAutoCompactSettingsStore) SetAutoCompactThreshold(percent int) error {
	s.entered <- percent
	<-s.release
	return nil
}

func autoCompactIntPtr(value int) *int {
	return &value
}
