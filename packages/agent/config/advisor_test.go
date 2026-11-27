package config

import (
	"strings"
	"testing"
)

func TestAdvisorConfigDefaultsAndValidation(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	got := (Config{}).AdvisorSettings()
	if got.Enabled || got.Model != "" || got.Mode != AdvisorModeAuto || got.MaxTokens != 0 {
		t.Fatalf("default = %+v", got)
	}
	cfg := Config{Advisor: &AdvisorConfig{Enabled: true, Model: "anthropic/claude-opus-4-7", Mode: "client", MaxTokens: 2048}}
	if s := cfg.AdvisorSettings(); !s.Enabled || s.Mode != AdvisorModeClient || s.MaxTokens != 2048 {
		t.Fatalf("settings = %+v", s)
	}
	for _, bad := range []*AdvisorConfig{{Mode: "native"}, {MaxTokens: -1}} {
		if err := Save(Config{Advisor: bad}); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "advisor.") {
			t.Fatalf("%+v: err = %v", bad, err)
		}
	}
}
