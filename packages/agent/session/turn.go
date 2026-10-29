package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// turnState owns the agent until assembly/settings replaces it through the
// existing builder boundary. Deps has no OnAssistant callback: transcript
// persistence uses SessionStore.FlushSession after assistant messages and turns.
type turnState struct {
	agent      *core.Agent
	busy       bool
	cancel     context.CancelFunc
	live       LiveTurn
	queue      []QueuedInput
	next       uint64
	projection turnInfoProjection
	end        core.EvTurnEnd
	// input is the fresh prompt of the running turn, kept for rescue retries.
	input *Input
	// rescuing holds the queue while a failed turn waits for a rescue model.
	rescuing  bool
	rescueEnd core.EvTurnEnd
}

func (s *turnState) snapshotInto(out *Snapshot) {
	out.Live = cloneTurn(s.live)
	out.Queue = append([]QueuedInput(nil), s.queue...)
	if s.agent != nil {
		out.Messages = s.agent.Messages()
	}
}
func cloneTurn(in LiveTurn) LiveTurn {
	in.Tools = append([]LiveTool(nil), in.Tools...)
	for i := range in.Tools {
		in.Tools[i].Args = append([]byte(nil), in.Tools[i].Args...)
		if in.Tools[i].Result != nil {
			r := *in.Tools[i].Result
			in.Tools[i].Result = &r
		}
	}
	return in
}

// Submit starts idle work, queues busy work, or steers the running agent.
// The request context governs command delivery only, never the agent lifetime.
//
// Compaction counts as busy: input is queued and runs once it ends. When idle
// with held queued input, the new input joins the queue and the queue drains.
//
// Input.Hidden messages are text-only: they become hidden user context through
// core.Agent.AppendUserContext, so non-text blocks are skipped with an error
// note. Steering cannot carry hidden context, so a busy steer with Hidden set
// is queued instead.
func (c *Controller) Submit(ctx context.Context, in Input) error {
	var result error
	err := c.call(ctx, func() {
		if in.Origin == "" {
			in.Origin = InputOrigin("user")
		}
		if in.Origin == "user" {
			c.turn.projection.LastUserInputAt = c.deps.Clock()
		}
		if c.isBusy() {
			if in.Mode == "steer" && c.turn.busy && c.turn.agent != nil && len(in.Hidden) == 0 {
				c.turn.agent.QueuePrompt(core.QueuedMessage{Text: in.Text, Images: in.Images, HostEvent: in.HostEvent})
			} else {
				c.enqueue(in)
			}
		} else if len(c.turn.queue) > 0 {
			// Held input (e.g. after an aborted rescue) runs first, in order.
			c.enqueue(in)
			c.drainQueued()
		} else {
			result = c.startTurn(in)
		}
	})
	if err != nil {
		return err
	}
	return result
}

// Abort cancels an in-flight compaction, or else the active turn and its
// pending interactions.
func (c *Controller) Abort(ctx context.Context) error {
	return c.call(ctx, func() {
		if c.compactSeam.compacting() {
			c.compactSeam.abortCompaction()
			return
		}
		c.cancelTurn()
	})
}

// CancelQueued removes one queued input by its published identifier.
func (c *Controller) CancelQueued(ctx context.Context, id string) error {
	var result error
	err := c.call(ctx, func() {
		result = ErrUnknownRequest
		for i, q := range c.turn.queue {
			if q.ID == id {
				c.turn.queue = append(c.turn.queue[:i], c.turn.queue[i+1:]...)
				c.publishQueue()
				result = nil
				break
			}
		}
	})
	if err != nil {
		return err
	}
	return result
}

// SlideBackQueued returns the newest queued input to the editor.
func (c *Controller) SlideBackQueued(ctx context.Context) (Input, error) {
	var in Input
	var result error
	err := c.call(ctx, func() {
		n := len(c.turn.queue)
		if n == 0 {
			result = ErrUnknownRequest
			return
		}
		in = c.turn.queue[n-1].Input
		c.turn.queue = c.turn.queue[:n-1]
		c.publishQueue()
	})
	if err != nil {
		return Input{}, err
	}
	return in, result
}
func (c *Controller) publishQueue() {
	c.publish(EvQueue, QueueEvent{Queue: append([]QueuedInput(nil), c.turn.queue...)})
}
func (c *Controller) publishTurnInfo() { c.updateInfo(); c.publish(EvInfo, InfoEvent{Info: c.Info()}) }
func (c *Controller) publishError(text string) {
	c.publish(EvNote, NoteEvent{Note: Note{Level: "error", Text: text}})
}
func (c *Controller) startTurn(in Input) error { return c.runTurn(in, false) }

func (c *Controller) runTurn(in Input, continuing bool) error {
	if c.turn.busy {
		return ErrBusy
	}
	if c.compactSeam.compacting() {
		if continuing {
			return ErrBusy
		}
		// Compaction owns the transcript; the input runs once it ends.
		c.enqueue(in)
		return nil
	}
	// Baseline compacts only before fresh input, never between agent-queued messages.
	if !continuing && c.compactSeam.maybeAutoCompact(in) {
		return nil
	}
	if err := c.ensureAgent(); err != nil {
		return err
	}
	if !continuing {
		c.goalSeam.goalBeforeTurn(&in)
		// Mirror the baseline goal reassessment path: append text as user context
		// with the hidden-message metadata before Prompt adds the visible input.
		for _, message := range in.Hidden {
			var text strings.Builder
			for _, content := range message.Content {
				if block, ok := content.(provider.TextBlock); ok {
					text.WriteString(block.Text)
				} else {
					c.publishError(fmt.Sprintf("hidden message: skipped non-text %T block", content))
				}
			}
			c.turn.agent.AppendUserContext(text.String(), message.Meta)
		}
	}
	ag := c.turn.agent
	if !continuing {
		ag.ResetRepetitionGuard()
		if transform := c.deps.FirstPromptTransform; transform != nil && !in.HostEvent && in.Origin == InputOriginUser {
			c.deps.FirstPromptTransform = nil
			in.Text = transform(in.Text)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.turn.cancel = cancel
	c.turn.busy = true
	c.turn.live = LiveTurn{Origin: in.Origin}
	c.turn.end = core.EvTurnEnd{}
	c.turn.input = nil
	if !continuing {
		retry := in
		retry.Hidden = nil
		c.turn.input = &retry
	}
	c.turn.projection.ContextMax = ag.ContextWindow
	c.turn.projection.Activity = Activity{Kind: "waiting_model", Since: c.deps.Clock()}
	c.publishTurnInfo()
	turnDone := make(chan struct{})
	c.turnDone = turnDone
	go func() {
		defer close(turnDone)
		// Controller.Done is daemon-owned; client disconnect cannot cancel a turn.
		finished := make(chan struct{})
		go func() {
			select {
			case <-c.Done():
				cancel()
			case <-finished:
			}
		}()
		callbacks := c.newTurnCallbacks()
		callbacks.push(func() { c.residentSeam.residentsRunStarted() })
		sink := func(ev core.AgentEvent) { callbacks.pushEvent(ev) }
		var err error
		if continuing {
			err = ag.Continue(ctx, sink)
		} else if in.HostEvent {
			ag.QueuePrompt(core.QueuedMessage{Text: in.Text, Images: in.Images, HostEvent: true})
			err = ag.Continue(ctx, sink)
		} else {
			err = ag.Prompt(ctx, in.Text, in.Images, sink)
		}
		callbacks.push(func() { c.residentSeam.residentsRunEnded(); c.finishTurn(ctx, err); close(callbacks.finished) })
		close(finished)
	}()
	return nil
}
func (c *Controller) finishTurn(ctx context.Context, err error) {
	origin := c.turn.live.Origin
	failed := c.turn.input
	c.turn.input = nil
	// The final assistant message lives in Messages; idle has no live turn.
	c.turn.live = LiveTurn{}
	c.turn.busy = false
	c.turn.cancel = nil
	c.turn.projection.Activity = Activity{Kind: "idle", Since: c.deps.Clock()}
	if c.deps.SessionStore != nil {
		c.deps.SessionStore.FlushSession()
	}
	// Match interactive_turn: repetition stops autonomous work but preserves
	// explicit prompts; ordinary errors preserve host reports without running
	// them immediately; cancellation discards both. Scheduled work is separate.
	repetitive := errors.Is(err, core.ErrRepetitiveLoop)
	if repetitive || ctx.Err() != nil || err != nil {
		pending := c.turn.agent.DrainQueuedMessages()
		host := c.turn.queue
		c.turn.queue = nil
		var explicit []core.QueuedMessage
		for _, message := range pending {
			if repetitive && !message.HostEvent {
				explicit = append(explicit, message)
			} else if ctx.Err() == nil && message.HostEvent {
				c.turn.agent.QueuePrompt(message)
			}
		}
		for _, q := range host {
			switch {
			case q.Input.Origin == "scheduler":
				if !repetitive {
					c.turn.queue = append(c.turn.queue, q)
				}
			case repetitive:
				if q.Input.HostEvent {
					c.turn.agent.QueuePrompt(core.QueuedMessage{Text: q.Input.Text, Images: q.Input.Images, HostEvent: true})
				} else {
					c.turn.queue = append(c.turn.queue, q)
				}
			case ctx.Err() == nil && q.Input.HostEvent:
				c.turn.agent.QueuePrompt(core.QueuedMessage{Text: q.Input.Text, Images: q.Input.Images, HostEvent: true})
			}
		}
		// Baseline order: surviving queued items first, then explicit prompts
		// typed during the repetitive loop, with fresh identifiers.
		for _, message := range explicit {
			c.turn.next++
			c.turn.queue = append(c.turn.queue, QueuedInput{ID: fmt.Sprint(c.turn.next), Input: Input{Text: message.Text, Images: message.Images, Origin: "user"}})
		}
		c.publishQueue()
	}
	c.publishTurnInfo()
	// Baseline offers a model rescue for recoverable provider failures and
	// retries the prompt; it is not a terminal goal error. rescueTurn waits off
	// the actor; remaining queued input runs after the retried turn.
	if err != nil && ctx.Err() == nil && failed != nil && rescueEligible(err) {
		c.turn.rescuing = true
		c.turn.rescueEnd = c.turn.end
		c.turn.rescueEnd.Err = err
		// Clients see the session busy while the rescue holds the queue.
		c.publishTurnInfo()
		c.compactSeam.rescueTurn(err, *failed)
		return
	}
	if len(c.turn.queue) > 0 {
		c.startQueued()
		return
	}
	if err == nil && ctx.Err() == nil && c.turn.agent.QueuedMessageCount() > 0 {
		e := c.runTurn(Input{Origin: origin}, true)
		if e == nil {
			return
		}
		c.publishError(e.Error())
	}
	end := c.turn.end
	if err != nil {
		end.Err = err
	}
	c.goalSeam.goalAfterTurn(end)
}

// startQueued starts the first non-scheduler queued input, or the first item.
func (c *Controller) startQueued() {
	index := 0
	for i, q := range c.turn.queue {
		if q.Input.Origin != "scheduler" {
			index = i
			break
		}
	}
	next := c.turn.queue[index]
	c.turn.queue = append(c.turn.queue[:index], c.turn.queue[index+1:]...)
	c.publishQueue()
	if e := c.startTurn(next.Input); e != nil {
		// Restore the item at its position so a failed start never loses input.
		c.turn.queue = append(c.turn.queue[:index], append([]QueuedInput{next}, c.turn.queue[index:]...)...)
		c.publishQueue()
		c.publishError(e.Error())
	}
}

// drainQueued runs on the actor. It is a no-op while busy (turn running or
// compaction active) and when the queue is empty.
func (c *Controller) drainQueued() {
	if c.isBusy() || len(c.turn.queue) == 0 {
		return
	}
	c.startQueued()
}

// enqueueFront puts in at the head of the queue with a fresh identifier.
func (c *Controller) enqueueFront(in Input) {
	c.turn.next++
	c.turn.queue = append([]QueuedInput{{ID: fmt.Sprint(c.turn.next), Input: in}}, c.turn.queue...)
	c.publishQueue()
}
func (c *Controller) enqueue(in Input) {
	c.turn.next++
	c.turn.queue = append(c.turn.queue, QueuedInput{ID: fmt.Sprint(c.turn.next), Input: in})
	c.publishQueue()
}
func (c *Controller) tool(id string) *LiveTool {
	for i := range c.turn.live.Tools {
		if c.turn.live.Tools[i].ID == id {
			return &c.turn.live.Tools[i]
		}
	}
	c.turn.live.Tools = append(c.turn.live.Tools, LiveTool{ID: id})
	return &c.turn.live.Tools[len(c.turn.live.Tools)-1]
}
func (c *Controller) applyAgentEvent(ev core.AgentEvent) {
	before := c.turn.projection
	switch e := ev.(type) {
	case core.EvTurnEnd:
		c.turn.end = e
	case core.EvTurnStart:
		c.turn.live.TurnID = e.TurnID
	case core.EvAssistantStart:
		c.turn.live.AssistantText = ""
	case core.EvTextDelta:
		c.turn.live.AssistantText += e.Delta
		if c.turn.projection.Activity.Kind != "streaming" {
			c.turn.projection.Activity = Activity{Kind: "streaming", Since: c.deps.Clock()}
		}
	case core.EvToolUseStart:
		t := c.tool(e.ID)
		t.Name = e.Name
		t.State = "composing"
	case core.EvToolUseArgs:
		t := c.tool(e.ID)
		t.Args = append(t.Args, e.Delta...)
	case core.EvToolUseEnd:
		c.tool(e.ID).State = "pending"
	case core.EvToolCall:
		t := c.tool(e.ID)
		t.Name = e.Name
		t.Args = append([]byte(nil), e.Args...)
		t.State = "pending"
	case core.EvToolExecutionStarted:
		t := c.tool(e.ID)
		t.Name = e.Name
		t.State = "running"
		c.turn.projection.Activity = Activity{Kind: "tool", Tool: e.Name, Since: c.deps.Clock()}
	case core.EvToolProgress:
		c.tool(e.ID).Progress += e.Text
	case core.EvToolResult:
		t := c.tool(e.ID)
		r := e.Result
		t.Result = &r
		t.State = "done"
		if r.IsError {
			t.State = "error"
		}
	case core.EvUsage:
		c.turn.projection.Usage = c.turn.projection.Usage.Add(e.Usage)
		c.turn.projection.ContextUsed = e.Usage.PromptTokens()
	case core.EvAssistantMessage:
		if c.deps.SessionStore != nil {
			c.deps.SessionStore.FlushSession()
		}
	}
	c.goal.observe(ev)
	c.compact.observe(ev)
	c.res.observe(ev)
	c.publishAgent(ev)
	if projectionChanged(before, c.turn.projection) {
		c.publishTurnInfo()
	}
}

// projectionChanged ignores activity timestamps so streaming does not flood EvInfo.
func projectionChanged(a, b turnInfoProjection) bool {
	return a.Activity.Kind != b.Activity.Kind || a.Activity.Tool != b.Activity.Tool || a.Usage != b.Usage || a.ContextUsed != b.ContextUsed || a.ContextMax != b.ContextMax
}

// isBusy reports a running turn, a pending turn rescue or an in-flight compaction.
func (c *Controller) isBusy() bool {
	return c.turn.busy || c.turn.rescuing || c.compactSeam.compacting()
}

// endRescue runs on the actor when the turn rescue completes.
func (c *Controller) endRescue() { c.turn.rescuing = false }

// rescueFailed reports the failed turn once; a retried turn reports its own end.
func (c *Controller) rescueFailed() {
	end := c.turn.rescueEnd
	c.turn.rescueEnd = core.EvTurnEnd{}
	c.goalSeam.goalAfterTurn(end)
}
func (c *Controller) turnOrigin() InputOrigin {
	if c.turn.busy {
		return c.turn.live.Origin
	}
	return ""
}
func (c *Controller) cancelTurn() {
	c.goalSeam.goalOnAbort()
	if c.turn.cancel != nil {
		c.turn.cancel()
	}
	c.confirmSeam.denyAllPending("turn cancelled")
}

// turnInfoProjection carries turn-owned activity and usage into the actor Info.
type turnInfoProjection struct {
	Activity                Activity
	Usage                   provider.Usage
	ContextUsed, ContextMax int
	LastUserInputAt         time.Time
}

func (c *Controller) turnInfo() turnInfoProjection {
	p := c.turn.projection
	if p.Activity.Kind == "" {
		p.Activity.Kind = "idle"
	}
	return p
}

// currentAgent returns the active agent, building it lazily on the actor. The
// seam cannot return an error; a failed build returns nil and can be retried.
// Turn startup uses ensureAgent directly to retain the builder's error.
func (c *Controller) currentAgent() *core.Agent {
	if c.ensureAgent() != nil {
		return nil
	}
	return c.turn.agent
}
func (c *Controller) ensureAgent() error {
	if c.turn.agent != nil {
		return nil
	}
	if c.deps.AgentBuilder == nil {
		return errors.New("session agent builder unavailable")
	}
	ag, _, _, err := c.deps.AgentBuilder.BuildAgent()
	if err != nil {
		return err
	}
	if ag == nil {
		return errors.New("session agent builder returned nil")
	}
	c.turn.agent = ag
	return nil
}

// setAgent changes the actor-owned agent only when execution has stopped.
// A nil agent clears the cache so the next access rebuilds through AgentBuilder.
// Compaction may adopt a rescue agent, so only a running turn blocks it.
func (c *Controller) setAgent(ag *core.Agent) error {
	if c.turn.busy {
		return ErrBusy
	}
	c.turn.agent = ag
	return nil
}

// turnCallbacks serializes run callbacks without waiting on the actor. In
// particular, a reentrant sink called by actor-owned hooks cannot deadlock.
// One worker preserves sink order and the final run boundary in the same FIFO
// and applies each pending batch in a single actor call. The sink never blocks;
// past callbackMergeThreshold entries, consecutive text or tool-progress deltas
// for the same target are merged so the backlog stays bounded without loss.
type turnCallbacks struct {
	mu       sync.Mutex
	pending  []turnCallback
	wake     chan struct{}
	finished chan struct{}
}

// turnCallback holds either an agent event or an actor function.
type turnCallback struct {
	ev core.AgentEvent
	fn func()
}

const callbackMergeThreshold = 256

func (c *Controller) newTurnCallbacks() *turnCallbacks {
	q := &turnCallbacks{wake: make(chan struct{}, 1), finished: make(chan struct{})}
	go func() {
		for {
			select {
			case <-c.Done():
				return
			case <-q.finished:
				return
			case <-q.wake:
				for {
					q.mu.Lock()
					batch := q.pending
					q.pending = nil
					q.mu.Unlock()
					if len(batch) == 0 {
						break
					}
					if c.call(context.Background(), func() {
						for _, item := range batch {
							if item.fn != nil {
								item.fn()
							} else {
								c.applyAgentEvent(item.ev)
							}
						}
					}) != nil {
						return
					}
				}
			}
		}
	}()
	return q
}
func (q *turnCallbacks) push(fn func()) { q.add(turnCallback{fn: fn}) }
func (q *turnCallbacks) pushEvent(ev core.AgentEvent) {
	q.add(turnCallback{ev: ev})
}
func (q *turnCallbacks) add(item turnCallback) {
	q.mu.Lock()
	if n := len(q.pending); n >= callbackMergeThreshold && item.ev != nil {
		last := &q.pending[n-1]
		switch e := item.ev.(type) {
		case core.EvTextDelta:
			if prev, ok := last.ev.(core.EvTextDelta); ok {
				prev.Delta += e.Delta
				last.ev = prev
				item = turnCallback{}
			}
		case core.EvToolProgress:
			if prev, ok := last.ev.(core.EvToolProgress); ok && prev.ID == e.ID {
				prev.Text += e.Text
				last.ev = prev
				item = turnCallback{}
			}
		}
	}
	if item.ev != nil || item.fn != nil {
		q.pending = append(q.pending, item)
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// rescueEligible mirrors modes.classifyRescueError: network, auth, permission,
// rate-limit and provider-unavailable failures can be fixed by switching
// models; context overflow has its own compaction path.
func rescueEligible(err error) bool {
	if err == nil || provider.IsContextOverflowError(err) {
		return false
	}
	low := strings.ToLower(err.Error())
	for _, needle := range []string{
		"timeout", "deadline exceeded", "connection refused", "connection reset", "no such host", "tls handshake", "eof",
		"http 401", " 401:", "invalid_authentication", "token expired", "api key appears to be invalid",
		"http 403", " 403:", "permission denied", "forbidden",
		"http 429", " 429:", "rate limit", "rate_limit", "too many requests", "quota",
		"http 500", "http 502", "http 503", "http 504", " 500:", " 502:", " 503:", " 504:", "upstream connect error", "service unavailable", "internal server error", "bad gateway", "gateway timeout",
	} {
		if strings.Contains(low, needle) {
			return true
		}
	}
	return false
}
