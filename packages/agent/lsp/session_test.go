package lsp

import (
	"strings"
	"testing"
)

func TestPerSessionEnvironment(t *testing.T) {
	t.Setenv("ZUT_L2_HOST_ONLY", "secret")
	got := mergedEnvironment(map[string]string{"LOCAL": "override"}, []string{"LOCAL=base", "PATH=/session/bin"})
	joined := strings.Join(got, "|")
	if strings.Contains(joined, "secret") || !strings.Contains(joined, "LOCAL=override") {
		t.Fatalf("environment = %v", got)
	}
	if got := zutHome("ZUT_HOME=/session/state"); got != "/session/state" {
		t.Fatalf("home = %q", got)
	}
}
