package modes

import (
	"context"
	"strings"
	"time"
)

// paintPaceRate is the minimum number of runes the streaming pacer releases
// per tick. With a 16ms tick this is ~375 runes/s, which reads like typing
// when a provider sends a few fat chunks instead of a steady drip.
const paintPaceRate = 6

// paintCatchUpTicks bounds how far the painted text may lag behind what the
// provider has sent. A larger backlog is released proportionally faster so
// a fast stream never trails by more than roughly this many ticks.
const paintCatchUpTicks = 24

// paintPaceInterval is the pacer tick. It matches the redraw throttle so the
// pacer never produces frames faster than the terminal can show them.
const paintPaceInterval = 16 * time.Millisecond

type streamState uint8

const (
	// streamIdle: no assistant reply is on screen as live text.
	streamIdle streamState = iota
	// streamReceiving: the provider is still sending text deltas.
	streamReceiving
	// streamDraining: the provider finished; the pacer is painting the
	// remaining buffered text before the transcript takes over.
	streamDraining
)

// streamPresenter owns the paced, live display of one assistant reply.
//
// The agent appends the finished message to the transcript on its own
// goroutine, while the pacer is often still typing that same text. Showing
// both would paint the reply twice. The presenter prevents that with one
// rule: while it is not idle, the transcript is shown only up to the length
// it had when the stream started (the anchor). Everything the agent appended
// since then — the assistant message and any fast tool results — becomes
// visible in the same frame that the live text disappears.
//
// All methods must be called with Interactive.mu held.
type streamPresenter struct {
	state        streamState
	painted      strings.Builder
	paintedRunes int
	pending      []rune

	// anchor is the raw transcript length when the stream started, and
	// anchorRev the transcript revision at that length. A negative anchor
	// disables transcript clipping (no agent attached).
	anchor    int
	anchorRev uint64

	// gates holds, per tool-call id, how many runes of live text must be
	// painted before that tool block may appear, so a tool never shows up
	// above the prose that preceded it. A missing entry means visible.
	gates map[string]int
}

// Active reports whether live text is still owned by the presenter.
func (s *streamPresenter) Active() bool { return s.state != streamIdle }

// Text returns the live text painted so far.
func (s *streamPresenter) Text() string { return s.painted.String() }

// Start begins a new reply. Anything still buffered from a previous reply is
// dropped: that reply is already complete in the transcript.
func (s *streamPresenter) Start(anchor int, anchorRev uint64) {
	s.Reset()
	s.state = streamReceiving
	s.anchor = anchor
	s.anchorRev = anchorRev
}

// Push buffers a provider text delta for pacing.
func (s *streamPresenter) Push(delta string) {
	if s.state != streamReceiving || delta == "" {
		return
	}
	s.pending = append(s.pending, []rune(delta)...)
}

// Finish marks the provider side of the reply as complete. The presenter
// stays active until the pacer has painted every buffered rune.
func (s *streamPresenter) Finish() {
	if s.state == streamIdle {
		return
	}
	if len(s.pending) == 0 {
		s.Reset()
		return
	}
	s.state = streamDraining
}

// Reset returns to idle immediately and reveals the whole transcript.
func (s *streamPresenter) Reset() {
	s.state = streamIdle
	s.painted.Reset()
	s.paintedRunes = 0
	s.pending = s.pending[:0]
	s.anchor = 0
	s.anchorRev = 0
	s.gates = nil
}

// needsTick reports whether the pacer has work to do.
func (s *streamPresenter) needsTick() bool {
	return len(s.pending) > 0 || s.state == streamDraining
}

// Tick paints the next batch of buffered runes. It reports whether the
// visible state changed.
func (s *streamPresenter) Tick() bool {
	if len(s.pending) == 0 {
		if s.state == streamDraining {
			s.Reset()
			return true
		}
		return false
	}
	n := max(paintPaceRate, (len(s.pending)+paintCatchUpTicks-1)/paintCatchUpTicks)
	n = min(n, len(s.pending))
	s.painted.WriteString(string(s.pending[:n]))
	s.paintedRunes += n
	s.pending = s.pending[n:]
	if len(s.pending) == 0 && s.state == streamDraining {
		// The last rune is painted: hand over to the transcript in the same
		// frame instead of leaving a one-tick gap.
		s.Reset()
	}
	return true
}

// Gate records where a tool block may appear. The first registration wins.
func (s *streamPresenter) Gate(id string) {
	if s.state == streamIdle {
		return
	}
	if _, ok := s.gates[id]; ok {
		return
	}
	if s.gates == nil {
		s.gates = make(map[string]int)
	}
	s.gates[id] = s.paintedRunes + len(s.pending)
}

// GateOpen reports whether the tool block may render yet.
func (s *streamPresenter) GateOpen(id string) bool {
	gate, ok := s.gates[id]
	return !ok || s.paintedRunes >= gate
}

// transcriptLimit returns how many raw transcript messages may be shown and
// the revision describing that prefix. ok is false when no clipping applies.
func (s *streamPresenter) transcriptLimit(total int) (limit int, rev uint64, ok bool) {
	if s.state == streamIdle || s.anchor < 0 || s.anchor > total {
		// A shorter transcript means it was replaced; never hide content of
		// a transcript the anchor does not describe.
		return 0, 0, false
	}
	return s.anchor, s.anchorRev, true
}

// startStreamLocked anchors a new reply at the current transcript length.
func (i *Interactive) startStreamLocked() {
	anchor, rev := -1, uint64(0)
	if i.agent != nil {
		// The agent goroutine delivers this event synchronously, so the
		// transcript cannot grow between these two reads.
		anchor, rev = i.agent.MessageCount(), i.agent.Revision()
	}
	i.stream.Start(anchor, rev)
}

// wakeStreamPacer nudges an idle pacer without blocking.
func (i *Interactive) wakeStreamPacer() {
	select {
	case i.streamWake <- struct{}{}:
	default:
	}
}

// runStreamPacer paints buffered deltas a small batch per tick. It sleeps
// while there is nothing to paint and exits when ctx is cancelled.
//
// Providers chunk text very differently: the Anthropic API-key path drips
// many small deltas, while the OAuth path can coalesce a reply into a few
// large chunks that would otherwise just appear. Pacing makes every path
// look the same on screen.
func (i *Interactive) runStreamPacer(ctx context.Context) {
	ticker := time.NewTicker(paintPaceInterval)
	ticker.Stop()
	defer ticker.Stop()
	running := false
	for {
		i.mu.Lock()
		need := i.stream.needsTick()
		i.mu.Unlock()
		switch {
		case need && !running:
			ticker.Reset(paintPaceInterval)
			running = true
		case !need && running:
			ticker.Stop()
			running = false
		}
		var tick <-chan time.Time
		if running {
			tick = ticker.C
		}
		select {
		case <-ctx.Done():
			return
		case <-i.streamWake:
		case <-tick:
			i.mu.Lock()
			changed := i.stream.Tick()
			i.mu.Unlock()
			if changed {
				i.invalidate()
			}
		}
	}
}
