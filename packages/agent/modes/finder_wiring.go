package modes

import (
	"context"
	"time"

	"github.com/bnema/zut/packages/tui"
)

const (
	// finderSearchDebounce delays searches triggered by query edits so a
	// burst of keystrokes runs one search.
	finderSearchDebounce = 80 * time.Millisecond
	// finderMinHeight is the smallest box Render lays out without overflow.
	finderMinHeight = 9
)

// openFinderShortcut opens the finder from ctrl+f unless another dialog
// owns the screen.
func (i *Interactive) openFinderShortcut(ctx context.Context) {
	if i.confirmChildActive() {
		return
	}
	i.openFinder(ctx)
}

// openFinder opens the finder on its first tab and starts the initial search.
func (i *Interactive) openFinder(ctx context.Context) {
	if i.cfg.SessionsDisabled {
		i.setFinderStatusErr("sessions are disabled by --no-session")
		return
	}
	var sources []finderSource
	if i.finderSources != nil {
		sources = i.finderSources()
	}
	if len(sources) == 0 {
		i.setFinderStatusErr("finder unavailable")
		return
	}
	i.mu.Lock()
	i.finder = newFinderModel(i.cfg.CWD, sources...)
	act := i.finder.Open(0)
	i.mu.Unlock()
	i.dispatchFinderSearch(ctx, act, 0)
	i.invalidate()
}

func (i *Interactive) setFinderStatusErr(message string) {
	i.mu.Lock()
	i.statusErr, i.statusOK = message, ""
	i.mu.Unlock()
	i.invalidate()
}

// closeFinder closes the finder and cancels its in-flight search.
func (i *Interactive) closeFinder() {
	i.mu.Lock()
	if i.finder != nil {
		i.finder.Close()
	}
	cancel := i.finderCancel
	i.finderCancel = nil
	i.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// handleFinderKey routes a key to the open finder and applies its action.
func (i *Interactive) handleFinderKey(ctx context.Context, k tui.Key) {
	i.mu.Lock()
	act := i.finder.HandleKey(k)
	delay := i.finderDelay
	i.mu.Unlock()
	switch act.Kind {
	case finderActionSearch:
		i.dispatchFinderSearch(ctx, act, delay)
	case finderActionClose:
		i.closeFinder()
	case finderActionOpen:
		i.finderOpen(act.Item, act.Hit)
	case finderActionRename:
		i.finderRename(act.Item)
	case finderActionDelete:
		i.finderDelete(act.Item)
	}
	i.invalidate()
}

// dispatchFinderSearch cancels the previous search and runs act after
// delay on a goroutine. The result is applied on the Run loop through
// loopCalls; the model drops it when a newer search superseded it.
func (i *Interactive) dispatchFinderSearch(ctx context.Context, act finderAction, delay time.Duration) {
	if act.Kind != finderActionSearch || act.Source == nil {
		return
	}
	searchCtx, cancel := context.WithCancel(ctx)
	i.mu.Lock()
	previous := i.finderCancel
	i.finderCancel = cancel
	i.mu.Unlock()
	if previous != nil {
		previous()
	}
	go func() {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-searchCtx.Done():
				timer.Stop()
				return
			}
		}
		results, err := act.Source.Search(searchCtx, act.Query)
		if searchCtx.Err() != nil {
			return
		}
		apply := func() {
			i.mu.Lock()
			applied := i.finder.ApplyResults(act.Gen, act.TabID, results, err)
			i.mu.Unlock()
			if applied {
				i.invalidate()
			}
		}
		select {
		case i.loopCalls <- apply:
		case <-searchCtx.Done():
		}
	}()
}

// finderOpen opens the chosen item. TODO: resume or attach the session and
// jump to the hit.
func (i *Interactive) finderOpen(item finderItem, hit int) {
	_, _ = item, hit
	i.setFinderStatusErr("finder: open is not implemented yet")
}

// finderRename renames the chosen item. TODO: inline rename.
func (i *Interactive) finderRename(item finderItem) {
	_ = item
	i.setFinderStatusErr("finder: rename is not implemented yet")
}

// finderDelete deletes the chosen item. TODO: delete the session file.
func (i *Interactive) finderDelete(item finderItem) {
	_ = item
	i.setFinderStatusErr("finder: delete is not implemented yet")
}

// openFinderResume resumes plan directly when it ran in this directory,
// otherwise shows the resume confirmation.
func (i *Interactive) openFinderResume(plan resumePlan) {
	confirm := decideResume(plan)
	if confirm == nil {
		i.finderResumeChoice(plan, resumeChoiceResume)
		return
	}
	i.mu.Lock()
	i.finderResume = confirm
	i.mu.Unlock()
	i.invalidate()
}

// handleFinderResumeKey routes a key to the resume confirmation.
func (i *Interactive) handleFinderResumeKey(k tui.Key) {
	i.mu.Lock()
	confirm := i.finderResume
	choice := confirm.HandleKey(k)
	if choice != resumeChoiceNone {
		i.finderResume = nil
	}
	i.mu.Unlock()
	if choice != resumeChoiceNone && choice != resumeChoiceCancel {
		i.finderResumeChoice(confirm.plan, choice)
	}
	i.invalidate()
}

// finderResumeChoice executes the chosen way to open plan. TODO: resume,
// switch directory, fork, or attach.
func (i *Interactive) finderResumeChoice(plan resumePlan, choice resumeChoice) {
	_, _ = plan, choice
	i.setFinderStatusErr("finder: resume is not implemented yet")
}
