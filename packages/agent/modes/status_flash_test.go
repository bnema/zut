package modes

import (
	"testing"
	"time"
)

func TestStatusFadesThenClears(t *testing.T) {
	i := &Interactive{dirty: make(chan struct{}, 1)}
	now := time.Now()
	i.mu.Lock()
	i.statusOK = "switched to session abc"
	i.observeStatusLocked(now)
	seq := i.statusFlashSeq
	if i.statusFlashFadedLocked(now) {
		t.Fatal("fresh status must not be faded")
	}
	if !i.statusFlashFadedLocked(now.Add(statusFlashSolid)) {
		t.Fatal("status past solid phase must be faded")
	}
	i.mu.Unlock()

	if !i.clearFlashStatus(seq) || i.statusOK != "" {
		t.Fatalf("status not cleared: %q", i.statusOK)
	}
}

func TestStatusNewerMessageRestartsCycle(t *testing.T) {
	i := &Interactive{dirty: make(chan struct{}, 1)}
	now := time.Now()
	i.mu.Lock()
	i.statusOK = "first"
	i.observeStatusLocked(now)
	seq := i.statusFlashSeq
	i.statusOK = "second"
	later := now.Add(time.Second)
	i.observeStatusLocked(later)
	faded := i.statusFlashFadedLocked(later)
	i.mu.Unlock()
	if faded {
		t.Fatal("a newer status must start solid")
	}
	if i.clearFlashStatus(seq) || i.statusOK != "second" {
		t.Fatalf("newer status was clobbered: %q", i.statusOK)
	}
}

func TestStatusHeldWhileExitMenuOpen(t *testing.T) {
	i := &Interactive{dirty: make(chan struct{}, 1)}
	now := time.Now()
	i.mu.Lock()
	i.daemonExitMenu = true
	i.statusOK = daemonExitPrompt
	i.observeStatusLocked(now)
	seq := i.statusFlashSeq
	if i.statusFlashFadedLocked(now.Add(time.Hour)) {
		t.Fatal("exit prompt must not fade while the menu is open")
	}
	// Menu closes with the prompt still shown: the fade cycle starts now.
	i.daemonExitMenu = false
	closed := now.Add(time.Minute)
	i.observeStatusLocked(closed)
	if i.statusFlashSeq == seq || i.statusFlashHeld {
		t.Fatal("closing the menu must restart the cycle")
	}
	if !i.statusFlashFadedLocked(closed.Add(statusFlashSolid)) {
		t.Fatal("released prompt must fade")
	}
	i.mu.Unlock()
	if i.clearFlashStatus(seq) {
		t.Fatal("stale held stamp must not clear the status")
	}
}

func TestStickyStatusNeverFades(t *testing.T) {
	for _, msg := range []string{"Reloading extensions...", "fork: switching session…", "Refreshing models"} {
		i := &Interactive{dirty: make(chan struct{}, 1)}
		now := time.Now()
		i.mu.Lock()
		i.statusOK = msg
		i.observeStatusLocked(now)
		faded := i.statusFlashFadedLocked(now.Add(time.Hour))
		i.mu.Unlock()
		if faded {
			t.Fatalf("%q must not fade", msg)
		}
	}
}
