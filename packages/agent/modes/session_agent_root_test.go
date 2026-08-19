package modes

import (
	"context"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestSessionsSlashUsesAgentSessionRoot(t *testing.T) {
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

	i := NewInteractive(InteractiveConfig{
		ZutHome:      zutHome,
		SessionsRoot: agentRoot,
		CWD:          cwd,
	})
	i.runSlash(context.Background(), "/sessions")
	defer i.sessionDialog.Close()
	for event := range i.sessionLoads {
		i.sessionDialog.ApplyLoad(event)
	}

	if got := len(i.sessionDialog.sessions); got != 1 {
		t.Fatalf("session picker entries = %d, want 1 from agent session root", got)
	}
	if got := i.sessionDialog.sessions[0].Path; got != session.Path {
		t.Fatalf("session picker path = %q, want %q", got, session.Path)
	}
}

func TestSessionsSlashRejectsNoSessionMode(t *testing.T) {
	i := NewInteractive(InteractiveConfig{SessionsDisabled: true})
	i.runSlash(context.Background(), "/sessions")
	if i.sessionDialog.Active() {
		t.Fatal("session picker opened while sessions were disabled")
	}
	if i.statusErr != "sessions are disabled by --no-session" {
		t.Fatalf("status error = %q", i.statusErr)
	}
}

func TestSessionsRootDefaultsToZutHome(t *testing.T) {
	i := &Interactive{cfg: InteractiveConfig{ZutHome: "/zut/home"}}
	if got := i.sessionsRoot(); got != "/zut/home" {
		t.Fatalf("sessions root = %q, want ZutHome fallback", got)
	}
}
