package modes

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func newPinnedRecallInteractive(t *testing.T) *Interactive {
	t.Helper()
	return NewInteractive(InteractiveConfig{
		Agent: core.NewAgent(nil, "test-model", "", nil), CWD: t.TempDir(),
		LoadSkillPins: func(string) (skills.Pins, error) { return skills.Pins{Global: []string{"review"}}, nil },
		SkillSnapshot: reviewSnapshot, PreloadPinnedSkills: true,
	})
}

func TestPinnedQueuedDraftRecallKeepsEditorRawAndRearms(t *testing.T) {
	for name, key := range map[string]tui.Key{"alt-up": {Kind: tui.KeyUp, Alt: true}, "escape": {Kind: tui.KeyEsc}} {
		t.Run(name, func(t *testing.T) {
			i := newPinnedRecallInteractive(t)
			expanded := i.consumePinnedSkills("user draft")
			i.agent.QueuePrompt(core.QueuedMessage{Text: expanded})
			i.mu.Lock()
			i.busy = true
			i.cancelTurn = func() {}
			i.mu.Unlock()
			i.handleKey(context.Background(), key)
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
	i := newPinnedRecallInteractive(t)
	expanded := i.consumePinnedSkills("user draft")
	i.agent.SetMessages([]provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: expanded}}}})
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
	i := newPinnedRecallInteractive(t)
	expanded := i.consumePinnedSkills("user draft")
	i.agent.SetMessages([]provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: expanded}}}})
	if history := i.inputHistory(); len(history) != 1 || history[0] != "user draft" {
		t.Fatalf("input history contains injected bodies: %#v", history)
	}
	if got := i.skillPins.rawPrompt("Pinned skills for this conversation.\nuser-written text"); !strings.Contains(got, "user-written text") {
		t.Fatal("stripped unrelated user-written preamble")
	}
}
