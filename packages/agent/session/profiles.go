package session

import (
	"context"
	"fmt"

	"github.com/bnema/zut/packages/provider"
)

// ProfileSlots is the number of model profile slots.
const ProfileSlots = 9

func (p QuickModelShortcut) empty() bool { return p.Provider == "" && p.Model == "" }

// initProfiles loads slots and validates the initial active slot against the
// live model (resume and CLI overrides may differ).
func (c *Controller) initProfiles() {
	c.loadProfiles()
	if n := c.deps.ActiveProfile; n >= 1 && n <= ProfileSlots {
		c.settings.activeProfile = n
	}
	c.detachOnMismatch()
}

// loadProfiles refreshes the cached slots, always padded to ProfileSlots.
func (c *Controller) loadProfiles() {
	src := c.deps.QuickModelShortcuts
	if c.deps.Profiles != nil {
		src = c.deps.Profiles.Profiles()
	}
	out := make([]QuickModelShortcut, ProfileSlots)
	copy(out, src)
	c.settings.profiles = out
}

// readProfile reads slot n from the store rather than the cached copy.
func (c *Controller) readProfile(n int) (QuickModelShortcut, error) {
	if n < 1 || n > ProfileSlots {
		return QuickModelShortcut{}, fmt.Errorf("unknown model profile %d", n)
	}
	c.loadProfiles()
	p := c.settings.profiles[n-1]
	if p.empty() {
		return QuickModelShortcut{}, ErrProfileEmpty
	}
	return p, nil
}

func (c *Controller) saveProfile(slot int, p QuickModelShortcut, makeDefault bool) error {
	if c.deps.Profiles == nil {
		return nil
	}
	err := c.deps.Profiles.Save(slot, p, makeDefault)
	c.loadProfiles()
	return err
}

func (c *Controller) liveFast() bool { return c.deps.FastMode != nil && *c.deps.FastMode }

// liveProfile is the live model state as a profile entry.
func (c *Controller) liveProfile() QuickModelShortcut {
	return QuickModelShortcut{Provider: c.deps.Provider, Model: c.deps.Model, Reasoning: provider.NormalizeReasoning(c.deps.Reasoning), FastMode: c.liveFast()}
}

// detachOnMismatch keeps the active slot only while it matches the live model.
// An unassigned active slot stays active to receive autosaves.
func (c *Controller) detachOnMismatch() {
	n := c.settings.activeProfile
	if n < 1 || n > len(c.settings.profiles) {
		c.settings.activeProfile = 0
		return
	}
	p := c.settings.profiles[n-1]
	if p.empty() {
		return
	}
	// Applying a slot clamps its reasoning to the model, so compare clamped
	// levels: an over-high slot level still matches its live model.
	p.Reasoning = clampReasoning(p.Provider, p.Model, p.Reasoning)
	if p != c.liveProfile() {
		c.settings.activeProfile = 0
	}
}

// clampReasoning maps level to one the model exposes; unknown models keep the
// normalised level.
func clampReasoning(providerName, model, level string) string {
	m, err := provider.FindModel(providerName, model)
	if err != nil {
		return provider.NormalizeReasoning(level)
	}
	return provider.ClampReasoningForModel(m, level)
}

func (c *Controller) setLiveReasoning(level string) {
	c.deps.Reasoning = level
	if ag := c.turnSeam.currentAgent(); ag != nil {
		ag.Reasoning = level
	}
}

// applyProfile swaps to p and activates slot n; the caller holds the busy guard.
func (c *Controller) applyProfile(n int, p QuickModelShortcut) error {
	if err := provider.ValidateFastMode(p.Provider, p.FastMode); err != nil {
		return err
	}
	if err := c.swapModel(p.Provider, p.Model); err != nil {
		return err
	}
	c.setLiveReasoning(clampReasoning(c.deps.Provider, c.deps.Model, p.Reasoning))
	fast := p.FastMode
	setSetting(&c.deps.FastMode, &fast)
	if ag := c.turnSeam.currentAgent(); ag != nil {
		ag.SetFastMode(fast)
	}
	c.settings.activeProfile = n
	if err := c.saveProfile(n, c.liveProfile(), true); err != nil {
		c.settings.activeProfile = 0
		return fmt.Errorf("model switched; profile not saved: %w", err)
	}
	return nil
}

// autosaveModel stores the live model into the active slot after a swap.
func (c *Controller) autosaveModel() error {
	n := c.settings.activeProfile
	if n == 0 {
		return nil
	}
	c.setLiveReasoning(clampReasoning(c.deps.Provider, c.deps.Model, c.deps.Reasoning))
	if err := c.saveProfile(n, c.liveProfile(), false); err != nil {
		c.settings.activeProfile = 0
		return fmt.Errorf("model switched; profile not saved: %w", err)
	}
	return nil
}

// profileCall runs fn on the actor without the busy guard and publishes Info.
func (c *Controller) profileCall(ctx context.Context, fn func() error) error {
	var result error
	err := c.call(ctx, func() {
		result = fn()
		c.updateInfo()
		c.publish(EvInfo, InfoEvent{Info: c.Info()})
	})
	if err != nil {
		return err
	}
	return result
}

func (c *Controller) busy() bool { return c.turnSeam.isBusy() || c.compact.busy }

// ApplyProfile activates a one-based model profile slot read from the store.
func (c *Controller) ApplyProfile(ctx context.Context, n int) error {
	return c.settingsCall(ctx, func() error {
		p, err := c.readProfile(n)
		if err != nil {
			return err
		}
		return c.applyProfile(n, p)
	})
}

// AssignProfile stores p in slot. It switches the model when activate is set or
// slot is active (rejected during a turn); otherwise it only saves.
func (c *Controller) AssignProfile(ctx context.Context, slot int, p QuickModelShortcut, activate bool) error {
	if slot < 1 || slot > ProfileSlots {
		return fmt.Errorf("unknown model profile %d", slot)
	}
	if p.empty() {
		return ErrProfileEmpty
	}
	if m, err := provider.FindModel(p.Provider, p.Model); err == nil {
		p.Provider, p.Model = m.Provider, m.ID
		p.Reasoning = provider.ClampReasoningForModel(m, p.Reasoning)
	} else {
		// Unlisted models (user or live catalogs) are kept as given.
		p.Reasoning = provider.NormalizeReasoning(p.Reasoning)
	}
	if err := provider.ValidateFastMode(p.Provider, p.FastMode); err != nil {
		return err
	}
	return c.profileCall(ctx, func() error {
		if activate || slot == c.settings.activeProfile {
			if c.busy() {
				return ErrBusy
			}
			return c.applyProfile(slot, p)
		}
		return c.saveProfile(slot, p, false)
	})
}

// ClearProfile empties slot; an active slot detaches but the model stays.
func (c *Controller) ClearProfile(ctx context.Context, slot int) error {
	if slot < 1 || slot > ProfileSlots {
		return fmt.Errorf("unknown model profile %d", slot)
	}
	return c.profileCall(ctx, func() error {
		if err := c.saveProfile(slot, QuickModelShortcut{}, false); err != nil {
			return err
		}
		if c.settings.activeProfile == slot {
			c.settings.activeProfile = 0
		}
		return nil
	})
}

// ProfilesChanged reloads slots after an external store change, detaching the
// active slot when it no longer matches the live model.
func (c *Controller) ProfilesChanged(ctx context.Context) error {
	return c.post(ctx, func() {
		c.loadProfiles()
		c.detachOnMismatch()
		c.updateInfo()
		c.publish(EvInfo, InfoEvent{Info: c.Info()})
	})
}
