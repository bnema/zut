package agent

import (
	"fmt"
	"sync"

	"github.com/bnema/zut/packages/agent/config"
	"github.com/bnema/zut/packages/agent/session"
	providerpkg "github.com/bnema/zut/packages/provider"
)

// modelProfileStore is the daemon-wide session.ProfileStore over config.json.
// Writes go through config.Update, which serialises them with every other
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
	cfg, err := config.LoadLocked()
	if err != nil {
		return err
	}
	s.cache(cfg)
	return nil
}

func (s *modelProfileStore) cache(cfg config.Config) {
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
	var saved config.Config
	err := config.Update(func(cfg *config.Config) error {
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
		if err := setModelProfile(cfg, slot, config.QuickModelShortcut{Provider: p.Provider, Model: p.Model, Reasoning: p.Reasoning, FastMode: p.FastMode}, active); err != nil {
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

// normalizedModelProfile keeps startup defaults and the TUI's copy consistent.
// Unknown/open-catalog models retain their configured reasoning until resolved.
func normalizedModelProfile(p config.QuickModelShortcut) config.QuickModelShortcut {
	p.Provider = canonicalProvider(p.Provider)
	p.Reasoning = providerpkg.NormalizeReasoning(p.Reasoning)
	if model, err := providerpkg.FindModel(p.Provider, p.Model); err == nil {
		p.Reasoning = providerpkg.ClampReasoningForModel(model, p.Reasoning)
	}
	return p
}

// applyActiveModelProfile supplies defaults without rewriting the config and
// returns their source slot (0 when none). Explicit flags retain precedence.
func applyActiveModelProfile(c *config.Config) int {
	slot := c.ActiveModelProfile
	if slot < 1 || slot > 9 {
		return 0
	}
	if slot > len(c.QuickModelShortcuts) {
		return 0
	}
	p := normalizedModelProfile(c.QuickModelShortcuts[slot-1])
	if p.Provider == "" || p.Model == "" {
		return 0
	}
	c.Provider, c.Model, c.Reasoning = p.Provider, p.Model, p.Reasoning
	fastMode := p.FastMode
	c.FastMode = &fastMode
	return slot
}
