package modes

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

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

func TestClassifyCompactionContinuation(t *testing.T) {
	tests := []struct {
		name         string
		origin       compactContinuationOrigin
		statusActive bool
		stop         provider.StopReason
		turnErr      error
		msgs         []provider.Message
		want         compactContinuationReason
	}{
		{
			name:   "completed text answer",
			origin: compactOriginAfterTurnThreshold,
			stop:   provider.StopEnd,
			msgs: []provider.Message{{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "done"}},
			}},
			want: compactContinuationNone,
		},
		{
			name:   "reasoning only is structural",
			origin: compactOriginAfterTurnThreshold,
			stop:   provider.StopEnd,
			msgs: []provider.Message{{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.ReasoningBlock{Summary: "still working"}},
			}},
			want: compactContinuationStructuralTail,
		},
		{
			name:   "tool call is structural",
			origin: compactOriginAfterTurnThreshold,
			stop:   provider.StopEnd,
			msgs: []provider.Message{{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.ToolCallBlock{Name: "read"}},
			}},
			want: compactContinuationStructuralTail,
		},
		{
			name:   "user tail is structural",
			origin: compactOriginAfterTurnThreshold,
			stop:   provider.StopEnd,
			msgs: []provider.Message{{
				Role:    provider.RoleUser,
				Content: []provider.Content{provider.TextBlock{Text: "continue"}},
			}},
			want: compactContinuationStructuralTail,
		},
		{
			name:   "next will inspect is status rescue",
			origin: compactOriginAfterTurnThreshold,
			stop:   provider.StopEnd,
			msgs: []provider.Message{{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "Next I will inspect the remaining call sites."}},
			}},
			want: compactContinuationStatusRescue,
		},
		{
			name:   "ill now run tests is status rescue",
			origin: compactOriginAfterTurnThreshold,
			stop:   provider.StopEnd,
			msgs: []provider.Message{{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "I’ll now run the targeted tests."}},
			}},
			want: compactContinuationStatusRescue,
		},
		{
			name:   "need to continue at sentence end is status rescue",
			origin: compactOriginAfterTurnThreshold,
			stop:   provider.StopEnd,
			msgs: []provider.Message{{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "I need to continue."}},
			}},
			want: compactContinuationStatusRescue,
		},
		{
			name:   "embedded phrase is not status rescue",
			origin: compactOriginAfterTurnThreshold,
			stop:   provider.StopEnd,
			msgs: []provider.Message{{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "The mini will now absorb this conceptual example."}},
			}},
			want: compactContinuationNone,
		},
		{
			name:   "generic next step prose is not status rescue",
			origin: compactOriginAfterTurnThreshold,
			stop:   provider.StopEnd,
			msgs: []provider.Message{{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "The next step in this algorithm is normalization."}},
			}},
			want: compactContinuationNone,
		},
		{
			name:   "empty",
			origin: compactOriginAfterTurnThreshold,
			stop:   provider.StopEnd,
			want:   compactContinuationNone,
		},
		{
			name:   "aborted status is not rescue",
			origin: compactOriginAfterTurnThreshold,
			stop:   provider.StopAborted,
			msgs: []provider.Message{{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "Next I will inspect the remaining call sites."}},
			}},
			want: compactContinuationNone,
		},
		{
			name:    "error status is not rescue",
			origin:  compactOriginAfterTurnThreshold,
			stop:    provider.StopEnd,
			turnErr: errors.New("provider failed"),
			msgs: []provider.Message{{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "Next I will inspect the remaining call sites."}},
			}},
			want: compactContinuationNone,
		},
		{
			name:   "manual does not admit initial status rescue",
			origin: compactOriginManual,
			stop:   provider.StopEnd,
			msgs: []provider.Message{{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "Next I will inspect the remaining call sites."}},
			}},
			want: compactContinuationNone,
		},
		{
			name:   "pre-turn does not admit initial status rescue",
			origin: compactOriginPreTurnThreshold,
			stop:   provider.StopEnd,
			msgs: []provider.Message{{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "Next I will inspect the remaining call sites."}},
			}},
			want: compactContinuationNone,
		},
		{
			name:   "recovery does not admit initial status rescue",
			origin: compactOriginRecovery,
			stop:   provider.StopEnd,
			msgs: []provider.Message{{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "Next I will inspect the remaining call sites."}},
			}},
			want: compactContinuationNone,
		},
		{
			name:         "active rescue may classify its own follow-up",
			origin:       compactOriginManual,
			statusActive: true,
			stop:         provider.StopEnd,
			msgs: []provider.Message{{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.TextBlock{Text: "Then I will inspect the remaining call sites."}},
			}},
			want: compactContinuationStatusRescue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyCompactionContinuation(tt.origin, tt.statusActive, tt.stop, tt.turnErr, tt.msgs); got != tt.want {
				t.Fatalf("classifyCompactionContinuation() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestAutoCompactContinuationIsHiddenFromTranscriptButPersisted(t *testing.T) {
	hidden := provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: autoCompactContinuationPrompt}},
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

func TestNewInteractiveRestoresValidCompactHandoff(t *testing.T) {
	interactive := NewInteractive(InteractiveConfig{
		InitialCompactHandoff: json.RawMessage(`{"version":1,"reason":"status_rescue","rescue_attempts":1}`),
	})
	if got, want := interactive.compactContinuation, (compactContinuationState{reason: compactContinuationStatusRescue, rescueAttempts: 1}); got != want {
		t.Fatalf("restored compact handoff = %#v, want %#v", got, want)
	}
}

func TestCancelTurnCancelsBeforePersistingCompactHandoff(t *testing.T) {
	persisted := make(chan string, 1)
	cancelled := make(chan struct{})
	persistenceStarted := make(chan struct{})
	releasePersistence := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releasePersistence)
		}
	}()

	interactive := NewInteractive(InteractiveConfig{
		InitialCompactHandoff: json.RawMessage(`{"version":1,"reason":"status_rescue","rescue_attempts":1}`),
		PersistCompactHandoff: func(state json.RawMessage) error {
			close(persistenceStarted)
			<-releasePersistence
			persisted <- string(state)
			return nil
		},
	})
	interactive.mu.Lock()
	interactive.busy = true
	interactive.cancelTurn = func() { close(cancelled) }
	interactive.mu.Unlock()

	done := make(chan struct{})
	go func() {
		interactive.CancelTurn()
		close(done)
	}()

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation waited for compact handoff persistence")
	}
	select {
	case <-persistenceStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not persist the cleared compact handoff")
	}
	close(releasePersistence)
	released = true
	<-done
	select {
	case state := <-persisted:
		if state != "" {
			t.Fatalf("cleared compact handoff = %q, want empty", state)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not finish compact handoff persistence")
	}
}

func TestDecodeCompactHandoff(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want compactContinuationState
	}{
		{
			name: "status rescue",
			raw:  `{"version":1,"reason":"status_rescue","rescue_attempts":1}`,
			want: compactContinuationState{reason: compactContinuationStatusRescue, rescueAttempts: 1},
		},
		{
			name: "forced length",
			raw:  `{"version":1,"reason":"forced_length"}`,
			want: compactContinuationState{reason: compactContinuationForcedLength},
		},
		{
			name: "goal compaction pending",
			raw:  `{"version":1,"reason":"goal_compaction_pending"}`,
			want: compactContinuationState{reason: compactContinuationGoalCompactionPending},
		},
		{
			name: "goal",
			raw:  `{"version":1,"reason":"goal"}`,
			want: compactContinuationState{reason: compactContinuationGoal},
		},
		{
			name: "invalid status attempt", raw: `{"version":1,"reason":"status_rescue","rescue_attempts":3}`},
		{name: "unknown reason", raw: `{"version":1,"reason":"other"}`},
		{name: "invalid JSON", raw: `{`},
		{name: "missing", raw: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decodeCompactHandoff([]byte(tt.raw)); got != tt.want {
				t.Fatalf("decodeCompactHandoff(%q) = %#v, want %#v", tt.raw, got, tt.want)
			}
		})
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
