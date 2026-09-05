package modes

import (
	"context"
	"fmt"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const codexUsageInterval = 5 * time.Minute

// State is protected by i.mu. Each request has
// its own buffered result channel, so canceled/account-switched requests can
// finish without blocking or publishing into the new account's display.
type codexUsageState struct {
	next   time.Time
	usage  *provider.CodexWeeklyUsage
	result <-chan *provider.CodexWeeklyUsage
	cancel context.CancelFunc
}

type codexUsageClient struct {
	provider.Client
	fetch func(context.Context) (*provider.CodexWeeklyUsage, error)
}

// BindCodexWeeklyUsageFetcher attaches the optional meter to a private candidate
// before publication. Preparing or discarding it cannot affect the live meter.
// The host supplies nil for API keys, custom endpoints, and other providers.
func BindCodexWeeklyUsageFetcher(ag *core.Agent, fetch func(context.Context) (*provider.CodexWeeklyUsage, error)) {
	if fetch != nil {
		ag.Client = &codexUsageClient{Client: ag.Client, fetch: fetch}
	}
}

func (i *Interactive) codexUsageFetcherLocked() func(context.Context) (*provider.CodexWeeklyUsage, error) {
	if i.agent != nil && i.cfg.Provider == "openai-codex" {
		if client, ok := i.agent.Client.(*codexUsageClient); ok {
			return client.fetch
		}
	}
	return nil
}

func (i *Interactive) resetCodexUsage() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.resetCodexUsageLocked()
}

func (i *Interactive) resetCodexUsageLocked() {
	if i.codexUsage.cancel != nil {
		i.codexUsage.cancel()
	}
	i.codexUsage = codexUsageState{}
}

func (i *Interactive) refreshCodexUsage(ctx context.Context, now time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	fetch := i.codexUsageFetcherLocked()
	if fetch == nil || ctx.Err() != nil {
		i.resetCodexUsageLocked()
		return
	}
	state := &i.codexUsage
	select {
	case usage := <-state.result:
		state.usage = usage
		state.result = nil
		state.cancel()
		state.cancel = nil
		i.invalidate()
	default:
	}
	if state.result != nil || now.Before(state.next) {
		return
	}
	state.next = now.Add(codexUsageInterval)
	state.usage = nil
	i.invalidate()
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	state.cancel = cancel
	result := make(chan *provider.CodexWeeklyUsage, 1)
	state.result = result
	go func() {
		defer cancel()
		usage, err := fetch(requestCtx)
		if err != nil {
			// Background usage is best-effort; never interrupt a conversation
			// or expose credentials/account payloads in the status error area.
			usage = nil
		}
		result <- usage
	}()
}

func (i *Interactive) codexWeeklyLabelLocked(now time.Time) string {
	if i.codexUsageFetcherLocked() == nil {
		return ""
	}
	usage := i.codexUsage.usage
	if usage == nil ||
		!now.Before(i.codexUsage.next) ||
		(!usage.ResetsAt.IsZero() && !now.Before(usage.ResetsAt)) {
		return "weekly:?"
	}
	return fmt.Sprintf("weekly:%.0f%%", usage.RemainingPercent)
}
