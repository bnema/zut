package modes

import (
	"fmt"

	"github.com/bnema/zut/packages/provider"
)

// detachMismatchedModelProfile is called under the UI lock (or before startup)
// after host-driven session/model changes, which must not rewrite favorites.
func (c *InteractiveConfig) detachMismatchedModelProfile() {
	slot := c.ActiveModelProfile
	if slot < 1 || slot > 9 || slot > len(c.QuickModelShortcuts) {
		c.ActiveModelProfile = 0
		return
	}
	p := c.QuickModelShortcuts[slot-1]
	liveFastMode := c.FastMode != nil && *c.FastMode
	if p.Provider != c.Provider || p.Model != c.Model || provider.NormalizeReasoning(p.Reasoning) != provider.NormalizeReasoning(c.Reasoning) || p.FastMode != liveFastMode {
		c.ActiveModelProfile = 0
	}
}

func modelProfileDescription(p QuickModelShortcut) string {
	level := p.Reasoning
	if level == "" {
		level = "off"
	}
	fast := "off"
	if p.FastMode {
		fast = "on"
	}
	return p.Provider + " / " + p.Model + " (reasoning: " + level + ", fast: " + fast + ")"
}

// persistModelProfile changes in-memory favorites only after saving succeeds.
func (i *Interactive) persistModelProfile(slot int, p QuickModelShortcut, active int) bool {
	if i.cfg.SettingsStore != nil {
		var err error
		if store, ok := i.cfg.SettingsStore.(modelProfileSettingsStore); ok {
			err = store.SetModelProfile(slot, p, active)
		} else {
			err = i.cfg.SettingsStore.SetQuickModelShortcut(slot, p.Provider, p.Model)
			active = 0 // legacy stores only persist provider/model bindings
		}
		if err != nil {
			i.mu.Lock()
			i.statusOK = ""
			i.statusErr = "profile not saved: " + err.Error()
			i.mu.Unlock()
			i.invalidate()
			return false
		}
	}
	i.mu.Lock()
	if len(i.cfg.QuickModelShortcuts) < slot {
		next := make([]QuickModelShortcut, slot)
		copy(next, i.cfg.QuickModelShortcuts)
		i.cfg.QuickModelShortcuts = next
	}
	i.cfg.QuickModelShortcuts[slot-1] = p
	i.cfg.ActiveModelProfile = active
	i.mu.Unlock()
	for n := 1; n <= 9; n++ {
		i.refreshQuickModelSettingsItem(n)
	}
	return true
}

func (i *Interactive) setQuickModelProfile(slot int, p QuickModelShortcut) {
	if slot < 1 || slot > 9 {
		return
	}
	if p.Model == "" {
		p = QuickModelShortcut{}
	} else {
		m, err := provider.FindModel(p.Provider, p.Model)
		if err != nil {
			i.mu.Lock()
			i.statusErr, i.statusOK = err.Error(), ""
			i.mu.Unlock()
			i.invalidate()
			return
		}
		p.Provider, p.Model = m.Provider, m.ID
		p.Reasoning = provider.ClampReasoningForModel(m, p.Reasoning)
	}
	active := i.cfg.ActiveModelProfile
	if active == slot && p.Model != "" {
		i.activateModelProfile(slot, p)
		return
	}
	// Clearing the active favorite leaves the live model unchanged.
	if active == slot {
		active = 0
	}
	if !i.persistModelProfile(slot, p, active) {
		return
	}
	i.mu.Lock()
	i.statusErr = ""
	if p.Model == "" {
		i.statusOK = quickModelShortcutLabel(slot) + " cleared"
	} else {
		i.statusOK = quickModelShortcutLabel(slot) + " saved: " + modelProfileDescription(p)
	}
	i.mu.Unlock()
	i.invalidate()
}

func (i *Interactive) saveActiveModelProfile() {
	slot := i.cfg.ActiveModelProfile
	if slot < 1 || slot > 9 {
		return
	}
	p := QuickModelShortcut{
		Provider: i.cfg.Provider, Model: i.cfg.Model, Reasoning: i.cfg.Reasoning,
		FastMode: i.cfg.FastMode != nil && *i.cfg.FastMode,
	}
	if m, err := provider.FindModel(p.Provider, p.Model); err == nil {
		p.Reasoning = provider.ClampReasoningForModel(m, p.Reasoning)
		if p.Reasoning != i.cfg.Reasoning {
			i.setLiveReasoning(p.Reasoning)
		}
	}
	if i.persistModelProfile(slot, p, slot) {
		i.mu.Lock()
		i.statusOK = fmt.Sprintf("profile %d updated: %s", slot, modelProfileDescription(p))
		i.mu.Unlock()
	} else {
		i.detachUnsavedModelProfile()
	}
}

func (i *Interactive) detachUnsavedModelProfile() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.cfg.ActiveModelProfile = 0
	// The model/session callback already ran. Do not imply rollback:
	// only the favorite and active-slot save failed.
	i.statusErr = "model switched; " + i.statusErr
}

func (i *Interactive) activateModelProfile(slot int, p QuickModelShortcut) {
	defer i.invalidate()
	if slot < 1 || slot > 9 {
		return
	}
	if i.busy {
		i.mu.Lock()
		i.statusErr, i.statusOK = "cannot switch model while a turn is running", ""
		i.mu.Unlock()
		return
	}
	if p.Provider == "" || p.Model == "" {
		i.mu.Lock()
		i.statusErr, i.statusOK = "profile requires a provider and model; assign it in /settings", ""
		i.mu.Unlock()
		return
	}
	if i.cfg.SettingsStore != nil {
		if _, ok := i.cfg.SettingsStore.(modelProfileSettingsStore); !ok {
			if i.persistModelProfile(slot, p, 0) {
				i.swapModel(p.Provider, p.Model, i.cfg.BuildAgentFor, false)
			}
			return
		}
	}
	var replaced bool
	activate := func() {
		var success bool
		replaced, success = i.swapModelUnserialized(p.Provider, p.Model, i.cfg.BuildAgentFor, false)
		if !success {
			return
		}
		m, _ := provider.FindModel(i.cfg.Provider, i.cfg.Model) // swap validated the model
		p.Provider, p.Model = i.cfg.Provider, i.cfg.Model
		p.Reasoning = provider.ClampReasoningForModel(m, p.Reasoning)
		i.setLiveReasoning(p.Reasoning)
		i.setLiveFastMode(p.FastMode)
		if !i.persistModelProfile(slot, p, slot) {
			i.detachUnsavedModelProfile()
			return
		}
		i.mu.Lock()
		i.statusOK = fmt.Sprintf("profile %d active: %s", slot, modelProfileDescription(p))
		i.mu.Unlock()
	}
	if i.cfg.SessionTransition != nil {
		i.cfg.SessionTransition(activate)
	} else {
		activate()
	}
	if replaced {
		i.resetCompactHandoff()
	}
}

func (i *Interactive) setLiveReasoning(level string) {
	if i.cfg.OnReasoningChanged != nil {
		i.cfg.OnReasoningChanged(level)
	}
	i.mu.Lock()
	i.cfg.Reasoning = level
	if i.agent != nil {
		i.agent.Reasoning = level
	}
	i.mu.Unlock()
}

func (i *Interactive) setLiveFastMode(enabled bool) {
	// Agent replacement publishes under agentMu -> mu. Hold agentMu across
	// the snapshot and update so a replacement cannot appear with stale mode.
	i.agentMu.Lock()
	i.mu.Lock()
	value := enabled
	i.cfg.FastMode = &value
	agent := i.agent
	onChanged := i.cfg.OnFastModeChanged
	i.mu.Unlock()
	if agent != nil {
		agent.SetFastMode(enabled)
	}
	i.agentMu.Unlock()
	if onChanged != nil {
		onChanged(enabled)
	}
}
