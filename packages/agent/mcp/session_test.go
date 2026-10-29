package mcp

import (
	"strings"
	"testing"
)

func TestPerSessionStdioEnvironment(t *testing.T) {
	t.Setenv("HOME", "host-home")
	got := buildStdioEnv(ServerConfig{BaseEnv: []string{"HOME=session-home", "SECRET=hidden"}, Env: map[string]string{"LOCAL": "value"}})
	joined := strings.Join(got, "|")
	if strings.Contains(joined, "host-home") || strings.Contains(joined, "SECRET") || !strings.Contains(joined, "HOME=session-home") {
		t.Fatalf("environment = %v", got)
	}
	empty := buildStdioEnv(ServerConfig{BaseEnv: []string{}})
	if len(empty) != 0 {
		t.Fatalf("empty session inherited environment: %v", empty)
	}
}
