package modes

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestRunInfoShowsCurrentSessionDetails(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(root, "state", "session.jsonl")
	cwd := filepath.Join(root, "workspace")
	// No Submit expectation: /info must not reach the session.
	i := newSessionInfoPortInteractive(t, session.Info{
		ID:          "session-123",
		SessionPath: sessionPath,
		CWD:         cwd,
		Provider:    "openai-codex",
		Model:       "gpt-5.6",
	}, nil)

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
	i.mu.Lock()
	transcript := len(i.portMsgs)
	i.mu.Unlock()
	if transcript != 0 {
		t.Fatalf("/info added %d messages to the session transcript", transcript)
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
	i := newSessionInfoPortInteractive(t, session.Info{ID: "info"}, []provider.Message{{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: "before info"}},
	}})

	i.runSlash(context.Background(), "/info")
	i.mu.Lock()
	i.applyPortAgentTranscriptLocked(core.EvUserMessage{Message: provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "after info"}},
	}})
	got := strings.Join(i.buildChatLocked(80), "\n")
	i.mu.Unlock()
	before := strings.Index(got, "before info")
	info := strings.Index(got, "session info")
	after := strings.Index(got, "after info")
	if before < 0 || info < before || after < info {
		t.Fatalf("/info did not stay at its transcript position:\n%s", got)
	}
}

// newSessionInfoPortInteractive attaches to a mocked session whose snapshot
// carries info and msgs.
func newSessionInfoPortInteractive(t *testing.T, info session.Info, msgs []provider.Message) *Interactive {
	t.Helper()
	info.Run = session.RunStateIdle
	port := sessionmocks.NewMockPort(t)
	port.EXPECT().Snapshot(mock.Anything).Return(session.Snapshot{Info: info, Seq: 1, Messages: msgs}, nil).Once()
	port.EXPECT().Subscribe(mock.Anything, uint64(1)).Return(make(chan session.Event), nil).Once()
	i := NewInteractive(InteractiveConfig{Port: port})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, i.attachPort(ctx))
	return i
}

func TestRenderSessionInfoMarksUnavailableValues(t *testing.T) {
	got := strings.Join(renderSessionInfoBlock(tui.Theme{}, 80, "", "", "", "", ""), "\n")
	if count := strings.Count(got, "unavailable"); count != 5 {
		t.Fatalf("unavailable value count = %d, want 5:\n%s", count, got)
	}
}
