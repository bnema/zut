package modes

import (
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/subagents"
)

// hasActivePortResidents mirrors the states shown by the editor-adjacent
// resident indicator. Callers hold i.mu while reading portInfo.Residents.
func hasActivePortResidents(residents []session.ResidentSummary) bool {
	for _, resident := range residents {
		switch subagents.ResidentState(resident.State) {
		case subagents.ResidentQueued, subagents.ResidentRunning:
			return true
		}
	}
	return false
}

// redrawAnimationTick is called by the main loop, including while input is idle.
func (i *Interactive) redrawAnimationTick(requestRedraw func()) {
	i.mu.Lock()
	busy := i.busy
	dictating := i.dictation.phase != ""
	sessionLoading := i.sessionDialog.Loading()
	sessionSearching := i.sessionDialog.SearchLoading()
	// Daemon clients have no local ResidentManager activity observer.
	residentAnimating := i.residentAnimating.Load() || hasActivePortResidents(i.portInfo.Residents)
	i.mu.Unlock()
	if busy || dictating || sessionLoading || sessionSearching || i.btwDialog.Loading() || residentAnimating {
		requestRedraw()
	}
}
