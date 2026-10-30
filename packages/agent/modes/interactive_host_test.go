package modes

import (
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

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
