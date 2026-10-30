package modes

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/agent/skills"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestSkillsPickerPinKeysUseHostCallbacksAndDedupeWarnings(t *testing.T) {
	var toggled []string
	i := NewInteractive(InteractiveConfig{
		CWD:           "/work",
		SkillSnapshot: reviewSnapshot,
		LoadSkillPins: func(string) (skills.Pins, error) {
			return skills.Pins{Global: []string{"gone"}}, nil
		},
		ToggleSkillPin: func(cwd, name string, global bool) error {
			toggled = append(toggled, cwd+":"+name)
			return nil
		},
		PreloadPinnedSkills: true,
	})
	i.openSkillsDialog()
	i.openSkillsDialog()
	i.handleKey(context.Background(), pinKey('p'))
	if len(toggled) != 1 || toggled[0] != "/work:review" {
		t.Fatalf("toggle calls = %v", toggled)
	}
	i.mu.Lock()
	warnings := 0
	for _, e := range i.reloadErrors {
		if strings.Contains(e, "gone") {
			warnings++
		}
	}
	notice := i.statusOK
	i.mu.Unlock()
	if warnings != 1 || notice == "" {
		t.Fatalf("warnings=%d status=%q reloadErrors=%q", warnings, notice, i.reloadErrors)
	}
}

// Pin selection and its notice are client presentation state; persistence and
// provider delivery belong to the session, not to a local turn executor.
func TestPinnedSkillsJoinFirstPromptOnceThroughPort(t *testing.T) {
	port := sessionmocks.NewMockPort(t)
	called := make(chan session.Input, 2)
	port.EXPECT().Submit(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in session.Input) error { called <- in; return nil }).Twice()
	i := newQueuePortInteractive(t, port, session.Snapshot{}, InteractiveConfig{CWD: t.TempDir(), SkillSnapshot: reviewSnapshot, LoadSkillPins: func(string) (skills.Pins, error) { return skills.Pins{Global: []string{"review"}}, nil }, PreloadPinnedSkills: true})
	for _, text := range []string{"first request", "second request"} {
		i.Submit(i.consumePinnedSkills(text))
		select {
		case in := <-called:
			if text == "first request" {
				require.Equal(t, 1, strings.Count(in.Text, "Inspect carefully."))
				require.True(t, strings.HasSuffix(in.Text, text))
			} else {
				require.Equal(t, text, in.Text)
			}
		case <-time.After(time.Second):
			t.Fatal("Submit was not called")
		}
	}
	i.mu.Lock()
	notice := i.skillPins.notice(i.cfg.Theme, 80)
	i.mu.Unlock()
	require.Empty(t, notice)
}
