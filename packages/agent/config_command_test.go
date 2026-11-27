package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/config"
	"github.com/bnema/zut/packages/provider/auth"
)

func TestAgentAPIKeyCommandHelperProcess(t *testing.T) {
	if os.Getenv("ZUT_AGENT_API_KEY_COMMAND_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 2 {
		os.Exit(2)
	}
	if err := os.WriteFile(args[1], []byte("ran\n"), 0o600); err != nil {
		os.Exit(3)
	}
	fmt.Print("resolved-secret\n")
	os.Exit(0)
}

func TestCommandCredentialIsLazy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZUT_HOME", home)
	t.Setenv("LAZY_PROVIDER_API_KEY", "")
	t.Setenv("ZUT_AGENT_API_KEY_COMMAND_HELPER", "1")
	marker := filepath.Join(home, "command-ran")
	credentials := auth.Credentials{AdditionalAPIKeyCreds: map[string]auth.ProviderCreds{
		"lazy-provider": {
			APIKeyCommand: &auth.APIKeyCommand{
				Program: os.Args[0],
				Args:    []string{"-test.run=^TestAgentAPIKeyCommandHelperProcess$", "--", marker},
			},
		},
	}}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.AuthPath(), encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	if !CredentialAvailable("lazy-provider") {
		t.Fatal("command-backed credential was not reported as available")
	}
	if _, _, err := resolveCredentialForBackground(context.Background(), "lazy-provider"); err == nil {
		t.Fatal("background resolution unexpectedly returned the command-backed credential")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("credential command ran during availability checks: %v", err)
	}

	cred, method, _, err := ResolveCredentialFull("lazy-provider", "")
	if err != nil {
		t.Fatal(err)
	}
	if cred != "resolved-secret" || method != "apikey" {
		t.Fatalf("resolved (%q, %q), want command API key", cred, method)
	}
	if contents, err := os.ReadFile(marker); err != nil || !strings.Contains(string(contents), "ran") {
		t.Fatalf("credential command marker = %q, %v", contents, err)
	}
}
