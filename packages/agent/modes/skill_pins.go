package modes

import (
	"slices"
	"sync"

	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/tui"
)

// skillPinState holds the pinned-skill preload state of one interactive
// conversation. Interactive owns one value and guards the mutable fields with
// its own mutex, so every method except fetch and runToggle assumes the caller
// holds that lock. Nothing here contacts a provider; the pending
// bodies only join the first genuine user prompt (see consume).
//
// The three function fields are host callbacks (InteractiveConfig
// LoadSkillPins, ToggleSkillPin, SkillSnapshot). Any may be nil: without
// load there are no pins, without toggle the picker is read-only. They do
// file and directory I/O, so they must never run under the interactive mutex.
// Callers prefetch (fetch, runToggle) outside it and publish the result under
// it (armFetched, applyRefresh, completeToggle).
type skillPinState struct {
	load     func(cwd string) (skills.Pins, error)
	toggle   func(cwd, name string, global bool) error
	snapshot func() []*skills.Skill

	// ioMu serializes prefetch flows (fetch or toggle followed by publish) so
	// a reload never publishes preferences older than a completed save. It is
	// taken before agentMu and the interactive mutex; nothing waits for it
	// while holding either.
	ioMu sync.Mutex

	pending  bool
	pins     skills.Pins
	selected []*skills.Skill
	// gen changes whenever the conversation is armed or disarmed (fresh
	// conversation, /clear, /cd, resume, import, fork); rev also changes when
	// the preload is consumed or recalled. A prefetch records both so a result
	// overtaken by newer state is never published over it.
	gen uint64
	rev uint64
	// Remember the one expanded user prompt so queue recall and input history
	// expose the user's draft, never the injected instruction bodies. The
	// consumed selection is kept with it so recall restores exactly what was
	// consumed without touching the disk again.
	expanded         string
	original         string
	consumedSelected []*skills.Skill
	consumedPins     skills.Pins
}

// skillPinFetch is the result of prefetching preferences (and, for arming,
// the discoverable skills) outside the interactive mutex.
type skillPinFetch struct {
	pins      skills.Pins
	err       error
	available []*skills.Skill
}

// skillPinMark records the state a prefetch started from.
type skillPinMark struct {
	gen, rev uint64
	cwd      string
}

// skillPinToggle is a validated picker request captured under the mutex and
// executed outside it. list is the dialog's already-fetched skill list.
type skillPinToggle struct {
	cwd, name string
	global    bool
	list      []*skills.Skill
}

const skillPinsSavedNotice = "skill pins updated (apply on the next fresh conversation or /clear)"

func newSkillPinState(load func(string) (skills.Pins, error), toggle func(string, string, bool) error, snapshot func() []*skills.Skill) skillPinState {
	return skillPinState{load: load, toggle: toggle, snapshot: snapshot}
}

// mark captures the current generation, revision and cwd for a later publish.
func (s *skillPinState) mark(cwd string) skillPinMark {
	return skillPinMark{gen: s.gen, rev: s.rev, cwd: cwd}
}

// fetch loads the preferences for cwd and, when withSkills is set, the
// discoverable skills. It touches no mutable state, so it runs without the
// interactive mutex. Without a loader it does nothing.
func (s *skillPinState) fetch(cwd string, withSkills bool) skillPinFetch {
	var f skillPinFetch
	if s.load == nil {
		return f
	}
	f.pins, f.err = s.load(cwd)
	if withSkills && f.err == nil && len(f.pins.Global)+len(f.pins.Project) > 0 && s.snapshot != nil {
		f.available = s.snapshot()
	}
	return f
}

// disarm turns preloading off for a resumed, imported or forked conversation.
// It never performs I/O.
func (s *skillPinState) disarm() {
	s.pending = false
	s.selected = nil
	s.expanded, s.original = "", ""
	s.consumedSelected, s.consumedPins = nil, skills.Pins{}
	s.gen++
	s.rev++
}

// armFetched prepares preloading for a fresh conversation (or /clear, /cd)
// from a prefetch made for the current cwd. Pending and the resolved
// selection are published together. It returns warnings.
func (s *skillPinState) armFetched(f skillPinFetch, d *skillsDialog) []string {
	s.disarm()
	if s.load == nil {
		return nil
	}
	s.pending = true
	s.pins = f.pins
	if d != nil {
		d.pins = f.pins
		d.canPin = s.toggle != nil
	}
	return s.resolveLocked(f)
}

// resolveLocked resolves the selection from f and returns warnings for
// unreadable preferences and pinned names no longer discoverable.
func (s *skillPinState) resolveLocked(f skillPinFetch) []string {
	var warnings []string
	if f.err != nil {
		warnings = append(warnings, f.err.Error())
	}
	var missing []string
	s.selected, missing = f.pins.Resolve(f.available)
	return append(warnings, missing...)
}

// applyRefresh publishes a prefetch made from m without changing whether the
// preload is pending; cwd is the current working directory. A prefetch for
// another cwd or conversation (gen) is dropped whole. One overtaken by a
// consume or recall (rev) only feeds the dialog's markers, so it cannot
// replace an exactly restored selection. Otherwise the state's pins are
// updated and, while pending, the selection is re-resolved from f.available.
func (s *skillPinState) applyRefresh(m skillPinMark, cwd string, f skillPinFetch, d *skillsDialog) []string {
	if s.load == nil || m.cwd != cwd || m.gen != s.gen {
		return nil
	}
	if d != nil {
		d.pins = f.pins
		d.canPin = s.toggle != nil
	}
	var warnings []string
	if m.rev == s.rev {
		s.pins = f.pins
		if s.pending {
			return s.resolveLocked(f)
		}
	}
	if f.err != nil {
		warnings = append(warnings, f.err.Error())
	}
	return warnings
}

// toggleRequest validates a picker key: 'p' (project) or 'g' (global) on a
// pinnable, listed skill. ok is false when the key is not a pin key or the
// picker is not in a state that pins (closed, body view, empty, read-only
// host); the caller then forwards the key to the dialog.
func (s *skillPinState) toggleRequest(k tui.Key, cwd string, d *skillsDialog) (req skillPinToggle, ok bool) {
	if k.Kind != tui.KeyRune || (k.Rune != 'p' && k.Rune != 'g') {
		return req, false
	}
	sk := d.selectedSkill()
	if sk == nil || !d.canPin || s.toggle == nil {
		return req, false
	}
	return skillPinToggle{cwd: cwd, name: sk.Name, global: k.Rune == 'g', list: d.skills}, true
}

// runToggle saves the toggle and reloads the preferences. It performs file
// I/O and must run without the interactive mutex.
func (s *skillPinState) runToggle(req skillPinToggle) (f skillPinFetch, saveErr error) {
	if saveErr = s.toggle(req.cwd, req.name, req.global); saveErr != nil {
		return f, saveErr
	}
	f = s.fetch(req.cwd, false)
	f.available = req.list
	return f, nil
}

// completeToggle publishes a finished toggle. statusErr carries a save
// failure and statusOK the success notice; pins are refreshed on success from
// the dialog's own skill list.
func (s *skillPinState) completeToggle(m skillPinMark, cwd string, f skillPinFetch, saveErr error, d *skillsDialog) (statusOK, statusErr string, warnings []string) {
	if saveErr != nil {
		return "", "save skill pin: " + saveErr.Error(), nil
	}
	return skillPinsSavedNotice, "", s.applyRefresh(m, cwd, f, d)
}

// consume folds the pending skill bodies into prompt exactly once. Later calls
// return the prompt unchanged until the conversation is armed again. The
// consumed selection stays in the state so recall can restore it exactly.
func (s *skillPinState) consume(prompt string) string {
	if !s.pending {
		return prompt
	}
	s.pending = false
	s.rev++
	out := skills.PreloadPrompt(s.selected, prompt)
	if out != prompt {
		s.expanded, s.original = out, prompt
		s.consumedSelected, s.consumedPins = s.selected, s.pins
	}
	s.selected = nil
	return out
}

// rawPrompt removes only the exact expansion generated by this conversation.
// Arbitrary user text resembling a pinned-skill preamble is never stripped.
func (s *skillPinState) rawPrompt(prompt string) string {
	if s.expanded != "" && prompt == s.expanded {
		return s.original
	}
	return prompt
}

// recall releases an undelivered preload so the next user submit can carry it.
// It restores exactly the consumed selection without discovery or preference
// I/O, so it is safe under the interactive mutex; queue removal serializes
// delivery.
func (s *skillPinState) recall(prompt string) {
	if s.expanded == "" || prompt != s.expanded || s.load == nil {
		return
	}
	s.pending = true
	s.selected, s.pins = s.consumedSelected, s.consumedPins
	s.rev++
}

// notice renders the read-only "[Pinned skills]" section shown above the
// input while pins are pending, never as editor text.
func (s *skillPinState) notice(th tui.Theme, cols int) []string {
	if !s.pending || len(s.selected) == 0 || cols < 4 {
		return nil
	}
	names := make([]string, 0, len(s.selected))
	for _, sk := range s.selected {
		name := sk.Name
		project, global := s.pins.Scopes(s.selected, sk)
		switch {
		case project && global:
			name += " (project + global)"
		case project:
			name += " (project)"
		case global:
			name += " (global)"
		}
		names = append(names, name)
	}
	return tui.RenderPinnedSkills(th, names, cols)
}

// SetPinnedSkillsPending arms (true) or disarms (false) the pinned-skill
// preload for the current conversation. Hosts disarm after loading a resumed,
// imported or forked session; /clear and /cd re-arm internally. Arming
// prefetches outside the mutex and is abandoned if the preload state changed
// meanwhile (including consumption or recall).
func (i *Interactive) SetPinnedSkillsPending(pending bool) {
	if !pending {
		i.mu.Lock()
		i.skillPins.disarm()
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.skillPins.ioMu.Lock()
	defer i.skillPins.ioMu.Unlock()
	i.mu.Lock()
	rev := i.skillPins.rev
	i.mu.Unlock()
	var f skillPinFetch
	cwd := i.skillPinCWD()
	for {
		f = i.skillPins.fetch(cwd, true)
		i.mu.Lock()
		if i.skillPins.rev != rev {
			i.mu.Unlock()
			return
		}
		if i.cfg.CWD == cwd {
			break
		}
		cwd = i.cfg.CWD
		i.mu.Unlock()
	}
	i.armSkillPinsLocked(f)
	i.mu.Unlock()
	i.invalidate()
}

// skillPinCWD reads the working directory under the mutex.
func (i *Interactive) skillPinCWD() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.cfg.CWD
}

// lockWithSkillPinFetch prefetches the preferences and discoverable skills for
// the current cwd without the mutex, then returns holding i.mu with the fetch
// still matching i.cfg.CWD (it refetches if the cwd moved meanwhile). The
// caller publishes with armSkillPinsLocked, unlocks i.mu, then calls done.
func (i *Interactive) lockWithSkillPinFetch() (f skillPinFetch, done func()) {
	i.skillPins.ioMu.Lock()
	cwd := i.skillPinCWD()
	for {
		f = i.skillPins.fetch(cwd, true)
		i.mu.Lock()
		if i.cfg.CWD == cwd {
			return f, i.skillPins.ioMu.Unlock
		}
		cwd = i.cfg.CWD
		i.mu.Unlock()
	}
}

// armSkillPinsLocked arms the preload for a fresh conversation from a prefetch
// for the current cwd and reports warnings once. The caller must hold i.mu.
func (i *Interactive) armSkillPinsLocked(f skillPinFetch) {
	i.addSkillPinWarningsLocked(i.skillPins.armFetched(f, i.skillsDialog))
}

// addSkillPinWarningsLocked shows pin warnings in the host-only reload error
// list, skipping ones already shown. The caller must hold i.mu.
func (i *Interactive) addSkillPinWarningsLocked(warnings []string) {
	for _, w := range warnings {
		if w = "skill pins: " + w; !slices.Contains(i.reloadErrors, w) {
			i.reloadErrors = append(i.reloadErrors, w)
		}
	}
}

// refreshPendingSkillPins re-resolves a pending selection (entry.pre may have
// added or removed skills). Nothing is read when nothing is pending. The
// callbacks run without the mutex; a result overtaken by a consume, recall,
// arm or cwd change is dropped.
func (i *Interactive) refreshPendingSkillPins() {
	i.skillPins.ioMu.Lock()
	defer i.skillPins.ioMu.Unlock()
	i.mu.Lock()
	if !i.skillPins.pending || i.skillPins.load == nil {
		i.mu.Unlock()
		return
	}
	mark := i.skillPins.mark(i.cfg.CWD)
	i.mu.Unlock()
	f := i.skillPins.fetch(mark.cwd, true)
	i.mu.Lock()
	i.addSkillPinWarningsLocked(i.skillPins.applyRefresh(mark, i.cfg.CWD, f, i.skillsDialog))
	i.mu.Unlock()
}

// openSkillsDialogWithPins opens the picker on a single skill snapshot. The
// preferences load outside the mutex, and a pending selection re-resolves from
// that same list rather than walking discovery again. If the cwd moved while
// loading, the whole prefetch is redone.
func (i *Interactive) openSkillsDialogWithPins() {
	i.skillPins.ioMu.Lock()
	defer i.skillPins.ioMu.Unlock()
	for {
		i.mu.Lock()
		mark := i.skillPins.mark(i.cfg.CWD)
		i.mu.Unlock()
		var list []*skills.Skill
		if i.cfg.SkillSnapshot != nil {
			list = i.cfg.SkillSnapshot()
		}
		f := i.skillPins.fetch(mark.cwd, false)
		f.available = list
		i.mu.Lock()
		if i.cfg.CWD == mark.cwd {
			i.skillsDialog.Open(list)
			i.addSkillPinWarningsLocked(i.skillPins.applyRefresh(mark, i.cfg.CWD, f, i.skillsDialog))
			i.mu.Unlock()
			return
		}
		i.mu.Unlock()
	}
}

// consumePinnedSkills folds pending pins into the first genuine user prompt.
func (i *Interactive) consumePinnedSkills(prompt string) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.skillPins.consume(prompt)
}

// handleSkillPinKey applies the picker's p/g keys. It reports whether the key
// was consumed. The preference save and reload run without the mutex; the
// result is applied under it from the dialog's own skill list.
func (i *Interactive) handleSkillPinKey(k tui.Key) bool {
	i.mu.Lock()
	req, ok := i.skillPins.toggleRequest(k, i.cfg.CWD, i.skillsDialog)
	i.mu.Unlock()
	if !ok {
		return false
	}
	i.skillPins.ioMu.Lock()
	defer i.skillPins.ioMu.Unlock()
	i.mu.Lock()
	mark := i.skillPins.mark(req.cwd)
	i.mu.Unlock()
	f, saveErr := i.skillPins.runToggle(req)
	i.mu.Lock()
	okText, errText, warnings := i.skillPins.completeToggle(mark, i.cfg.CWD, f, saveErr, i.skillsDialog)
	i.statusOK, i.statusErr = okText, errText
	i.addSkillPinWarningsLocked(warnings)
	i.mu.Unlock()
	return true
}
