package modes

import (
	"strings"
	"time"
)

// Every OK status line is transient: the renderer stamps a new message the
// first time it sees it, draws it muted after statusFlashSolid and clears it
// after statusFlashTotal. A newer message restarts the cycle; the clear timer
// only touches the line if it still holds the stamped message. Progress
// messages (see statusIsSticky) stay until their owner replaces them.
const (
	statusFlashSolid = 2500 * time.Millisecond
	statusFlashTotal = 4 * time.Second
)

// statusIsSticky reports whether msg describes ongoing work and must not
// expire on its own.
func statusIsSticky(msg string) bool {
	return strings.HasSuffix(msg, "…") || strings.HasSuffix(msg, "...") ||
		msg == "Refreshing models"
}

// statusAwaitingInputLocked reports whether the OK line is an instruction for
// a prompt that is still waiting for the user (exit menu, profile picker), so
// it must stay visible. Caller holds i.mu.
func (i *Interactive) statusAwaitingInputLocked() bool {
	return i.daemonExitMenu ||
		(strings.HasPrefix(i.statusOK, profilePickerPromptPrefix) && i.modelDialog.Active())
}

// observeStatusLocked stamps a newly set OK status and schedules its fade and
// clear. A status held by a waiting prompt starts its cycle once the prompt
// closes. Caller holds i.mu.
func (i *Interactive) observeStatusLocked(now time.Time) {
	held := i.statusAwaitingInputLocked()
	if i.statusOK == i.statusFlashMsg && held == i.statusFlashHeld {
		return
	}
	i.statusFlashMsg = i.statusOK
	i.statusFlashAt = now
	i.statusFlashSeq++
	i.statusFlashHeld = held
	if held || i.statusOK == "" || statusIsSticky(i.statusOK) {
		return
	}
	seq := i.statusFlashSeq
	time.AfterFunc(statusFlashSolid, i.invalidate)
	time.AfterFunc(statusFlashTotal, func() {
		if i.clearFlashStatus(seq) {
			i.invalidate()
		}
	})
}

// clearFlashStatus clears the status if it is still the one stamped as seq.
func (i *Interactive) clearFlashStatus(seq uint64) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.statusFlashSeq != seq || i.statusOK != i.statusFlashMsg {
		return false
	}
	i.statusOK, i.statusFlashMsg = "", ""
	return true
}

// statusFlashFadedLocked reports whether the current OK status is past its
// solid phase and should render muted. Caller holds i.mu.
func (i *Interactive) statusFlashFadedLocked(now time.Time) bool {
	return i.statusOK != "" && i.statusOK == i.statusFlashMsg && !i.statusFlashHeld &&
		!statusIsSticky(i.statusOK) && now.Sub(i.statusFlashAt) >= statusFlashSolid
}
