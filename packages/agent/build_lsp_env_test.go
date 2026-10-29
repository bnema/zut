package agent

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/bnema/zut/packages/agent/tools"
)

func TestBuildToolRegistryPassesSessionEnvToLSP(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	t.Setenv("ZUT_HOME", t.TempDir())
	registry := buildToolRegistry(Args{Tools: []string{"lsp"}, Env: []string{"ZUT_HOME=" + home}}, cwd, nil, true, false, false)
	tool, ok := registry["lsp"].(*tools.LSPTool)
	if !ok || tool.Manager == nil {
		t.Fatalf("lsp tool = %T", registry["lsp"])
	}
	defer tool.Manager.Close()
	config, err := tool.Manager.Config(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "lsp.json"); !slices.Contains(config.Files, want) {
		t.Fatalf("config files %v missing session %s", config.Files, want)
	}
}
