package agent

import (
	"encoding/json"
	"testing"
)

func TestCodemodeConfigDefaultsAndRoundTrip(t *testing.T) {
	pythonTestEnv(t)
	for _, tc := range []struct {
		raw               string
		enabled, deferMCP bool
	}{
		{`{}`, false, false},
		{`{"experimental":{}}`, false, false},
		{`{"experimental":{"codemode":false,"codemode_defer_mcp":true}}`, false, false},
		{`{"experimental":{"codemode":true}}`, true, true},
		{`{"experimental":{"codemode":true,"codemode_defer_mcp":true}}`, true, true},
		{`{"experimental":{"codemode":true,"codemode_defer_mcp":false}}`, true, false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			var cfg Config
			if err := json.Unmarshal([]byte(tc.raw), &cfg); err != nil {
				t.Fatal(err)
			}
			if err := SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if loaded.CodemodeEnabled() != tc.enabled || loaded.CodemodeDeferMCP() != tc.deferMCP {
				t.Fatalf("enabled=%v defer=%v", loaded.CodemodeEnabled(), loaded.CodemodeDeferMCP())
			}
			raw, err := json.Marshal(loaded)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if tc.raw == `{}` && fields["experimental"] != nil {
				t.Fatalf("default experimental persisted: %s", raw)
			}
		})
	}
}
