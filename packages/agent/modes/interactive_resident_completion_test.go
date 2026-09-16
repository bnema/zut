package modes

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/internal/orchestration"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
)

func TestResidentCompletionBatchDoesNotReportTerminalSiblingAsRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ag := core.NewAgent(nil, "model", "", nil)
	i := &Interactive{agent: ag, busy: true, runCtx: ctx}
	release := i.beginCompletionDeliveryHold()
	defer release()
	i.TrackResidentSubagent("first", "turn-1")
	i.TrackResidentSubagent("second", "turn-2")

	tracker := i.ensureCompletionTracker()
	tracker.Report(subagents.Completion{AgentID: "first", TurnID: "turn-1", Status: "completed"})
	tracker.Report(subagents.Completion{AgentID: "second", TurnID: "turn-2", Status: "completed"})
	i.requestCompletionDelivery()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		queued := ag.PendingQueuedMessages()
		if len(queued) == 2 {
			for _, message := range queued {
				if strings.Contains(message.Text, "Still running:") {
					t.Fatalf("terminal sibling reported as running: %q", message.Text)
				}
			}
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("queued completions = %#v, want 2", queued)
		case <-time.After(time.Millisecond):
		}
	}
}

// TestReleaseCompletionHoldDoesNotInvertCompletionLock pins the lock order
// between the completion delivery hold and the coordinator state read. The
// release path used to hold completionDeliveryMu while it called
// goalContinuationMessage and interruptedPromptPending, which acquire i.mu,
// while coordinatorHasPendingWorkers is legitimately called by a caller that
// already holds i.mu. The two orders deadlocked.
//
// goalSampleGate makes the probe deterministic: the test waits until the
// release is provably inside goal sampling before it reads coordinator state.
// Under the inverted order that sample still owns completionDeliveryMu, so the
// probe queues behind it and the bounded wait fails the test instead of
// hanging the package.
func TestReleaseCompletionHoldDoesNotInvertCompletionLock(t *testing.T) {
	goal := &core.SessionGoal{ID: "goal-1", Status: core.GoalActive, Objective: "keep building"}
	gate := newGoalSampleGate(func(int64) *core.SessionGoal { return goal })
	defer gate.unblockNow()

	i := &Interactive{agent: core.NewAgent(nil, "model", "", goalToolRegistry())}
	i.cfg.CurrentGoal = gate.current
	// A manager wake is not the subject of this test; stop it before it starts
	// a turn.
	i.cfg.PersistGoalRuntime = func(*core.SessionGoal) error { return errors.New("stop after wake") }
	i.completionDeliveryHolds = 1

	released := startGateRelease(i.releaseCompletionDeliveryHold)
	enteredGate(t, gate, "releaseCompletionDeliveryHold")

	// A caller holding i.mu that reads coordinator state takes
	// completionDeliveryMu: that is the i.mu -> completionDeliveryMu order this
	// test protects. Probing from that side must not queue behind the release.
	i.mu.Lock()
	pending := make(chan bool, 1)
	go func() {
		pending <- i.coordinatorHasPendingWorkers()
	}()

	select {
	case <-pending:
	case <-time.After(completionGateTimeout):
		// Break the cycle before failing so the release can sample and relock.
		i.mu.Unlock()
		gate.unblockNow()
		awaitGate(t, released, "releaseCompletionDeliveryHold")
		t.Fatal("coordinatorHasPendingWorkers blocked behind releaseCompletionDeliveryHold")
	}
	i.mu.Unlock()
	gate.unblockNow()
	awaitGate(t, released, "releaseCompletionDeliveryHold")

	i.completionDeliveryMu.Lock()
	holds := i.completionDeliveryHolds
	i.completionDeliveryMu.Unlock()
	if holds != 0 {
		t.Fatalf("completionDeliveryHolds = %d, want 0", holds)
	}
}

// TestNonSealingReleaseCompletionHoldSkipsGoalSampling pins that only the
// release which drains the last hold reads goal and interruption state. A
// nested release (2 -> 1) or a duplicate release (already 0) must return
// without acquiring i.mu, which those reads take.
func TestNonSealingReleaseCompletionHoldSkipsGoalSampling(t *testing.T) {
	for _, tc := range []struct {
		name      string
		holds     int
		wantHolds int
	}{
		{name: "nested", holds: 2, wantHolds: 1},
		{name: "duplicate", holds: 0, wantHolds: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := &Interactive{agent: core.NewAgent(nil, "model", "", nil)}
			i.completionDeliveryHolds = tc.holds

			i.mu.Lock()
			done := startGateRelease(i.releaseCompletionDeliveryHold)
			select {
			case <-done:
			case <-time.After(completionGateTimeout):
				// Break the cycle before failing so the release goroutine exits.
				i.mu.Unlock()
				awaitGate(t, done, "non-sealing release")
				t.Fatal("non-sealing release sampled goal state while i.mu was held")
			}
			i.mu.Unlock()

			i.completionDeliveryMu.Lock()
			holds := i.completionDeliveryHolds
			i.completionDeliveryMu.Unlock()
			if holds != tc.wantHolds {
				t.Fatalf("completionDeliveryHolds = %d, want %d", holds, tc.wantHolds)
			}
		})
	}
}

// TestStaleReleaseCompletionHoldDoesNotSealNewerWave covers a duplicate release
// that arrives after the earlier wave already drained, while a newer manager
// wave is active. The stale call carries no hold and must not seal that wave:
// pre-fix it re-applied GoalChanged and ManagerFinished, which left the newer
// wave looking finished.
func TestStaleReleaseCompletionHoldDoesNotSealNewerWave(t *testing.T) {
	i := &Interactive{agent: core.NewAgent(nil, "model", "", nil)}
	// A newer manager wave is already active with no completion hold
	// outstanding, as happens when completion delivery wakes the manager after
	// the holds drained.
	i.applyCoordinator(orchestration.Event{Kind: orchestration.EventManagerStarted})
	i.applyCoordinator(orchestration.Event{Kind: orchestration.EventWorkerRegistered, WorkerID: "w1", AgentID: "agent-1"})
	if i.coordinatorAcceptsUserInput() {
		t.Fatal("setup: newer manager wave is already sealed")
	}

	i.releaseCompletionDeliveryHold()

	if i.coordinatorAcceptsUserInput() {
		t.Fatal("duplicate release sealed the newer manager wave")
	}
	if !i.coordinatorHasPendingWorkers() {
		t.Fatal("duplicate release lost the newer wave's pending worker")
	}
}

// goalSampleGate pins the goal-sampling window of a completion delivery
// release without sleeps. The first CurrentGoal call signals entered and then
// waits for unblock, so a test can deterministically act while that release is
// between its hold transition and its relock.
type goalSampleGate struct {
	entered chan struct{}
	unblock chan struct{}
	// once lets tests close unblock more than once, including from a deferred
	// cleanup that runs after the test already closed it.
	once  sync.Once
	calls atomic.Int64
	next  func(call int64) *core.SessionGoal
}

func newGoalSampleGate(next func(call int64) *core.SessionGoal) *goalSampleGate {
	return &goalSampleGate{
		entered: make(chan struct{}),
		unblock: make(chan struct{}),
		next:    next,
	}
}

// unblockNow releases the sampling window exactly once, so a failure cleanup
// can let a parked release finish without a double close.
func (g *goalSampleGate) unblockNow() {
	g.once.Do(func() { close(g.unblock) })
}

// current fulfils InteractiveConfig.CurrentGoal.
func (g *goalSampleGate) current() *core.SessionGoal {
	call := g.calls.Add(1)
	if call == 1 {
		close(g.entered)
		<-g.unblock
	}
	return g.next(call)
}

// completionGateTimeout bounds every wait in the completion-hold gate tests so
// an inverted lock order fails the test instead of hanging the package.
const completionGateTimeout = 5 * time.Second

// startGateRelease runs a potentially blocking hold, release, or coordinator
// transition in its own goroutine and returns a channel closed when it
// returns, so a test can bound the wait instead of hanging.
func startGateRelease(release func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		release()
	}()
	return done
}

// awaitGate waits for done to close, failing the test after
// completionGateTimeout rather than blocking forever.
func awaitGate(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(completionGateTimeout):
		t.Fatalf("%s did not finish within %s", what, completionGateTimeout)
	}
}

// enteredGate waits until a parked release has provably reached goal sampling.
func enteredGate(t *testing.T, gate *goalSampleGate, what string) {
	t.Helper()
	select {
	case <-gate.entered:
	case <-time.After(completionGateTimeout):
		gate.unblockNow()
		t.Fatalf("%s never sampled goal state", what)
	}
}

// TestCancelledCoordinatorIsNotSealedByStaleRelease covers cancellation
// invalidating an in-flight release. The release drained an older hold and is
// sampling goal state when the coordinator is replaced, so its stale
// GoalChanged/ManagerFinished snapshot must not seal or wake the replacement.
func TestCancelledCoordinatorIsNotSealedByStaleRelease(t *testing.T) {
	goal := &core.SessionGoal{ID: "goal-1", Status: core.GoalActive, Objective: "keep building"}
	gate := newGoalSampleGate(func(int64) *core.SessionGoal { return goal })
	defer gate.unblockNow()

	// A manager wake reaches goal sampling only through PersistGoalRuntime, so
	// recording that callback detects an unintended seal. Returning an error
	// stops the continuation before it touches the provider.
	woke := make(chan struct{}, 1)
	i := &Interactive{agent: core.NewAgent(nil, "model", "", goalToolRegistry())}
	i.cfg.CurrentGoal = gate.current
	i.cfg.PersistGoalRuntime = func(*core.SessionGoal) error {
		select {
		case woke <- struct{}{}:
		default:
		}
		return errors.New("stop after wake")
	}

	// The snapshot belongs to a wave that still owns a worker, so sealing it
	// would wake the manager.
	i.applyCoordinator(orchestration.Event{Kind: orchestration.EventManagerStarted})
	i.applyCoordinator(orchestration.Event{Kind: orchestration.EventWorkerRegistered, WorkerID: "w1", AgentID: "agent-1"})
	if !i.coordinatorHasPendingWorkers() {
		t.Fatal("setup: outstanding wave has no pending worker")
	}

	releaseOld := i.beginCompletionDeliveryHold()
	released := startGateRelease(releaseOld)
	enteredGate(t, gate, "older release")

	awaitGate(t, startGateRelease(i.cancelCoordinator), "cancelCoordinator")

	gate.unblockNow()
	awaitGate(t, released, "older release")

	// The replacement coordinator never owned the older wave, so the stale
	// release must leave it compact and must not wake the manager.
	if i.coordinatorHasPendingWorkers() {
		t.Fatal("stale release resurrected the cancelled wave's worker")
	}
	if i.coordinatorAcceptsUserInput() {
		t.Fatal("stale release sealed the replacement coordinator")
	}
	select {
	case <-woke:
		t.Fatal("stale release woke the manager after cancellation")
	default:
	}

	// Positive control: the replacement coordinator still seals and wakes for a
	// wave that starts after cancellation, so the check above is meaningful.
	awaitGate(t, startGateRelease(func() { i.beginCompletionDeliveryHold() }), "post-cancel hold")
	awaitGate(t, startGateRelease(i.releaseCompletionDeliveryHold), "post-cancel release")
	select {
	case <-woke:
	default:
		t.Fatal("post-cancel wave did not seal and wake the manager")
	}
}

// TestImplicitCoordinatorWaveIsNotSealedByStaleRelease covers a worker
// registered outside a completion-delivery wave. That implicit wave replaces
// whatever wave an older release drained, so once the worker finishes and the
// wave is sealed with no pending work, the stale release must not fall through
// to a goal wake when it relocks.
func TestImplicitCoordinatorWaveIsNotSealedByStaleRelease(t *testing.T) {
	goal := &core.SessionGoal{ID: "goal-1", Status: core.GoalActive, Objective: "keep building"}
	gate := newGoalSampleGate(func(int64) *core.SessionGoal { return goal })
	defer gate.unblockNow()

	woke := make(chan struct{}, 1)
	i := &Interactive{agent: core.NewAgent(nil, "model", "", goalToolRegistry())}
	i.cfg.CurrentGoal = gate.current
	i.cfg.PersistGoalRuntime = func(*core.SessionGoal) error {
		select {
		case woke <- struct{}{}:
		default:
		}
		return errors.New("stop after wake")
	}

	releaseOld := i.beginCompletionDeliveryHold()
	released := startGateRelease(releaseOld)
	enteredGate(t, gate, "older release")

	// A worker registered with no hold outstanding opens the implicit
	// one-worker sealed wave.
	awaitGate(t, startGateRelease(func() { i.registerCoordinatorWorker("w1") }), "registerCoordinatorWorker")
	workerID := i.takeCoordinatorWorkerID("w1")
	if workerID == "" {
		t.Fatal("setup: implicit wave registered no worker")
	}

	// The worker finishes inside the older release's sampling window, leaving
	// the implicit wave sealed with no pending work. Its own worker wake is not
	// under test, so only the coordinator state is advanced here.
	i.applyCoordinator(orchestration.Event{Kind: orchestration.EventWorkerFinished, WorkerID: workerID})
	if i.coordinatorHasPendingWorkers() {
		t.Fatal("setup: implicit wave still has pending work")
	}

	gate.unblockNow()
	awaitGate(t, released, "older release")

	select {
	case <-woke:
		t.Fatal("stale release sealed the newer implicit coordinator wave")
	default:
	}
}

// TestReleaseCompletionHoldLeavesActiveNewerWaveUnsealed covers a newer hold
// that begins while an older release samples goal state and is still
// outstanding when that release relocks. The older release must leave the
// newer wave alone, and the newer release must still seal it.
func TestReleaseCompletionHoldLeavesActiveNewerWaveUnsealed(t *testing.T) {
	goal := &core.SessionGoal{ID: "goal-1", Status: core.GoalActive, Objective: "keep building"}
	gate := newGoalSampleGate(func(int64) *core.SessionGoal { return goal })
	defer gate.unblockNow()

	i := &Interactive{agent: core.NewAgent(nil, "model", "", goalToolRegistry())}
	i.cfg.CurrentGoal = gate.current
	// A manager wake is not the subject of this test; stop it before it starts
	// a turn.
	i.cfg.PersistGoalRuntime = func(*core.SessionGoal) error { return errors.New("stop after wake") }

	releaseOld := i.beginCompletionDeliveryHold()
	i.registerCoordinatorWorker("w1")

	released := startGateRelease(releaseOld)
	enteredGate(t, gate, "older release")

	var releaseNew func()
	awaitGate(t, startGateRelease(func() { releaseNew = i.beginCompletionDeliveryHold() }), "newer hold")
	if i.coordinatorAcceptsUserInput() {
		t.Fatal("setup: newer hold wave is already sealed")
	}
	gate.unblockNow()
	awaitGate(t, released, "older release")

	i.completionDeliveryMu.Lock()
	holds := i.completionDeliveryHolds
	i.completionDeliveryMu.Unlock()
	if holds != 1 {
		t.Fatalf("completionDeliveryHolds = %d, want 1 (newer hold still active)", holds)
	}
	if i.coordinatorAcceptsUserInput() {
		t.Fatal("older release sealed the newer wave it did not drain")
	}

	awaitGate(t, startGateRelease(releaseNew), "newer release")
	if !i.coordinatorAcceptsUserInput() {
		t.Fatal("newer release did not seal its own wave")
	}
	if !i.coordinatorHasPendingWorkers() {
		t.Fatal("newer wave lost its pending worker")
	}
}

// TestDrainedNewerWaveIsNotResealedByOlderRelease pins the wave generation
// guard. While an older release samples goal state, a newer wave both begins
// and fully drains, leaving the hold count at zero when the older release
// relocks. The count alone would let that older release re-apply its stale
// snapshot (including GoalChanged) over the wave that already sealed.
func TestDrainedNewerWaveIsNotResealedByOlderRelease(t *testing.T) {
	goal := &core.SessionGoal{ID: "goal-1", Status: core.GoalActive, Objective: "keep building"}
	gate := newGoalSampleGate(func(call int64) *core.SessionGoal {
		if call == 2 {
			// The newer wave seals with no active goal, so its own seal must not
			// wake the manager.
			return nil
		}
		return goal
	})
	defer gate.unblockNow()

	// A manager wake reaches goal sampling only through PersistGoalRuntime, so
	// recording that callback detects an unintended seal. Returning an error
	// stops the continuation before it touches the provider.
	woke := make(chan struct{}, 1)
	i := &Interactive{agent: core.NewAgent(nil, "model", "", goalToolRegistry())}
	i.cfg.CurrentGoal = gate.current
	i.cfg.PersistGoalRuntime = func(*core.SessionGoal) error {
		select {
		case woke <- struct{}{}:
		default:
		}
		return errors.New("stop after wake")
	}

	releaseOld := i.beginCompletionDeliveryHold()
	released := startGateRelease(releaseOld)
	enteredGate(t, gate, "older release")

	// A newer wave begins and fully drains inside the older release's sampling
	// window.
	awaitGate(t, startGateRelease(func() {
		releaseNew := i.beginCompletionDeliveryHold()
		releaseNew()
	}), "newer hold wave")

	i.completionDeliveryMu.Lock()
	holds := i.completionDeliveryHolds
	i.completionDeliveryMu.Unlock()
	if holds != 0 {
		t.Fatalf("completionDeliveryHolds = %d, want 0 after the newer wave drained", holds)
	}
	gate.unblockNow()
	awaitGate(t, released, "older release")

	// The older release relocked with zero holds but belongs to the previous
	// generation, so it must not seal, and must not wake the manager for, the
	// wave that the newer release already sealed.
	select {
	case <-woke:
		t.Fatal("stale release sealed a drained newer wave")
	default:
	}

	// Positive control: the same coordinator state does wake the manager when
	// the goal really is active, so the check above is meaningful.
	i.executeCoordinatorActions(i.applyCoordinator(orchestration.Event{Kind: orchestration.EventGoalChanged, GoalActive: true}))
	i.executeCoordinatorActions(i.applyCoordinator(orchestration.Event{Kind: orchestration.EventManagerFinished}))
	select {
	case <-woke:
	default:
		t.Fatal("goal-active manager wake no longer reaches goal sampling")
	}
}

func TestResidentCompletionSlidesIntoBusyParent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		compacting bool
	}{
		{name: "success"},
		{name: "failure", err: errors.New("review failed")},
		{name: "compaction", compacting: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ag := core.NewAgent(nil, "model", "", nil)
			i := &Interactive{agent: ag, busy: true, compacting: tc.compacting, runCtx: ctx}
			release := i.beginCompletionDeliveryHold()
			defer release()
			i.TrackResidentSubagent("finished", "turn-1")
			i.TrackResidentSubagent("still-running", "turn-2")
			i.ReportResidentSubagent(subagents.ResidentCompletion{ChildID: "finished", TurnID: "turn-1", Task: "review", Summary: "saved result", Err: tc.err})

			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				i.mu.Lock()
				queued := ag.PendingQueuedMessages()
				if tc.compacting {
					queued = append([]core.QueuedMessage(nil), i.queued...)
				}
				i.mu.Unlock()
				if len(queued) != 0 {
					if len(queued) != 1 || !queued[0].HostEvent || !strings.Contains(queued[0].Text, "saved result") || !strings.Contains(queued[0].Text, "[auto-subagents update]") {
						t.Fatalf("queued completions = %#v", queued)
					}
					if !strings.Contains(queued[0].Text, "Still running: still-running") {
						t.Fatalf("completion update missing active sibling: %q", queued[0].Text)
					}
					if tc.err != nil && !strings.Contains(queued[0].Text, tc.err.Error()) {
						t.Fatalf("missing failure: %q", queued[0].Text)
					}
					if i.ensureCompletionTracker().Pending() != 1 {
						t.Fatal("unfinished child was not retained")
					}
					return
				}
				select {
				case <-deadline.C:
					t.Fatal("completion did not slide in while parent and sibling were active")
				case <-tick.C:
				}
			}
		})
	}
}
