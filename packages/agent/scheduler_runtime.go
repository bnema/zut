package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/bnema/zut/packages/agent/extensions"
	"github.com/bnema/zut/packages/agent/scheduler"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// runScheduledSession reconstructs an inactive session in the current zut
// process and appends the resulting turn to its existing transcript. The
// caller serializes this with session transitions before invoking it.
func runScheduledSession(ctx context.Context, task scheduler.Task, args Args, base Resolved, extMgr *extensions.Manager, prepareRegistry func(core.Registry) core.Registry) error {
	path, err := core.FindManagedSessionByID(ctx, ZutHome(), task.SessionID)
	if err != nil {
		return fmt.Errorf("find scheduled session %q: %w", task.SessionID, err)
	}
	if path == "" {
		return fmt.Errorf("scheduled session %q no longer exists", task.SessionID)
	}
	sess, messages, err := core.OpenSession(path)
	if err != nil {
		return fmt.Errorf("open scheduled session: %w", err)
	}
	defer sess.Close()

	providerName := strings.TrimSpace(sess.Meta.Provider)
	if providerName == "" {
		providerName = base.Provider
	}
	model := strings.TrimSpace(sess.Meta.Model)
	if model == "" {
		model = base.Model
	}
	ag, _, _, err := buildNonInteractiveSessionAgentWithRegistry(ctx, args, base, extMgr, providerName, model, prepareRegistry)
	if err != nil {
		return fmt.Errorf("build scheduled session agent: %w", err)
	}
	ag.SetMessages(trimMessagesForResume(messages, 100))
	if cumulative, lastTurn, usageErr := core.SessionUsageDetail(path); usageErr == nil {
		ag.SeedCost(cumulative)
		ag.SeedLastTurnUsage(lastTurn)
	}
	ag.SetSessionTimeContext(sess.Meta.Started, sess.Meta.Timezone, sess.Meta.TimezoneOffset)
	var appendMu sync.Mutex
	var appendErr error
	recordAppendError := func(err error) {
		if err == nil {
			return
		}
		appendMu.Lock()
		defer appendMu.Unlock()
		if appendErr == nil {
			appendErr = err
		}
	}
	ag.OnMessageAppended = func(message provider.Message) {
		recordAppendError(sess.AppendMessage(message))
	}
	ag.OnUsage = func(cumulative provider.Usage) {
		recordAppendError(sess.AppendUsage(cumulative, cumulative))
	}
	ag.OnTranscriptCompacted = func(messages []provider.Message) {
		recordAppendError(sess.AppendCompaction(messages))
	}
	if err := ag.Prompt(ctx, task.Message, nil, func(core.AgentEvent) {}); err != nil {
		return err
	}
	appendMu.Lock()
	err = appendErr
	appendMu.Unlock()
	if err != nil {
		return fmt.Errorf("append scheduled transcript: %w", err)
	}
	if err := sess.Flush(); err != nil {
		return fmt.Errorf("flush scheduled session: %w", err)
	}
	return nil
}
