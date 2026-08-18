package modes

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
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

	if len(i.sessionInfoBlocks) != 1 {
		t.Fatalf("/info blocks = %d, want 1", len(i.sessionInfoBlocks))
	}
	got := strings.Join(i.sessionInfoBlocks[0].lines, "\n")
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
	if got := len(agent.Messages()); got != 0 {
		t.Fatalf("/info added %d messages to provider context", got)
	}
}

func TestRunInfoAppendsEachInvocation(t *testing.T) {
	i := &Interactive{}

	i.runSlash(context.Background(), "/info")
	i.runSlash(context.Background(), "/info")

	if got := len(i.sessionInfoBlocks); got != 2 {
		t.Fatalf("/info blocks = %d, want one per invocation", got)
	}
}

func TestInfoScrollsWithTranscript(t *testing.T) {
	agent := &core.Agent{}
	agent.SetMessages([]provider.Message{{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: "before info"}},
	}})
	i := &Interactive{agent: agent, view: &tui.View{}}

	i.runSlash(context.Background(), "/info")
	agent.SetMessages(append(agent.Messages(), provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "after info"}},
	}))

	i.mu.Lock()
	got := strings.Join(i.buildChatLocked(80), "\n")
	i.mu.Unlock()
	before := strings.Index(got, "before info")
	info := strings.Index(got, "session info")
	after := strings.Index(got, "after info")
	if before < 0 || info < before || after < info {
		t.Fatalf("/info did not stay at its transcript position:\n%s", got)
	}
}

func TestRenderSessionInfoMarksUnavailableValues(t *testing.T) {
	got := strings.Join(renderSessionInfoBlock(tui.Theme{}, 80, "", "", "", "", ""), "\n")
	if count := strings.Count(got, "unavailable"); count != 5 {
		t.Fatalf("unavailable value count = %d, want 5:\n%s", count, got)
	}
}
