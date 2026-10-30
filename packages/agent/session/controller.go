package session

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bnema/zut/packages/core"
)

// EventRingSize bounds retained events in addition to the 16 MiB byte limit.
const EventRingSize = 8192
const eventRingBytes = 16 << 20
const subscriberBuffer = 256

type ringEntry struct {
	event Event
	bytes int
}
type eventRing struct {
	entries []ringEntry
	bytes   int
}

func (r *eventRing) append(ev Event) {
	b, err := json.Marshal(ev)
	size := len(b)
	if err != nil {
		size = eventRingBytes + 1
	}
	r.entries = append(r.entries, ringEntry{ev, size})
	r.bytes += size
	for len(r.entries) > EventRingSize || r.bytes > eventRingBytes {
		r.bytes -= r.entries[0].bytes
		r.entries[0] = ringEntry{}
		r.entries = r.entries[1:]
	}
}
func (r *eventRing) replay(after, seq uint64) ([]Event, error) {
	if after > seq {
		// A sequence ahead of ours predates a restart; the client must resync.
		return nil, ErrSeqTooOld
	}
	if after == seq {
		return nil, nil
	}
	if len(r.entries) == 0 || after < r.entries[0].event.Seq-1 {
		return nil, ErrSeqTooOld
	}
	var out []Event
	for _, entry := range r.entries {
		if entry.event.Seq > after {
			out = append(out, entry.event)
		}
	}
	return out, nil
}

type subscription struct {
	events chan Event
	stop   chan struct{}
}

// Controller serializes session mutations on a single actor loop. Cluster
// operations are placeholders until their owning implementation lanes land.
type Controller struct {
	deps         Deps
	loop         chan func()
	done         chan struct{}
	seq          uint64
	ring         *eventRing
	subs         map[uint64]*subscription
	nextSub      uint64
	attach       AttachState
	id           ID
	created      time.Time
	info         atomic.Value
	turn         turnState
	confirm      confirmState
	ext          extState
	goal         goalState
	sched        schedState
	res          residentState
	compact      compactState
	settings     settingsState
	inter        interState
	turnSeam     turnSeam
	confirmSeam  confirmSeam
	goalSeam     goalSeam
	schedSeam    schedSeam
	residentSeam residentSeam
	compactSeam  compactSeam
	settingsSeam settingsSeam
	interSeam    interSeam
	// actorBase is the parent of every actor context handed out by Do; it
	// carries actorKey so Close and SetAttach never wait on the actor.
	actorBase context.Context
	closeReq  chan struct{}
	// turnDone closes when the latest turn goroutine returned (actor-owned).
	turnDone chan struct{}
	// compactDone closes when the latest compaction goroutine returned.
	compactDone chan struct{}
	closeOnce   sync.Once
	closeWhy    string
}

// Options configures NewControllerWithOptions.
type Options struct {
	// ID is used as-is when set;
	// otherwise a random ID unique across daemon restarts is generated.
	ID ID
}

// NewID returns a random session ID (12 lowercase base32 characters).
func NewID() ID { return ID(strings.ToLower(rand.Text()[:12])) }

// NewController starts a daemon-owned actor with a fresh random ID. Its
// context belongs to the daemon, not an attached client; cancelling it or
// calling Close stops the actor and its subscriptions.
func NewController(ctx context.Context, deps Deps) *Controller {
	return NewControllerWithOptions(ctx, deps, Options{})
}

// NewControllerWithOptions is NewController with an optional preset identity.
func NewControllerWithOptions(ctx context.Context, deps Deps, opts Options) *Controller {
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	id := opts.ID
	if id == "" {
		id = NewID()
	}
	c := &Controller{deps: deps, loop: make(chan func()), done: make(chan struct{}), closeReq: make(chan struct{}), ring: &eventRing{}, subs: make(map[uint64]*subscription), attach: AttachStateDetached, id: id, created: deps.Clock()}
	c.actorBase = context.WithValue(context.WithoutCancel(ctx), actorKey{}, c)
	c.extInit(ctx)
	c.turnSeam = c
	c.confirmSeam = c
	c.goalSeam = c
	c.schedSeam = c
	c.residentSeam = c
	c.compactSeam = c
	c.settingsSeam = c
	c.interSeam = c
	c.initGoal()
	c.residentSeam.residentsInit()
	c.initConfirm()
	c.updateInfo()
	go func() {
		defer close(c.done)
		defer c.extShutdown()
		defer func() {
			for _, s := range c.subs {
				close(s.events)
				close(s.stop)
			}
		}()
		for {
			select {
			case <-ctx.Done():
				// The daemon context ending is a daemon stop: close the
				// session properly (D3/D6) instead of just dropping it.
				c.requestClose(closeReasonDaemonStop)
				c.closeOnActor()
				return
			case <-c.closeReq:
				c.closeOnActor()
				return
			case <-c.ext.wake:
				c.extDrain()
			case fn := <-c.loop:
				c.extDrain()
				fn()
			}
		}
	}()
	return c
}
func (c *Controller) post(ctx context.Context, fn func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.done:
		return ErrClosed
	case <-c.closeReq:
		return ErrClosed
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	case <-c.closeReq:
		// The loop stops reading once close is requested; never block on it.
		return ErrClosed
	case c.loop <- fn:
		return nil
	}
}
func (c *Controller) call(ctx context.Context, fn func()) error {
	complete := make(chan struct{})
	if err := c.post(ctx, func() { fn(); close(complete) }); err != nil {
		return err
	}
	select {
	case <-complete:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	}
}

// publish is called only from the actor. An extra channel slot is reserved for
// the terminal slow-consumer event so backpressure never blocks the actor.
func (c *Controller) publish(kind EventKind, data any) {
	c.seq++
	ev := Event{Seq: c.seq, Session: c.id, At: c.deps.Clock(), Kind: kind, Data: data}
	if kind == EvAgent {
		ev.Agent, _ = data.(core.AgentEvent)
		ev.Data = nil
	}
	c.ring.append(ev)
	c.updateInfo()
	for id, s := range c.subs {
		if len(s.events) >= cap(s.events)-1 {
			terminal := Event{Seq: ev.Seq, Session: c.id, At: ev.At, Kind: EvAttach, Data: AttachEvent{State: AttachStateDetached, Reason: "slow_consumer"}}
			s.events <- terminal
			close(s.events)
			close(s.stop)
			delete(c.subs, id)
			continue
		}
		s.events <- ev
	}
}
func (c *Controller) updateInfo() {
	run := RunStateIdle
	if c.turnSeam != nil && c.turnSeam.isBusy() {
		run = RunStateRunning
	}
	compacting := c.compactSeam != nil && c.compactSeam.compacting()
	if compacting {
		run = RunStateCompacting
	}
	info := Info{ID: string(c.id), CWD: c.deps.CWD, Provider: c.deps.Provider, Model: c.deps.Model, Reasoning: c.deps.Reasoning, Attach: c.attach, Run: run, CreatedAt: c.created, UpdatedAt: c.deps.Clock(), Activity: Activity{Kind: "idle"}}
	if c.deps.FastMode != nil {
		info.FastMode = *c.deps.FastMode
	}
	if c.turnSeam != nil {
		info.CurrentOrigin = c.turnSeam.turnOrigin()
		projection := c.turnSeam.turnInfo()
		info.Activity = projection.Activity
		info.Usage = projection.Usage
		info.ContextUsed = projection.ContextUsed
		info.ContextMax = projection.ContextMax
		info.LastUserInputAt = projection.LastUserInputAt
	}
	if compacting {
		info.Activity = Activity{Kind: "compacting", Since: info.Activity.Since}
	}
	if c.schedSeam != nil {
		info.Schedules = c.schedSeam.scheduleCount()
	}
	if c.residentSeam != nil {
		info.Residents = c.residentSeam.residentSummaries()
	}
	info.Deferred = c.deferredCount()
	if c.deps.SessionStore != nil {
		info.SessionPath = c.deps.SessionStore.CurrentSessionPath()
	}
	info.Goal, info.PlanStep = c.goalProjection()
	c.info.Store(info)
}

// Info returns the latest immutable actor projection without blocking the loop.
func (c *Controller) Info() Info {
	info := c.info.Load().(Info)
	info.Residents = append([]ResidentSummary(nil), info.Residents...)
	if info.Goal != nil {
		goal := *info.Goal
		info.Goal = &goal
	}
	if info.PlanStep != nil {
		step := *info.PlanStep
		info.PlanStep = &step
	}
	return info
}

// Snapshot assembles the actor's current cluster projections at one sequence.
func (c *Controller) Snapshot(ctx context.Context) (Snapshot, error) {
	var out Snapshot
	err := c.call(ctx, func() {
		// The agent is built lazily; a resumed session holds its restored
		// transcript there, so build it before projecting messages. A failed
		// build still returns a snapshot so the client can attach (for example
		// to log in), but reports the failure instead of an empty history.
		var buildErr error
		if c.deps.AgentBuilder != nil {
			buildErr = c.ensureAgent()
		}
		c.updateInfo()
		out = Snapshot{Info: c.Info(), Seq: c.seq}
		c.turn.snapshotInto(&out)
		c.confirm.snapshotInto(&out)
		c.ext.snapshotInto(&out)
		c.goal.snapshotInto(&out)
		c.sched.snapshotInto(&out)
		c.res.snapshotInto(&out)
		c.compact.snapshotInto(&out)
		c.settings.snapshotInto(&out)
		c.inter.snapshotInto(&out)
		if buildErr != nil {
			out.Notes = append(out.Notes, Note{Level: "error", Text: "session history unavailable: " + buildErr.Error()})
		}
	})
	if err != nil {
		return Snapshot{}, err
	}
	return out, nil
}

// Subscribe atomically replays retained events then joins live publication.
// A full live queue is closed with a final detached/slow_consumer event.
func (c *Controller) Subscribe(ctx context.Context, afterSeq uint64) (<-chan Event, error) {
	var s *subscription
	var id uint64
	var replayErr error
	err := c.call(ctx, func() {
		events, e := c.ring.replay(afterSeq, c.seq)
		if e != nil {
			replayErr = e
			return
		}
		s = &subscription{events: make(chan Event, len(events)+subscriberBuffer+1), stop: make(chan struct{})}
		for _, ev := range events {
			s.events <- ev
		}
		c.nextSub++
		id = c.nextSub
		c.subs[id] = s
	})
	if err != nil {
		return nil, err
	}
	if replayErr != nil {
		return nil, replayErr
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = c.call(context.Background(), func() {
				if current, ok := c.subs[id]; ok {
					close(current.events)
					close(current.stop)
					delete(c.subs, id)
				}
			})
		case <-s.stop:
		case <-c.done:
		}
	}()
	return s.events, nil
}

// Done closes when the actor and its subscriptions have stopped.
func (c *Controller) Done() <-chan struct{} { return c.done }

// publishAgent routes core events through the shared sequence, ring and broadcaster.
func (c *Controller) publishAgent(ev core.AgentEvent) { c.publish(EvAgent, ev) }
