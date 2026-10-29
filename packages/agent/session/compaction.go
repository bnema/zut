package session

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

type compactState struct {
	busy     bool
	skipOnce bool
	// cancel and aborted belong to the in-flight compaction only.
	cancel  context.CancelFunc
	aborted bool
	// rescuing marks a compaction waiting on a rescue model answer.
	rescuing bool
}

func (s *compactState) snapshotInto(out *Snapshot) {
	if s.busy {
		out.Info.Run = RunStateCompacting
		out.Info.Activity = Activity{Kind: "compacting"}
	}
}
func (s *compactState) observe(ev core.AgentEvent) {}

// Compact reserves the idle transcript and starts a daemon-owned summary request.
// Completion is reported by EvCompaction, not by holding the client request open.
func (c *Controller) Compact(ctx context.Context) error {
	var result error
	err := c.call(ctx, func() {
		if c.turnSeam.isBusy() || c.compact.busy {
			result = ErrBusy
			return
		}
		result = c.beginCompact(nil)
	})
	if err != nil {
		return err
	}
	return result
}

// maybeAutoCompact runs on the actor before TURN appends the next prompt. A
// one-shot bypass prevents the preserved prompt from recursively compacting.
func (c *Controller) maybeAutoCompact(next Input) bool {
	if c.compact.skipOnce {
		c.compact.skipOnce = false
		return false
	}
	if c.compact.busy {
		c.turnSeam.enqueue(next)
		return true
	}
	c.maybeGenerateTitle(next)
	p := c.turnSeam.turnInfo()
	threshold := 85
	if c.deps.AutoCompactThreshold != nil && validCompactThreshold(*c.deps.AutoCompactThreshold) {
		threshold = *c.deps.AutoCompactThreshold
	}
	if threshold == 0 || p.ContextMax <= 0 || p.ContextUsed*100 < p.ContextMax*threshold {
		return false
	}
	ag := c.turnSeam.currentAgent()
	if ag == nil || ag.MessageCount() < 2 {
		return false
	}
	if err := c.beginCompact(&next); err != nil {
		c.compactNote(err)
	}
	return true
}
func (c *Controller) compactNote(err error) {
	c.publish(EvNote, NoteEvent{Note: Note{Level: "error", Text: "compaction failed: " + err.Error()}})
}

// compacting reports whether a compaction owns the transcript.
func (c *Controller) compacting() bool { return c.compact.busy }

// abortCompaction cancels the in-flight summary; completion publishes
// EvCompaction{failed} and restores the pre-compaction transcript.
func (c *Controller) abortCompaction() {
	if !c.compact.busy || c.compact.aborted {
		return
	}
	c.compact.aborted = true
	if c.compact.cancel != nil {
		c.compact.cancel()
	}
}
func (c *Controller) beginCompact(next *Input) error {
	ag := c.turnSeam.currentAgent()
	if ag == nil {
		return errors.New("agent unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.compact.busy, c.compact.aborted, c.compact.cancel = true, false, cancel
	before := ag.Messages()
	c.publish(EvCompaction, CompactionEvent{Phase: "start"})
	compactDone := make(chan struct{})
	c.compactDone = compactDone
	go func() {
		defer close(compactDone)
		defer cancel()
		go func() {
			select {
			case <-c.done:
				cancel()
			case <-ctx.Done():
			}
		}()
		target := ag
		summary, err := target.Compact(ctx, compactTail(target), nil)
		if err != nil && ctx.Err() == nil && rescueEligible(err) {
			aborted := false
			_ = c.call(context.Background(), func() {
				aborted = c.compact.aborted
				if !aborted {
					c.compact.rescuing = true
				}
			})
			var replacement *core.Agent
			if !aborted {
				cause := err
				if replacement, err = c.rescue(ctx, cause); errors.Is(err, tools.ErrQuestionsDeclined) {
					// A declined rescue is a cancel: the compaction error stands.
					err = cause
				}
			}
			if !aborted && err == nil {
				target = replacement
				summary, err = target.Compact(ctx, compactTail(target), nil)
			}
		}
		_ = c.call(context.Background(), func() {
			aborted, rescuing := c.compact.aborted, c.compact.rescuing
			c.compact.busy, c.compact.aborted, c.compact.cancel, c.compact.rescuing = false, false, nil, false
			switch {
			case aborted:
				// A summary that won the race with cancellation is rolled back.
				if err == nil {
					target.RestoreMessages(before)
				}
				c.publish(EvCompaction, CompactionEvent{Phase: "failed"})
				// The user aborted compaction, not the prompt: it runs first
				// and must not immediately re-trigger the aborted compaction.
				if next != nil {
					c.compact.skipOnce = true
					c.turnSeam.enqueueFront(*next)
				}
				// Aborting a rescue prompt means the model is unusable: hold
				// the queue for the user instead of retrying on it.
				if !rescuing {
					c.turnSeam.drainQueued()
				} else {
					c.publish(EvNote, NoteEvent{Note: Note{Level: "info", Text: "queued prompts held; they run on your next message"}})
				}
				return
			case err != nil:
				c.publish(EvCompaction, CompactionEvent{Phase: "failed"})
				c.compactNote(err)
			default:
				if c.deps.SessionStore != nil {
					c.deps.SessionStore.FlushSession()
				}
				c.publish(EvCompaction, CompactionEvent{Phase: "done", Summary: summary})
				c.publish(EvMessagesReplaced, MessagesReplacedEvent{})
			}
			// The triggering prompt survives success and failure alike.
			if next != nil {
				c.compact.skipOnce = true
				if e := c.turnSeam.startTurn(*next); e != nil {
					c.compact.skipOnce = false
					// Never lose the prompt: it runs first once startable.
					c.turnSeam.enqueueFront(*next)
					c.compactNote(e)
				}
			}
			// drainQueued starts queued input only when TURN is idle.
			c.turnSeam.drainQueued()
		})
	}()
	return nil
}
func compactTail(ag *core.Agent) int {
	tail := 4
	if n := ag.MessageCount(); tail >= n {
		tail = n - 1
	}
	return tail
}

// rescueTurn asks the controlling client for a fallback model after a failed
// turn, adopts it through TURN and retries the prompt. Called on the actor; the
// wait runs off it. Detached sessions fail with a Note.
func (c *Controller) rescueTurn(cause error, retry Input) {
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-c.done:
				cancel()
			case <-ctx.Done():
			}
		}()
		_, err := c.rescue(ctx, cause)
		_ = c.call(context.Background(), func() {
			c.turnSeam.endRescue()
			if err == nil {
				err = c.turnSeam.startTurn(retry)
			}
			if err != nil {
				// A declined rescue is a cancel: the original turn error stands.
				if !errors.Is(err, tools.ErrQuestionsDeclined) {
					c.publish(EvNote, NoteEvent{Note: Note{Level: "error", Text: "rescue failed: " + err.Error()}})
				}
				// A failed rescue must not strand input queued behind the failed
				// turn; a started queued turn reports its own end instead.
				c.turnSeam.drainQueued()
				if !c.turnSeam.isBusy() {
					c.turnSeam.rescueFailed()
				}
			}
		})
	}()
}

// rescue must run off the actor: awaitPending blocks until a client answers.
// Candidates mirror configured quick-model shortcuts because the baseline Deps
// has no model-discovery boundary. The adopted agent keeps the transcript.
func (c *Controller) rescue(ctx context.Context, cause error) (*core.Agent, error) {
	var req PendingRequest
	var seam confirmSeam
	var builder AgentBuilder
	if err := c.call(ctx, func() {
		req = PendingRequest{Kind: "rescue", Rescue: &RescueOptions{Error: cause.Error()}}
		for _, p := range c.deps.QuickModelShortcuts {
			req.Rescue.Candidates = append(req.Rescue.Candidates, p.Provider+"/"+p.Model)
		}
		seam = c.confirmSeam
		builder = c.deps.AgentBuilder
	}); err != nil {
		return nil, err
	}
	if builder == nil {
		// Without a builder no answer can be adopted; do not prompt the client.
		return nil, cause
	}
	answers, err := seam.awaitPending(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(answers) != 1 {
		return nil, errors.New("rescue requires one model")
	}
	value := answers[0].Custom
	if len(answers[0].Selected) == 1 {
		value = answers[0].Selected[0]
	}
	p, m, ok := strings.Cut(value, "/")
	if !ok || p == "" || m == "" {
		return nil, errors.New("invalid rescue model")
	}
	replacement, actualP, actualM, err := builder.BuildAgentForRescue(p, m)
	if err != nil {
		return nil, err
	}
	if replacement == nil {
		return nil, errors.New("rescue builder returned nil")
	}
	if e := c.call(ctx, func() {
		c.replaceAgent(replacement)
		if err = c.settings.replaceErr; err != nil {
			return
		}
		c.deps.Provider, c.deps.Model = actualP, actualM
		if c.deps.SessionStore != nil {
			c.deps.SessionStore.PersistModel(actualP, actualM)
		}
		c.updateInfo()
		c.publish(EvInfo, InfoEvent{Info: c.Info()})
	}); e != nil {
		return nil, e
	}
	if err != nil {
		return nil, err
	}
	return replacement, nil
}

// maybeGenerateTitle is best effort and runs only for the first real user
// input of an interactive session; headless runs never spend a provider call
// on a title.
func (c *Controller) maybeGenerateTitle(in Input) {
	if c.deps.Headless || c.settings.titleStarted || c.settings.title != "" || (in.Origin != "" && in.Origin != InputOriginUser) || strings.TrimSpace(in.Text) == "" {
		return
	}
	ag := c.turnSeam.currentAgent()
	if ag == nil {
		return
	}
	c.settings.titleStarted = true
	client, model := ag.Client, ag.Model
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		go func() {
			select {
			case <-c.done:
				cancel()
			case <-ctx.Done():
			}
		}()
		title, err := core.GenerateSessionTitle(ctx, client, model, in.Text)
		if err != nil {
			title = core.NormalizeSessionTitle(in.Text)
		}
		_ = c.call(context.Background(), func() {
			if c.settings.title == "" {
				if e := c.persistTitle(title); e != nil {
					c.publish(EvNote, NoteEvent{Note: Note{Level: "error", Text: e.Error()}})
				}
			}
		})
	}()
}
