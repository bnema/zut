package modes

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func TestFinderUsesAgentSessionRoot(t *testing.T) {
	zutHome := t.TempDir()
	agentRoot := t.TempDir()
	cwd := t.TempDir()

	session, err := core.NewSession(agentRoot, cwd, "test", "test-model", "test-version")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "agent session"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := core.NewSession(zutHome, cwd, "test", "test-model", "test-version"); err != nil {
		t.Fatal(err)
	}

	i := NewInteractive(InteractiveConfig{
		Theme:        tui.Dark,
		Terminal:     &alertTestTerminal{},
		ZutHome:      zutHome,
		SessionsRoot: agentRoot,
		CWD:          cwd,
	})
	i.finderDelay, i.finderRetryDelay = 0, 0
	t.Cleanup(i.closeFinderIndex)
	t.Cleanup(i.closeFinder)
	i.runSlash(context.Background(), "/find")
	driveFinderLoop(t, i, func() bool { return !i.finder.indexing && !i.finder.loading && len(i.finder.items) > 0 })

	if got := len(i.finder.items); got != 1 {
		t.Fatalf("finder entries = %d, want 1 from agent session root", got)
	}
	if got := i.finder.items[0].Key; got != session.Path {
		t.Fatalf("finder path = %q, want %q", got, session.Path)
	}
}

func TestSessionsRootDefaultsToZutHome(t *testing.T) {
	i := &Interactive{cfg: InteractiveConfig{ZutHome: "/zut/home"}}
	if got := i.sessionsRoot(); got != "/zut/home" {
		t.Fatalf("sessions root = %q, want ZutHome fallback", got)
	}
}
