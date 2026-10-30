package agent

import (
	"fmt"
	"sync"

	"github.com/bnema/zut/packages/agent/session"
)

// modelProfileStore is the daemon-wide session.ProfileStore over config.json.
// Writes go through updateConfig, which serialises them with every other
// in-process config update; mu orders store writes and cache refreshes.
type modelProfileStore struct {
	mu     sync.Mutex
	slots  [session.ProfileSlots]session.QuickModelShortcut
	active int
	// onSaved runs after each successful Save, outside the lock.
	onSaved func()
}

var _ session.ProfileStore = (*modelProfileStore)(nil)

// Reload refreshes the cache from disk so hand edits reach new sessions.
func (s *modelProfileStore) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	configMu.Lock()
	cfg, err := LoadConfig()
	configMu.Unlock()
	if err != nil {
		return err
	}
	s.cache(cfg)
	return nil
}

func (s *modelProfileStore) cache(cfg Config) {
	s.slots = [session.ProfileSlots]session.QuickModelShortcut{}
	for i, p := range cfg.QuickModelShortcuts {
		if i >= session.ProfileSlots {
			break
		}
		if p.Provider == "" && p.Model == "" {
			continue
		}
		p = normalizedModelProfile(p)
		s.slots[i] = session.QuickModelShortcut{Provider: p.Provider, Model: p.Model, Reasoning: p.Reasoning, FastMode: p.FastMode}
	}
	s.active = cfg.ActiveModelProfile
}

// Profiles returns a copy of the cached slots.
func (s *modelProfileStore) Profiles() []session.QuickModelShortcut {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]session.QuickModelShortcut(nil), s.slots[:]...)
}

// ActiveDefault is the configured active slot for new sessions, or 0 (none)
// when the config holds none or an invalid one.
func (s *modelProfileStore) ActiveDefault() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active < 1 || s.active > session.ProfileSlots {
		return 0
	}
	return s.active
}

// Save writes slot (an empty p clears it). makeDefault also records slot as
// active_model_profile with the startup provider, model, reasoning and fast
// mode; clearing the configured active slot resets it to 0.
func (s *modelProfileStore) Save(slot int, p session.QuickModelShortcut, makeDefault bool) error {
	if err := s.save(slot, p, makeDefault); err != nil {
		return err
	}
	if s.onSaved != nil {
		s.onSaved()
	}
	return nil
}

func (s *modelProfileStore) save(slot int, p session.QuickModelShortcut, makeDefault bool) error {
	if slot < 1 || slot > session.ProfileSlots {
		return fmt.Errorf("model profile slot must be between 1 and %d", session.ProfileSlots)
	}
	empty := p.Provider == "" && p.Model == ""
	if empty && makeDefault {
		return fmt.Errorf("cannot activate an empty model profile")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var saved Config
	err := updateConfig(func(cfg *Config) error {
		active := cfg.ActiveModelProfile
		switch {
		case makeDefault:
			active = slot
		case empty && active == slot:
			active = 0
		}
		if active < 0 || active > session.ProfileSlots {
			active = 0
		}
		if err := setModelProfile(cfg, slot, QuickModelShortcut{Provider: p.Provider, Model: p.Model, Reasoning: p.Reasoning, FastMode: p.FastMode}, active); err != nil {
			return err
		}
		saved = *cfg
		return nil
	})
	if err != nil {
		return err
	}
	s.cache(saved)
	return nil
}
