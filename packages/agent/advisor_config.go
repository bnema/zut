package agent

import (
	"fmt"
	"strings"

	providerpkg "github.com/bnema/zut/packages/provider"
)

// Advisor modes accepted in config.
const (
	AdvisorModeAuto   = "auto"
	AdvisorModeClient = "client"
)

// advisorSpecFormat is shown in parse errors.
const advisorSpecFormat = "provider/model:reasoning"

// defaultAdvisorReasoning applies when a spec omits the reasoning suffix.
const defaultAdvisorReasoning = "high"

// AdvisorConfig is the optional persisted advisor block; nil means disabled.
type AdvisorConfig struct {
	Enabled   bool   `json:"enabled"`
	Model     string `json:"model,omitempty"`
	Mode      string `json:"mode,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
}

// AdvisorSettings are the effective advisor settings with defaults applied.
// Model is the raw spec; parse it with ParseAdvisorModelSpec.
type AdvisorSettings struct {
	Enabled   bool
	Model     string
	Mode      string
	MaxTokens int // 0 means provider default
}

// AdvisorModelSpec is a parsed "provider/model[:reasoning]" value.
type AdvisorModelSpec struct {
	Provider  string
	Model     string
	Reasoning string // clamped to the model; empty when unsupported or off
	// canonical is the normalized spec, computed at parse time.
	canonical string
}

// String renders the canonical "provider/model[:reasoning]" form. Reasoning
// that is off on a reasoning-capable model is written as ":off" so it does not
// re-parse as the "high" default.
func (s AdvisorModelSpec) String() string { return s.canonical }

// ParseAdvisorModelSpec parses "provider/model[:reasoning]" against the model
// catalog. The whole remainder is tried as a model ID first because IDs may
// contain ":"; otherwise the last ":suffix" is peeled only when it is a known
// reasoning level. Omitted reasoning defaults to "high", then is clamped to
// the levels the model supports.
func ParseAdvisorModelSpec(spec string) (AdvisorModelSpec, error) {
	spec = strings.TrimSpace(spec)
	prov, rest, ok := strings.Cut(spec, "/")
	prov, rest = strings.TrimSpace(prov), strings.TrimSpace(rest)
	if !ok || prov == "" || rest == "" {
		return AdvisorModelSpec{}, fmt.Errorf("invalid advisor model %q: expected %s", spec, advisorSpecFormat)
	}
	prov = canonicalProvider(prov)
	reasoning := defaultAdvisorReasoning
	model, err := providerpkg.FindModel(prov, rest)
	if err != nil {
		id, suffix, hasSuffix := cutLast(rest, ":")
		if !hasSuffix {
			return AdvisorModelSpec{}, fmt.Errorf("unknown advisor model %q for provider %q: expected %s", rest, prov, advisorSpecFormat)
		}
		if strings.TrimSpace(suffix) == "" {
			return AdvisorModelSpec{}, fmt.Errorf("empty reasoning in advisor model %q: expected %s", spec, advisorSpecFormat)
		}
		level := providerpkg.NormalizeReasoning(suffix)
		if !providerpkg.IsReasoningLevel(level) {
			return AdvisorModelSpec{}, fmt.Errorf("invalid reasoning %q in advisor model %q: expected %s", suffix, spec, advisorSpecFormat)
		}
		model, err = providerpkg.FindModel(prov, id)
		if err != nil {
			return AdvisorModelSpec{}, fmt.Errorf("unknown advisor model %q for provider %q: expected %s", id, prov, advisorSpecFormat)
		}
		reasoning = level
	}
	out := AdvisorModelSpec{
		Provider:  model.Provider,
		Model:     model.ID,
		Reasoning: providerpkg.ClampReasoningForModel(model, reasoning),
	}
	out.canonical = out.Provider + "/" + out.Model
	switch {
	case out.Reasoning != "":
		out.canonical += ":" + out.Reasoning
	case model.Reasoning:
		out.canonical += ":off"
	}
	return out, nil
}

func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

func validateAdvisorConfig(a *AdvisorConfig) error {
	if a == nil {
		return nil
	}
	switch strings.TrimSpace(a.Mode) {
	case "", AdvisorModeAuto, AdvisorModeClient:
	default:
		return fmt.Errorf("parse config: advisor.mode %q: expected %q or %q", a.Mode, AdvisorModeAuto, AdvisorModeClient)
	}
	if a.MaxTokens < 0 {
		return fmt.Errorf("parse config: advisor.max_tokens must not be negative")
	}
	return nil
}

// AdvisorSettings returns the effective advisor settings. A missing block is
// disabled; an empty mode means auto.
func (c Config) AdvisorSettings() AdvisorSettings {
	if c.Advisor == nil {
		return AdvisorSettings{Mode: AdvisorModeAuto}
	}
	mode := strings.TrimSpace(c.Advisor.Mode)
	if mode != AdvisorModeClient {
		mode = AdvisorModeAuto
	}
	maxTokens := c.Advisor.MaxTokens
	if maxTokens < 0 {
		maxTokens = 0
	}
	return AdvisorSettings{
		Enabled:   c.Advisor.Enabled,
		Model:     strings.TrimSpace(c.Advisor.Model),
		Mode:      mode,
		MaxTokens: maxTokens,
	}
}
