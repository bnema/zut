package modes

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
	"github.com/stretchr/testify/mock"
)

func newPinnedRecallInteractive(t *testing.T) *Interactive {
	t.Helper()
	return NewInteractive(InteractiveConfig{
		Agent: core.NewAgent(nil, "test-model", "", nil), CWD: t.TempDir(),
		LoadSkillPins: func(string) (skills.Pins, error) { return skills.Pins{Global: []string{"review"}}, nil },
		SkillSnapshot: reviewSnapshot, PreloadPinnedSkills: true,
	})
}

// newPinnedRecallPortInteractive is newPinnedRecallInteractive on a session
// port. The pinned expansion of "user draft" reaches the transcript the way
// the daemon publishes it: as an EvUserMessage.
func newPinnedRecallPortInteractive(t *testing.T) (*Interactive, string) {
	t.Helper()
	i := newHistoryPortInteractive(t, nil, InteractiveConfig{
		CWD:           t.TempDir(),
		LoadSkillPins: func(string) (skills.Pins, error) { return skills.Pins{Global: []string{"review"}}, nil },
		SkillSnapshot: reviewSnapshot, PreloadPinnedSkills: true,
	})
	expanded := i.consumePinnedSkills("user draft")
	i.mu.Lock()
	i.applyPortAgentTranscriptLocked(core.EvUserMessage{Message: provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: expanded}}}})
	i.mu.Unlock()
	return i, expanded
}

func TestPinnedQueuedDraftRecallKeepsEditorRawAndRearms(t *testing.T) {
	for name, key := range map[string]tui.Key{"alt-up": {Kind: tui.KeyUp, Alt: true}, "escape": {Kind: tui.KeyEsc}} {
		t.Run(name, func(t *testing.T) {
			// The expansion is what the daemon queued and returns on slide-back.
			expanded := newPinnedRecallInteractive(t).consumePinnedSkills("user draft")
			port := sessionmocks.NewMockPort(t)
			port.EXPECT().SlideBackQueued(mock.Anything).Return(session.Input{Text: expanded}, nil).Once()
			aborted := make(chan struct{})
			if key.Kind == tui.KeyEsc {
				port.EXPECT().Abort(mock.Anything).RunAndReturn(func(context.Context) error { close(aborted); return nil }).Once()
			}
			i := newQueuePortInteractive(t, port, session.Snapshot{
				Info:  session.Info{Run: session.RunStateRunning},
				Queue: []session.QueuedInput{{Input: session.Input{Text: expanded}}},
			}, InteractiveConfig{
				CWD:           t.TempDir(),
				LoadSkillPins: func(string) (skills.Pins, error) { return skills.Pins{Global: []string{"review"}}, nil },
				SkillSnapshot: reviewSnapshot, PreloadPinnedSkills: true,
			})
			// The prompt was consumed locally before submission.
			if got := i.consumePinnedSkills("user draft"); got != expanded {
				t.Fatalf("fixture expansion differs: %q vs %q", got, expanded)
			}
			i.handleKey(context.Background(), key)
			runLoopCall(t, i)
			if key.Kind == tui.KeyEsc {
				waitSignal(t, aborted, "Abort")
			}
			if text := i.ed.Value(); text != "user draft" {
				t.Fatalf("recalled editor text=%q, want raw draft", text)
			}
			if next := i.consumePinnedSkills(i.ed.Value()); strings.Count(next, "Inspect carefully.") != 1 {
				t.Fatalf("recall lost or doubled pins: %q", next)
			}
		})
	}
}

func TestDiscardPinnedQueuedDraftRearmsNextUser(t *testing.T) {
	for _, preserveHost := range []bool{false, true} {
		i := newPinnedRecallInteractive(t)
		expanded := i.consumePinnedSkills("discarded draft")
		i.agent.QueuePrompt(core.QueuedMessage{Text: expanded})
		i.agent.QueuePrompt(core.QueuedMessage{Text: "host report", HostEvent: true})
		i.mu.Lock()
		i.discardQueuedMessagesLocked(preserveHost)
		i.mu.Unlock()
		if next := i.consumePinnedSkills("new draft"); strings.Count(next, "Inspect carefully.") != 1 || !strings.HasSuffix(next, "new draft") {
			t.Fatalf("discard lost pins: %q", next)
		}
	}
}

func TestCtrlCClearsPinnedQueueWithoutLosingPreload(t *testing.T) {
	i := newPinnedRecallInteractive(t)
	expanded := i.consumePinnedSkills("discarded draft")
	i.agent.QueuePrompt(core.QueuedMessage{Text: expanded})
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyCtrlC})
	if next := i.consumePinnedSkills("next draft"); strings.Count(next, "Inspect carefully.") != 1 {
		t.Fatalf("clearing queue lost pins: %q", next)
	}
}

func TestPinnedRecallAndHistorySynchronizeWithHostArming(t *testing.T) {
	i, expanded := newPinnedRecallPortInteractive(t)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 100 {
			i.SetPinnedSkillsPending(true)
		}
	}()
	for range 100 {
		i.restoreQueuedMessageToEditor(core.QueuedMessage{Text: expanded})
		i.inputHistory()
	}
	wg.Wait()
}

func TestPinnedInputHistoryShowsRawUserText(t *testing.T) {
	i, _ := newPinnedRecallPortInteractive(t)
	if history := i.inputHistory(); len(history) != 1 || history[0] != "user draft" {
		t.Fatalf("input history contains injected bodies: %#v", history)
	}
	if got := i.skillPins.rawPrompt("Pinned skills for this conversation.\nuser-written text"); !strings.Contains(got, "user-written text") {
		t.Fatal("stripped unrelated user-written preamble")
	}
}
