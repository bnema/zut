package config

import (
	"fmt"
	"strings"
)

// Advisor modes accepted in config.
const (
	AdvisorModeAuto   = "auto"
	AdvisorModeClient = "client"
)

// AdvisorConfig is the optional persisted advisor block; nil means disabled.
type AdvisorConfig struct {
	Enabled   bool   `json:"enabled"`
	Model     string `json:"model,omitempty"`
	Mode      string `json:"mode,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
}

// AdvisorSettings are the effective advisor settings with defaults applied.
// Model is the raw "provider/model[:reasoning]" spec.
type AdvisorSettings struct {
	Enabled   bool
	Model     string
	Mode      string
	MaxTokens int // 0 means provider default
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
