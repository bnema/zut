package modes

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/tui"
)

func TestRunInfoShowsCurrentSessionDetails(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(root, "state", "session.jsonl")
	cwd := filepath.Join(root, "workspace")
	agent := &core.Agent{}
	if err := agent.BindSessionID("session-123"); err != nil {
		t.Fatal(err)
	}
	i := &Interactive{
		agent: agent,
		cfg: InteractiveConfig{
			CWD:                cwd,
			Provider:           "openai-codex",
			Model:              "gpt-5.6",
			CurrentSessionPath: func() string { return sessionPath },
		},
	}

	i.runSlash(context.Background(), "/INFO")

	got := strings.Join(i.sessionInfoBlock, "\n")
	for _, want := range []string{
		"session info",
		"session-123",
		sessionPath,
		cwd,
		"openai-codex",
		"gpt-5.6",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("/info output missing %q:\n%s", want, got)
		}
	}
	if len(i.helpBlock) != 0 {
		t.Fatalf("/info retained help block: %#v", i.helpBlock)
	}
}

func TestRenderSessionInfoMarksUnavailableValues(t *testing.T) {
	got := strings.Join(renderSessionInfoBlock(tui.Theme{}, 80, "", "", "", "", ""), "\n")
	if count := strings.Count(got, "unavailable"); count != 5 {
		t.Fatalf("unavailable value count = %d, want 5:\n%s", count, got)
	}
}
