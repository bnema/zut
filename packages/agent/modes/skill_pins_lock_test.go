package modes

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/tui"
)

// Synchronous helpers composing the prefetch and publish steps for state-level
// tests that have no Interactive mutex to release.

func (s *skillPinState) arm(pending bool, cwd string, d *skillsDialog) []string {
	if !pending {
		s.disarm()
		return nil
	}
	return s.armFetched(s.fetch(cwd, true), d)
}

func (s *skillPinState) refresh(cwd string, d *skillsDialog) []string {
	m := s.mark(cwd)
	return s.applyRefresh(m, cwd, s.fetch(cwd, true), d)
}

func (s *skillPinState) handleKey(k tui.Key, cwd string, d *skillsDialog) (handled bool, statusOK, statusErr string, warnings []string) {
	req, ok := s.toggleRequest(k, cwd, d)
	if !ok {
		return false, "", "", nil
	}
	m := s.mark(cwd)
	f, err := s.runToggle(req)
	statusOK, statusErr, warnings = s.completeToggle(m, cwd, f, err, d)
	return true, statusOK, statusErr, warnings
}

// mutexProbe fails the test when a callback runs while i.mu is held.
type mutexProbe struct {
	t     *testing.T
	i     *Interactive
	mu    sync.Mutex
	calls map[string]int
}

func newMutexProbe(t *testing.T, i *Interactive) *mutexProbe {
	return &mutexProbe{t: t, i: i, calls: map[string]int{}}
}

func (p *mutexProbe) hit(name string) {
	p.t.Helper()
	if !p.i.mu.TryLock() {
		p.t.Errorf("%s ran while i.mu was held", name)
	} else {
		p.i.mu.Unlock()
	}
	p.mu.Lock()
	p.calls[name]++
	p.mu.Unlock()
}

func (p *mutexProbe) count(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[name]
}

func (p *mutexProbe) reset() {
	p.mu.Lock()
	p.calls = map[string]int{}
	p.mu.Unlock()
}

func newProbedInteractive(t *testing.T, preload bool) (*Interactive, *mutexProbe) {
	t.Helper()
	var i *Interactive
	var probe *mutexProbe
	pins := skills.Pins{Global: []string{"review"}}
	i = NewInteractive(InteractiveConfig{
		Agent: core.NewAgent(nil, "test-model", "", nil), CWD: t.TempDir(),
		LoadSkillPins: func(string) (skills.Pins, error) {
			if probe != nil {
				probe.hit("load")
			}
			return pins, nil
		},
		ToggleSkillPin: func(_, name string, _ bool) error {
			probe.hit("toggle")
			pins = skills.Pins{Global: []string{name}}
			return nil
		},
		SkillSnapshot: func() []*skills.Skill {
			if probe != nil {
				probe.hit("snapshot")
			}
			return reviewSnapshot()
		},
		PreloadPinnedSkills: preload,
	})
	probe = newMutexProbe(t, i)
	return i, probe
}

func TestSkillPinCallbacksRunOutsideInteractiveMutex(t *testing.T) {
	t.Run("startup arm", func(t *testing.T) {
		i, p := newProbedInteractive(t, false)
		i.SetPinnedSkillsPending(true)
		if p.count("load") != 1 || p.count("snapshot") != 1 {
			t.Fatalf("arm callbacks: %v", p.calls)
		}
		if !strings.Contains(i.consumePinnedSkills("x"), "Inspect carefully.") {
			t.Fatal("arm lost selection")
		}
	})
	t.Run("clear", func(t *testing.T) {
		i, p := newProbedInteractive(t, true)
		i.consumePinnedSkills("first")
		p.reset()
		i.runSlash(context.Background(), "/clear")
		if p.count("load") != 1 || p.count("snapshot") != 1 {
			t.Fatalf("clear callbacks: %v", p.calls)
		}
		if !strings.Contains(i.consumePinnedSkills("x"), "Inspect carefully.") {
			t.Fatal("/clear did not re-arm")
		}
	})
	t.Run("cd", func(t *testing.T) {
		i, p := newProbedInteractive(t, true)
		i.consumePinnedSkills("first")
		p.reset()
		i.ApplyChangedCWD(core.NewAgent(nil, "test-model", "", nil), "p", "m", t.TempDir())
		if p.count("load") != 1 || p.count("snapshot") != 1 {
			t.Fatalf("cd callbacks: %v", p.calls)
		}
		if !strings.Contains(i.consumePinnedSkills("x"), "Inspect carefully.") {
			t.Fatal("/cd did not re-arm")
		}
	})
	t.Run("skills dialog and toggle", func(t *testing.T) {
		i, p := newProbedInteractive(t, true)
		p.reset()
		i.openSkillsDialog()
		if p.count("snapshot") != 1 || p.count("load") != 1 {
			t.Fatalf("/skills must walk discovery once: %v", p.calls)
		}
		p.reset()
		if !i.handleSkillPinKey(pinKey('p')) {
			t.Fatal("pin key not handled")
		}
		if p.count("toggle") != 1 || p.count("load") != 1 || p.count("snapshot") != 0 {
			t.Fatalf("toggle callbacks: %v", p.calls)
		}
		i.mu.Lock()
		ok := i.statusOK
		i.mu.Unlock()
		if ok == "" {
			t.Fatal("toggle status missing")
		}
	})
	t.Run("startup pre", func(t *testing.T) {
		i, p := newProbedInteractive(t, true)
		p.reset()
		i.applyStartupPreResult(startupPreResult{})
		if p.count("load") != 1 || p.count("snapshot") != 1 {
			t.Fatalf("startup callbacks: %v", p.calls)
		}
		i.consumePinnedSkills("first")
		p.reset()
		i.applyStartupPreResult(startupPreResult{})
		if len(p.calls) != 0 {
			t.Fatalf("consumed pins were refreshed: %v", p.calls)
		}
	})
	t.Run("disarm", func(t *testing.T) {
		i, p := newProbedInteractive(t, true)
		p.reset()
		i.SetPinnedSkillsPending(false)
		i.ApplySessionAgent(core.NewAgent(nil, "test-model", "", nil), "p", "m")
		if len(p.calls) != 0 {
			t.Fatalf("disarm did IO: %v", p.calls)
		}
	})
}

func TestSkillPinRecallAndDiscardDoNoIOAndRestoreExactSelection(t *testing.T) {
	i, p := newProbedInteractive(t, true)
	expanded := i.consumePinnedSkills("draft")
	p.reset()
	// Preferences and discovery change after consumption; recall must still
	// restore exactly what was consumed, without reading either again.
	i.mu.Lock()
	i.skillPins.load = func(string) (skills.Pins, error) { t.Error("recall loaded pins"); return skills.Pins{}, nil }
	i.skillPins.snapshot = func() []*skills.Skill { t.Error("recall walked discovery"); return nil }
	i.mu.Unlock()

	if !i.restoreQueuedMessageToEditor(core.QueuedMessage{Text: expanded}) {
		t.Fatal("restore failed")
	}
	if i.ed.Value() != "draft" {
		t.Fatalf("editor = %q", i.ed.Value())
	}
	i.mu.Lock()
	notice := stripANSIBytes(strings.Join(i.skillPins.notice(tui.Theme{}, 80), ""))
	i.mu.Unlock()
	if !strings.Contains(notice, "review (global)") {
		t.Fatalf("scope marker lost: %q", notice)
	}
	if next := i.consumePinnedSkills("draft"); next != expanded {
		t.Fatalf("recalled selection differs:\n%q\n%q", next, expanded)
	}

	i.agent.QueuePrompt(core.QueuedMessage{Text: expanded})
	i.mu.Lock()
	i.discardQueuedMessagesLocked(false)
	i.mu.Unlock()
	if next := i.consumePinnedSkills("again"); strings.Count(next, "Inspect carefully.") != 1 {
		t.Fatalf("discard lost pins: %q", next)
	}
	if len(p.calls) != 0 {
		t.Fatalf("recall did IO: %v", p.calls)
	}
}

func TestSkillPinNoPendingWithEmptySelectionPublished(t *testing.T) {
	// A blocked prefetch must not expose pending with a stale/empty selection.
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	i := NewInteractive(InteractiveConfig{
		Agent: core.NewAgent(nil, "test-model", "", nil), CWD: t.TempDir(),
		LoadSkillPins: func(string) (skills.Pins, error) { return skills.Pins{Global: []string{"review"}}, nil },
		SkillSnapshot: func() []*skills.Skill {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return reviewSnapshot()
		},
	})
	done := make(chan struct{})
	go func() { i.SetPinnedSkillsPending(true); close(done) }()
	<-entered
	i.mu.Lock()
	pending, selected := i.skillPins.pending, len(i.skillPins.selected)
	i.mu.Unlock()
	if pending || selected != 0 {
		t.Fatalf("published early: pending=%v selected=%d", pending, selected)
	}
	close(release)
	<-done
	i.mu.Lock()
	pending, selected = i.skillPins.pending, len(i.skillPins.selected)
	i.mu.Unlock()
	if !pending || selected != 1 {
		t.Fatalf("pending=%v selected=%d", pending, selected)
	}
}

// blockingSnapshot lets a test hold one discovery walk open.
type blockingSnapshot struct {
	entered chan struct{}
	release chan struct{}
	block   bool
	empty   bool // discovery finds nothing once released
}

func (b *blockingSnapshot) snapshot() []*skills.Skill {
	if b.block {
		b.block = false
		b.entered <- struct{}{}
		<-b.release
		if b.empty {
			return nil
		}
	}
	return reviewSnapshot()
}

func TestStaleSkillPinArmDroppedAfterDisarmOrSessionReplacement(t *testing.T) {
	for name, overtake := range map[string]func(*Interactive){
		"disarm":              func(i *Interactive) { i.SetPinnedSkillsPending(false) },
		"session replacement": func(i *Interactive) { i.ApplySessionAgent(core.NewAgent(nil, "test-model", "", nil), "p", "m") },
	} {
		t.Run(name, func(t *testing.T) {
			b := &blockingSnapshot{entered: make(chan struct{}, 1), release: make(chan struct{}), block: true}
			i := NewInteractive(InteractiveConfig{
				Agent: core.NewAgent(nil, "test-model", "", nil), CWD: t.TempDir(),
				LoadSkillPins: func(string) (skills.Pins, error) { return skills.Pins{Global: []string{"review"}}, nil },
				SkillSnapshot: b.snapshot,
			})
			done := make(chan struct{})
			go func() { i.SetPinnedSkillsPending(true); close(done) }()
			<-b.entered
			// The overtaking transition must not wait for the blocked callback.
			finished := make(chan struct{})
			go func() { overtake(i); close(finished) }()
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("transition blocked behind in-flight prefetch")
			}
			close(b.release)
			<-done
			if got := i.consumePinnedSkills("hello"); got != "hello" {
				t.Fatalf("stale arm published pins: %q", got)
			}
		})
	}
}

func TestStaleSkillPinRefreshDroppedAfterConsumeAndRecall(t *testing.T) {
	b := &blockingSnapshot{entered: make(chan struct{}, 1), release: make(chan struct{})}
	i := NewInteractive(InteractiveConfig{
		Agent: core.NewAgent(nil, "test-model", "", nil), CWD: t.TempDir(),
		LoadSkillPins: func(string) (skills.Pins, error) { return skills.Pins{Global: []string{"review"}}, nil },
		SkillSnapshot: b.snapshot, PreloadPinnedSkills: true,
	})
	// Startup refresh blocked in discovery while the user submits, then the
	// queued prompt is recalled. The exactly restored selection must survive
	// the (now stale) refresh, which would otherwise resolve to nothing.
	b.block = true
	done := make(chan struct{})
	go func() { i.refreshPendingSkillPins(); close(done) }()
	<-b.entered
	expanded := i.consumePinnedSkills("draft")
	i.mu.Lock()
	i.skillPins.recall(expanded)
	i.mu.Unlock()
	// The blocked discovery finds nothing: a published stale result would
	// empty the restored selection.
	b.empty = true
	close(b.release)
	<-done
	if got := i.consumePinnedSkills("draft"); got != expanded {
		t.Fatalf("stale refresh replaced restored selection: %q", got)
	}
}

func TestStaleSkillPinPrefetchDroppedAfterCWDOrGenerationChange(t *testing.T) {
	load := func(string) (skills.Pins, error) { return skills.Pins{Global: []string{"review"}}, nil }
	s := newSkillPinState(load, nil, reviewSnapshot)
	s.armFetched(s.fetch("/a", true), nil)
	before := s.selected

	mark := s.mark("/a")
	stale := skillPinFetch{pins: skills.Pins{Global: []string{"gone"}}}
	if w := s.applyRefresh(mark, "/b", stale, nil); w != nil || s.pins.Global[0] != "review" || len(s.selected) != len(before) {
		t.Fatalf("prefetch for another cwd published: pins=%+v warnings=%v", s.pins, w)
	}
	s.disarm()
	s.armFetched(s.fetch("/a", true), nil)
	s.applyRefresh(mark, "/a", stale, nil)
	if s.pins.Global[0] != "review" || len(s.selected) != 1 {
		t.Fatalf("prefetch from an older conversation published: %+v", s.pins)
	}
}

func TestEmptySkillPinsSkipDiscovery(t *testing.T) {
	loads := 0
	i := NewInteractive(InteractiveConfig{
		Agent: core.NewAgent(nil, "test-model", "", nil), CWD: t.TempDir(), PreloadPinnedSkills: true,
		LoadSkillPins: func(string) (skills.Pins, error) { loads++; return skills.Pins{}, nil },
		SkillSnapshot: func() []*skills.Skill { t.Fatal("empty pins triggered discovery"); return nil },
	})
	i.refreshPendingSkillPins()
	i.runSlash(context.Background(), "/clear")
	i.applyChangedCWD(core.NewAgent(nil, "test-model", "", nil), "p", "m", t.TempDir(), nil)
	if loads < 4 {
		t.Fatalf("pin preferences not refreshed at lifecycle boundaries: %d loads", loads)
	}
}

func TestStaleSkillPinArmDroppedAfterConsumption(t *testing.T) {
	b := &blockingSnapshot{entered: make(chan struct{}, 1), release: make(chan struct{})}
	i := NewInteractive(InteractiveConfig{
		Agent: core.NewAgent(nil, "test-model", "", nil), CWD: t.TempDir(), PreloadPinnedSkills: true,
		LoadSkillPins: func(string) (skills.Pins, error) { return skills.Pins{Global: []string{"review"}}, nil },
		SkillSnapshot: b.snapshot,
	})
	b.block = true
	done := make(chan struct{})
	go func() { i.SetPinnedSkillsPending(true); close(done) }()
	<-b.entered
	if got := i.consumePinnedSkills("first"); !strings.Contains(got, "Inspect carefully.") {
		t.Fatalf("initial selection not consumed: %q", got)
	}
	close(b.release)
	<-done
	if got := i.consumePinnedSkills("second"); got != "second" {
		t.Fatalf("stale rearm duplicated preload: %q", got)
	}
}

func TestSkillPinToggleFailureKeepsStateAndAppliesNoStatusOK(t *testing.T) {
	i := NewInteractive(InteractiveConfig{
		Agent: core.NewAgent(nil, "test-model", "", nil), CWD: t.TempDir(),
		LoadSkillPins:  func(string) (skills.Pins, error) { return skills.Pins{}, nil },
		ToggleSkillPin: func(string, string, bool) error { return errors.New("read-only") },
		SkillSnapshot:  reviewSnapshot,
	})
	i.openSkillsDialog()
	if !i.handleSkillPinKey(pinKey('g')) {
		t.Fatal("not handled")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if !strings.Contains(i.statusErr, "read-only") || i.statusOK != "" {
		t.Fatalf("status ok=%q err=%q", i.statusOK, i.statusErr)
	}
}
