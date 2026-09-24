package modes

import (
	"strings"
	"testing"
)

// drainTicks paints n pacer ticks worth of text.
func drainTicks(s *streamPresenter, n int) {
	for range n {
		s.Tick()
	}
}

// A tool that arrives while paced text is still buffered stays hidden until
// the pacer has painted the text that preceded it.
func TestToolGateHoldsToolUntilTextDrains(t *testing.T) {
	var s streamPresenter
	s.Start(-1, 0)
	s.Push(strings.Repeat("a", 30))
	s.Tick() // paints paintPaceRate runes

	s.Gate("t1")
	if s.GateOpen("t1") {
		t.Fatal("tool visible before the preceding text was painted")
	}
	for s.paintedRunes < 30 {
		if s.GateOpen("t1") {
			t.Fatalf("tool visible at %d/30 painted runes", s.paintedRunes)
		}
		s.Tick()
	}
	if !s.GateOpen("t1") {
		t.Fatal("tool still hidden after the preceding text was painted")
	}
}

// Without live text a tool shows immediately.
func TestToolGateOpenWhenNotStreaming(t *testing.T) {
	var s streamPresenter
	s.Gate("t1")
	if !s.GateOpen("t1") {
		t.Fatal("tool hidden while no text is streaming")
	}
}

// First registration wins: a later EvToolCall must not move the gate.
func TestToolGateFirstRegistrationWins(t *testing.T) {
	var s streamPresenter
	s.Start(-1, 0)
	s.Push("0123456789")
	s.Gate("t1")
	first := s.gates["t1"]
	s.Push("more text")
	s.Gate("t1")
	if s.gates["t1"] != first {
		t.Fatalf("gate moved from %d to %d", first, s.gates["t1"])
	}
}

// Once the stream finishes, every gate is open again.
func TestToolGatesOpenAfterStreamEnds(t *testing.T) {
	var s streamPresenter
	s.Start(-1, 0)
	s.Push("0123456789")
	s.Gate("t1")
	s.Finish()
	drainTicks(&s, 10)
	if s.Active() {
		t.Fatal("stream still active after draining")
	}
	if !s.GateOpen("t1") {
		t.Fatal("tool re-hidden after the stream ended")
	}
}
