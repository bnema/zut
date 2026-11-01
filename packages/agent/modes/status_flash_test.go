package modes

import (
	"testing"
	"time"
)

func TestFlashStatusFadesThenClears(t *testing.T) {
	i := &Interactive{dirty: make(chan struct{}, 1)}
	i.mu.Lock()
	i.flashStatusLocked("switched to session abc")
	seq := i.statusFlashSeq
	start := i.statusFlashAt
	if i.statusFlashFadedLocked(start) {
		t.Fatal("fresh flash must not be faded")
	}
	if !i.statusFlashFadedLocked(start.Add(statusFlashSolid)) {
		t.Fatal("flash past solid phase must be faded")
	}
	i.mu.Unlock()

	if !i.clearFlashStatus(seq) || i.statusOK != "" {
		t.Fatalf("flash not cleared: %q", i.statusOK)
	}
}

func TestFlashStatusKeepsNewerStatus(t *testing.T) {
	i := &Interactive{dirty: make(chan struct{}, 1)}
	i.mu.Lock()
	i.flashStatusLocked("switched")
	seq := i.statusFlashSeq
	i.statusOK = "something else"
	faded := i.statusFlashFadedLocked(time.Now().Add(time.Hour))
	i.mu.Unlock()
	if faded {
		t.Fatal("a newer status must not render faded")
	}
	if i.clearFlashStatus(seq) || i.statusOK != "something else" {
		t.Fatalf("newer status was clobbered: %q", i.statusOK)
	}
}
