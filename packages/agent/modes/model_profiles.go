package modes

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
