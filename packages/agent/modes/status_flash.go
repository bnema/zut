package modes

import "time"

// Transient ("flash") OK status: shown normally, then rendered muted
// (fade), then cleared. Any later status write wins: the timers only
// touch the line if it still holds the flashed message.
const (
	statusFlashSolid = 2500 * time.Millisecond
	statusFlashTotal = 4 * time.Second
)

// flashStatusLocked shows msg as a transient OK status. Caller holds i.mu.
func (i *Interactive) flashStatusLocked(msg string) {
	i.statusOK, i.statusErr = msg, ""
	i.statusFlashSeq++
	i.statusFlashMsg = msg
	i.statusFlashAt = time.Now()
	seq := i.statusFlashSeq
	time.AfterFunc(statusFlashSolid, i.invalidate)
	time.AfterFunc(statusFlashTotal, func() {
		if i.clearFlashStatus(seq) {
			i.invalidate()
		}
	})
}

// clearFlashStatus clears the status if it is still flash seq.
func (i *Interactive) clearFlashStatus(seq uint64) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.statusFlashSeq != seq || i.statusOK != i.statusFlashMsg {
		return false
	}
	i.statusOK, i.statusFlashMsg = "", ""
	return true
}

// statusFlashFadedLocked reports whether the current OK status is a flash
// past its solid phase and should render muted. Caller holds i.mu.
func (i *Interactive) statusFlashFadedLocked(now time.Time) bool {
	return i.statusOK != "" && i.statusOK == i.statusFlashMsg &&
		now.Sub(i.statusFlashAt) >= statusFlashSolid
}
