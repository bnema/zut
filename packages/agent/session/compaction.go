package session

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/bnema/zut/packages/core"
)

type compactState struct {
	busy     bool
	skipOnce bool
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
	if c.deps.AutoCompactThreshold != nil {
		switch *c.deps.AutoCompactThreshold {
		case 0, 70, 80, 85, 90, 95:
			threshold = *c.deps.AutoCompactThreshold
		}
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
func (c *Controller) beginCompact(next *Input) error {
	ag := c.turnSeam.currentAgent()
	if ag == nil {
		return errors.New("agent unavailable")
	}
	c.compact.busy = true
	c.publish(EvCompaction, CompactionEvent{Phase: "start"})
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
		tail := 4
		if n := ag.MessageCount(); tail >= n {
			tail = n - 1
		}
		summary, err := ag.Compact(ctx, tail, nil)
		if err != nil && ctx.Err() == nil {
			summary, err = c.rescueCompact(ctx, ag, tail, err)
		}
		_ = c.call(context.Background(), func() {
			c.compact.busy = false
			if err != nil {
				c.publish(EvCompaction, CompactionEvent{Phase: "failed"})
				c.compactNote(err)
				return
			}
			if c.deps.SessionStore != nil {
				c.deps.SessionStore.FlushSession()
			}
			c.publish(EvCompaction, CompactionEvent{Phase: "done", Summary: summary})
			c.publish(EvMessagesReplaced, MessagesReplacedEvent{})
			if next != nil {
				c.compact.skipOnce = true
				if e := c.turnSeam.startTurn(*next); e != nil {
					c.compact.skipOnce = false
					c.compactNote(e)
				}
			}
		})
	}()
	return nil
}

// rescueCompact waits outside the actor; confirm owns request registration and
// detach handling. Candidates mirror configured quick-model shortcuts because
// the baseline Deps has no model-discovery boundary.
func (c *Controller) rescueCompact(ctx context.Context, ag *core.Agent, tail int, cause error) (string, error) {
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
		return "", err
	}
	answers, err := seam.awaitPending(ctx, req)
	if err != nil {
		return "", err
	}
	if len(answers) != 1 {
		return "", errors.New("rescue requires one model")
	}
	value := answers[0].Custom
	if len(answers[0].Selected) == 1 {
		value = answers[0].Selected[0]
	}
	p, m, ok := strings.Cut(value, "/")
	if !ok || p == "" || m == "" {
		return "", errors.New("invalid rescue model")
	}
	if builder == nil {
		return "", cause
	}
	replacement, _, _, err := builder.BuildAgentForRescue(p, m)
	if err != nil {
		return "", err
	}
	if replacement == nil {
		return "", errors.New("rescue builder returned nil")
	}
	replacement.RestoreMessages(ag.Messages())
	summary, err := replacement.Compact(ctx, tail, nil)
	if err != nil {
		return "", err
	}
	ag.SetMessages(replacement.Messages())
	return summary, nil
}

// maybeGenerateTitle is best effort and runs only for the first real user input.
func (c *Controller) maybeGenerateTitle(in Input) {
	if c.settings.titleStarted || c.settings.title != "" || (in.Origin != "" && in.Origin != InputOriginUser) || strings.TrimSpace(in.Text) == "" {
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
