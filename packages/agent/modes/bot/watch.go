package bot

import (
	"context"
	"errors"
	"strings"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// TurnResult is one finished session run (running → idle).
type TurnResult struct {
	// Origin is the input origin of the run.
	Origin session.InputOrigin
	// Text is the last assistant text of the run.
	Text string
	// Err is the last turn error, if any.
	Err error
	// Prompts are user messages appended during the run (for mirroring).
	Prompts []string
}

// WatchTurns follows port events from its current sequence and calls onTurn
// each time the session goes from running to idle. It resubscribes after a
// dropped subscription and returns when ctx ends or the session closes.
func WatchTurns(ctx context.Context, port session.Port, onTurn func(TurnResult)) error {
	snap, err := port.Snapshot(ctx)
	if err != nil {
		return err
	}
	seq := snap.Seq
	var t turnTracker
	t.running = snap.Info.Run == session.RunStateRunning
	t.origin = snap.Info.CurrentOrigin
	for {
		ch, err := port.Subscribe(ctx, seq)
		if errors.Is(err, session.ErrSeqTooOld) {
			s, serr := port.Snapshot(ctx)
			if serr != nil {
				return serr
			}
			seq = s.Seq
			continue
		}
		if err != nil {
			return err
		}
		for ev := range ch {
			seq = ev.Seq
			if ev.Kind == session.EvClosed {
				return session.ErrClosed
			}
			if r, ok := t.apply(ev); ok {
				onTurn(r)
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

type turnTracker struct {
	running bool
	origin  session.InputOrigin
	text    string
	err     error
	prompts []string
}

func (t *turnTracker) apply(ev session.Event) (TurnResult, bool) {
	switch ev.Kind {
	case session.EvAgent:
		switch e := ev.Agent.(type) {
		case core.EvAssistantMessage:
			if s := messageText(e.Message); s != "" {
				t.text = s
			}
		case core.EvUserMessage:
			if s := messageText(e.Message); s != "" {
				t.prompts = append(t.prompts, s)
			}
		case core.EvTurnEnd:
			if e.Err != nil {
				t.err = e.Err
			}
		}
	case session.EvInfo:
		info, ok := ev.Data.(session.InfoEvent)
		if !ok {
			if p, ok2 := ev.Data.(*session.InfoEvent); ok2 && p != nil {
				info, ok = *p, true
			}
		}
		if !ok {
			return TurnResult{}, false
		}
		running := info.Info.Run == session.RunStateRunning
		if running {
			if !t.running {
				t.origin = info.Info.CurrentOrigin
			}
			t.running = true
			return TurnResult{}, false
		}
		if !t.running || info.Info.Run == session.RunStateCompacting {
			return TurnResult{}, false
		}
		r := TurnResult{Origin: t.origin, Text: t.text, Err: t.err, Prompts: t.prompts}
		*t = turnTracker{}
		return r, true
	}
	return TurnResult{}, false
}

func messageText(m provider.Message) string {
	var sb strings.Builder
	for _, c := range m.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(tb.Text)
		}
	}
	return strings.TrimSpace(sb.String())
}
