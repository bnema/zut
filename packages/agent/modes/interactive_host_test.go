package modes

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestApplySessionAgentWithCWDRebindsInteractiveWorkspace(t *testing.T) {
	oldCWD := t.TempDir()
	newCWD := filepath.Join(t.TempDir(), "selected-workspace")
	if err := os.MkdirAll(newCWD, 0o755); err != nil {
		t.Fatal(err)
	}
	current := core.NewAgent(nil, "model", "", nil)
	selected := core.NewAgent(nil, "model", "", nil)
	selected.SetMessages([]provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "selected"}}}})
	i := NewInteractive(InteractiveConfig{Agent: current, CWD: oldCWD, StartupContextPaths: []string{filepath.Join(oldCWD, "AGENTS.md")}})
	i.view.StartupContextPaths = []string{filepath.Join(oldCWD, "AGENTS.md")}
	i.ApplySessionAgentWithCWD(selected, "provider", "model", newCWD, nil)
	if i.cfg.CWD != newCWD || i.agent != selected {
		t.Fatalf("interactive workspace = %q agent=%p, want cwd=%q agent=%p", i.cfg.CWD, i.agent, newCWD, selected)
	}
	if len(i.cfg.StartupContextPaths) != 0 || len(i.view.StartupContextPaths) != 0 {
		t.Fatalf("old workspace paths remained: cfg=%q view=%q", i.cfg.StartupContextPaths, i.view.StartupContextPaths)
	}
	text, _ := i.view.Messages[0].Content[0].(provider.TextBlock)
	if text.Text != "selected" {
		t.Fatalf("resumed view text = %q", text.Text)
	}
}

func TestCanResumeSessionSelectionRejectsBusyTurn(t *testing.T) {
	i := newQueuePortInteractive(t, sessionmocks.NewMockPort(t), session.Snapshot{Info: session.Info{Run: session.RunStateIdle}}, InteractiveConfig{})
	if !i.canResumeSessionSelection() {
		t.Fatal("idle session rejected selection")
	}
	i.mu.Lock()
	i.applyInfoLocked(session.Info{Run: session.RunStateRunning})
	i.mu.Unlock()
	if i.canResumeSessionSelection() {
		t.Fatal("busy turn allowed session selection")
	}
}

func TestSubmitOrQueuePreservesImagesInHostQueue(t *testing.T) {
	i := &Interactive{agent: &core.Agent{}, busy: true, compacting: true}
	image := provider.ImageBlock{MimeType: "image/png", Data: []byte("png-1")}

	i.submitOrQueue("inspect", []provider.ImageBlock{image}, false)

	if len(i.queued) != 1 || i.queued[0].Text != "inspect" {
		t.Fatalf("queued messages = %#v, want one inspect prompt", i.queued)
	}
	if len(i.queued[0].Images) != 1 || string(i.queued[0].Images[0].Data) != "png-1" {
		t.Fatalf("queued images = %#v, want png-1", i.queued[0].Images)
	}
}
