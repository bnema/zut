package modes

import (
	"context"
	"errors"
	"time"

	"github.com/bnema/zut/packages/agent/internal/sessionindex"
	"github.com/bnema/zut/packages/tui"
)

const (
	// finderSearchDebounce delays searches triggered by query edits so a
	// burst of keystrokes runs one search.
	finderSearchDebounce = 80 * time.Millisecond
	// finderMinHeight is the smallest box Render lays out without overflow.
	finderMinHeight = 9
	// finderIndexRetryDelay waits for another process's index update
	// before retrying once.
	finderIndexRetryDelay = time.Second
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
	i.finderSnippets.reset()
	act := i.finder.Open(0)
	i.mu.Unlock()
	i.dispatchFinderSearch(ctx, act, 0)
	i.startFinderIndexUpdate(ctx)
	i.loadFinderLive(ctx)
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
	i.finderSnippets.reset()
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
		i.finderRename(ctx, act.Item, act.Title)
	case finderActionDelete:
		i.finderDelete(ctx, act.Item)
	}
	i.requestFinderSnippets(ctx)
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
				i.requestFinderSnippets(ctx)
				i.invalidate()
			}
		}
		select {
		case i.loopCalls <- apply:
		case <-searchCtx.Done():
		}
	}()
}

// finderOpen opens the chosen item and jumps to the hit once resumed.
func (i *Interactive) finderOpen(item finderItem, hit int) {
	i.finderOpenItem(item, hit)
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

// finderResumeChoice executes the chosen way to open plan.
func (i *Interactive) finderResumeChoice(plan resumePlan, choice resumeChoice) {
	i.finderExecuteResume(plan, choice)
}

// defaultFinderSources opens the session index on first use and returns
// the Sessions and Messages tabs, or nil when the index cannot be opened.
func (i *Interactive) defaultFinderSources() []finderSource {
	index := i.ensureFinderIndex()
	if index == nil {
		return nil
	}
	return []finderSource{
		&finderSessionSource{index: index, now: time.Now},
		&finderMessageSource{index: index},
	}
}

func (i *Interactive) ensureFinderIndex() *sessionindex.Index {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.finderIndex != nil {
		return i.finderIndex
	}
	index, err := sessionindex.Open(i.sessionsRoot(), sessionindex.Options{Skip: isHiddenTranscriptMessage})
	if err != nil {
		return nil
	}
	i.finderIndex = index
	return index
}

// startFinderIndexUpdate refreshes the index in the background, shows
// progress in the finder header, and re-runs the current search when done.
// ErrBusy (another process is updating) is retried once after
// finderRetryDelay; the finder keeps searching the current generation.
func (i *Interactive) startFinderIndexUpdate(ctx context.Context) {
	i.mu.Lock()
	index := i.finderIndex
	if index == nil || i.finderUpdating {
		i.mu.Unlock()
		return
	}
	i.finderUpdating = true
	updateCtx, cancel := context.WithCancel(ctx)
	i.finderUpdateCancel = cancel
	retry := i.finderRetryDelay
	if i.finder != nil {
		i.finder.SetIndexing(true, 0, 0)
	}
	i.mu.Unlock()

	progress := func(done, total int) {
		select {
		case i.loopCalls <- func() {
			i.mu.Lock()
			if i.finder != nil && i.finderUpdating {
				i.finder.SetIndexing(true, done, total)
			}
			i.mu.Unlock()
			i.invalidate()
		}:
		default: // progress is best-effort; never block the scan
		}
	}
	go func() {
		defer cancel()
		err := index.Update(updateCtx, progress)
		if errors.Is(err, sessionindex.ErrBusy) {
			timer := time.NewTimer(retry)
			select {
			case <-timer.C:
				err = index.Update(updateCtx, progress)
			case <-updateCtx.Done():
				timer.Stop()
				err = updateCtx.Err()
			}
		}
		finish := func() {
			i.mu.Lock()
			i.finderUpdating = false
			i.finderUpdateCancel = nil
			var act finderAction
			if i.finder != nil {
				i.finder.SetIndexing(false, 0, 0)
				act = i.finder.Refresh()
			}
			i.mu.Unlock()
			if err != nil && !errors.Is(err, sessionindex.ErrBusy) && updateCtx.Err() == nil {
				i.setFinderStatusErr("finder: index update: " + err.Error())
			}
			i.dispatchFinderSearch(ctx, act, 0)
			i.invalidate()
		}
		select {
		case i.loopCalls <- finish:
		case <-ctx.Done():
		}
	}()
}

// closeFinderIndex stops a running update and closes the index.
func (i *Interactive) closeFinderIndex() {
	i.mu.Lock()
	cancel := i.finderUpdateCancel
	index := i.finderIndex
	i.finderUpdateCancel = nil
	i.finderIndex = nil
	i.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if index != nil {
		_ = index.Close()
	}
}
