package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigLSPEnabledForDefaultsToTrue(t *testing.T) {
	cfg := Config{}
	if !cfg.LSPEnabledFor(false) {
		t.Fatal("main-session LSP should default to enabled")
	}
	if !cfg.LSPEnabledFor(true) {
		t.Fatal("sub-agent LSP should default to enabled")
	}

	noMain, noSub := false, false
	cfg.LSPEnabled = &noMain
	cfg.SubagentLSPEnabled = &noSub
	if cfg.LSPEnabledFor(false) || cfg.LSPEnabledFor(true) {
		t.Fatal("explicitly disabled LSP settings were ignored")
	}
	yesSub := true
	cfg.SubagentLSPEnabled = &yesSub
	write, edit := true, true
	cfg.LSPDiagnosticsOnWrite = &write
	cfg.LSPDiagnosticsOnEdit = &edit
	if cfg.LSPDiagnosticsOnWriteEnabled(false) || cfg.LSPDiagnosticsOnEditEnabled(false) {
		t.Fatal("diagnostics remained enabled with main-session LSP disabled")
	}
	if !cfg.LSPDiagnosticsOnWriteEnabled(true) || !cfg.LSPDiagnosticsOnEditEnabled(true) {
		t.Fatal("sub-agent diagnostics did not follow sub-agent LSP settings")
	}
}

func TestConfigWebSearchEnabledForCLIDefaultsToTrue(t *testing.T) {
	cfg := Config{}
	if !cfg.WebSearchEnabledForCLI() {
		t.Fatal("web search should default to enabled when the config field is absent")
	}
	disabled := false
	cfg.WebSearchEnabled = &disabled
	if cfg.WebSearchEnabledForCLI() {
		t.Fatal("explicitly disabled web search setting was ignored")
	}
}

func TestFastModeConfigJSONUsesSnakeCase(t *testing.T) {
	enabled := true
	data, err := json.Marshal(Config{FastMode: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"fast_mode":true`) {
		t.Fatalf("config JSON = %s, want fast_mode", data)
	}
}
