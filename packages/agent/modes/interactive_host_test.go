package modes

import (
	"context"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
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

func TestSubmitOrQueuePreservesImagesThroughPort(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	image := provider.ImageBlock{MimeType: "image/png", Data: []byte("png-1")}
	called := make(chan session.Input, 1)
	port.EXPECT().Submit(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in session.Input) error { called <- in; return nil }).Once()
	i := newQueuePortInteractive(t, port, session.Snapshot{Info: session.Info{Run: session.RunStateRunning}}, InteractiveConfig{})
	i.submitOrQueueMessage(core.QueuedMessage{Text: "inspect", Images: []provider.ImageBlock{image}, HostEvent: true}, false)
	select {
	case in := <-called:
		require.Equal(t, "inspect", in.Text)
		require.Equal(t, []provider.ImageBlock{image}, in.Images)
		require.True(t, in.HostEvent)
		require.Equal(t, session.InputOriginResident, in.Origin)
	case <-time.After(time.Second):
		t.Fatal("Submit was not called")
	}
}

func TestSlashPromptsSubmitThroughPort(t *testing.T) {
	for _, command := range []string{"/study", "/skill:review focus here"} {
		t.Run(command, func(t *testing.T) {
			port := sessionmocks.NewMockPort(t)
			called := make(chan session.Input, 1)
			port.EXPECT().Submit(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in session.Input) error { called <- in; return nil }).Once()
			i := newQueuePortInteractive(t, port, session.Snapshot{Info: session.Info{Run: session.RunStateRunning}}, InteractiveConfig{CWD: t.TempDir(), SkillSnapshot: reviewSnapshot})
			i.runSlash(context.Background(), command)
			select {
			case in := <-called:
				require.NotEmpty(t, in.Text)
				require.Equal(t, session.InputOriginUser, in.Origin)
				require.Equal(t, session.SubmitModeAuto, in.Mode)
				if command == "/study" {
					require.Equal(t, buildStudyPrompt("", i.cfg.CWD), in.Text)
				} else {
					require.Contains(t, in.Text, "Inspect carefully.")
					require.Contains(t, in.Text, "focus here")
				}
			case <-time.After(time.Second):
				t.Fatal("Submit was not called")
			}
		})
	}
}
