package modes

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func newCodexUsageTestInteractive(fetch func(context.Context) (*provider.CodexWeeklyUsage, error)) *Interactive {
	return NewInteractive(InteractiveConfig{Provider: "openai-codex", AuthMethod: "oauth",
		Agent: core.NewAgent(nil, "test", "", nil), FetchCodexWeeklyUsage: fetch})
}

// Wait for the worker's channel, then make that result available to the normal
// tick path. No sleeps or real five-minute timer are needed.
func finishCodexUsageRequest(t *testing.T, i *Interactive, now time.Time) {
	t.Helper()
	var usage *provider.CodexWeeklyUsage
	select {
	case usage = <-i.codexUsage.result:
	case <-time.After(time.Second):
		t.Fatal("usage request did not finish")
	}
	ready := make(chan *provider.CodexWeeklyUsage, 1)
	ready <- usage
	i.codexUsage.result = ready
	i.refreshCodexUsage(context.Background(), now)
}

func TestCodexUsagePollIntervalAndFailure(t *testing.T) {
	now := time.Unix(2000000000, 0)
	var calls atomic.Int32
	i := newCodexUsageTestInteractive(func(ctx context.Context) (*provider.CodexWeeklyUsage, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("usage request has no deadline")
		}
		if calls.Add(1) == 2 {
			return nil, errors.New("synthetic failure")
		}
		return &provider.CodexWeeklyUsage{RemainingPercent: 69}, nil
	})
	defer i.resetCodexUsage()
	i.refreshCodexUsage(context.Background(), now)
	finishCodexUsageRequest(t, i, now)
	if got := i.codexWeeklyLabelLocked(now); got != "weekly:69%" {
		t.Fatalf("label = %q", got)
	}
	i.refreshCodexUsage(context.Background(), now.Add(5*time.Minute-time.Nanosecond))
	if calls.Load() != 1 {
		t.Fatal("polled before five minutes")
	}
	now = now.Add(5 * time.Minute)
	i.refreshCodexUsage(context.Background(), now)
	if got := i.codexWeeklyLabelLocked(now); got != "weekly:?" {
		t.Fatalf("stale usage during refresh: %q", got)
	}
	finishCodexUsageRequest(t, i, now)
	if calls.Load() != 2 || i.codexWeeklyLabelLocked(now) != "weekly:?" || i.statusErr != "" {
		t.Fatal("failed request should clear only the weekly meter")
	}
	now = now.Add(5 * time.Minute)
	i.refreshCodexUsage(context.Background(), now)
	finishCodexUsageRequest(t, i, now)
	if calls.Load() != 3 || i.codexWeeklyLabelLocked(now) != "weekly:69%" {
		t.Fatal("polling did not recover")
	}
}

func TestCodexUsageAsyncSingleFlightAndCancellation(t *testing.T) {
	started := make(chan context.Context, 2)
	i := newCodexUsageTestInteractive(func(ctx context.Context) (*provider.CodexWeeklyUsage, error) {
		started <- ctx
		<-ctx.Done()
		return &provider.CodexWeeklyUsage{RemainingPercent: 99}, nil
	})
	defer i.resetCodexUsage()
	now := time.Now()
	i.refreshCodexUsage(context.Background(), now) // must return while fetch is blocked
	var requestCtx context.Context
	select {
	case requestCtx = <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	oldResult := i.codexUsage.result
	i.refreshCodexUsage(context.Background(), now.Add(10*time.Minute))
	if i.codexUsage.result != oldResult {
		t.Fatal("started overlapping request")
	}
	i.SetCodexWeeklyUsageFetcher(i.cfg.FetchCodexWeeklyUsage) // route/account rebuild
	select {
	case <-requestCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("request not canceled")
	}
	select {
	case <-oldResult:
	case <-time.After(time.Second):
		t.Fatal("canceled worker did not exit")
	}
	if i.codexWeeklyLabelLocked(now) != "weekly:?" {
		t.Fatal("old account result was published")
	}
}

func TestCodexUsageEligibilityAndProviderSwitch(t *testing.T) {
	var calls atomic.Int32
	i := newCodexUsageTestInteractive(func(context.Context) (*provider.CodexWeeklyUsage, error) {
		calls.Add(1)
		return &provider.CodexWeeklyUsage{RemainingPercent: 0}, nil
	})
	defer i.resetCodexUsage()
	now := time.Now()
	for _, p := range []string{"openai", "anthropic", "openai-responses"} {
		i.cfg.Provider = p
		i.refreshCodexUsage(context.Background(), now)
		if i.codexWeeklyLabelLocked(now) != "" || i.codexUsage.result != nil {
			t.Fatalf("polled for %s", p)
		}
	}
	i.cfg.Provider = "openai-codex"
	fetch := i.cfg.FetchCodexWeeklyUsage
	i.SetCodexWeeklyUsageFetcher(nil) // custom/API-key route resolved by the host
	i.refreshCodexUsage(context.Background(), now)
	if calls.Load() != 0 || i.codexWeeklyLabelLocked(now) != "" {
		t.Fatal("polled disabled route")
	}
	i.SetCodexWeeklyUsageFetcher(fetch)
	i.cfg.AuthMethod = "apikey" // startup metadata from the previous provider
	i.refreshCodexUsage(context.Background(), now)
	finishCodexUsageRequest(t, i, now)
	if i.codexWeeklyLabelLocked(now) != "weekly:0%" {
		t.Fatal("switching into Codex did not fetch subscription usage")
	}
	i.agent = nil // logout
	i.refreshCodexUsage(context.Background(), now)
	if i.codexWeeklyLabelLocked(now) != "" || i.codexUsage.usage != nil {
		t.Fatal("logout retained usage")
	}
}

func TestCodexUsageMissingAndExpiredWindow(t *testing.T) {
	now := time.Now()
	i := newCodexUsageTestInteractive(func(context.Context) (*provider.CodexWeeklyUsage, error) { return nil, nil })
	defer i.resetCodexUsage()
	i.refreshCodexUsage(context.Background(), now)
	finishCodexUsageRequest(t, i, now)
	if i.codexWeeklyLabelLocked(now) != "weekly:?" {
		t.Fatal("missing weekly window presented as zero")
	}
	i.codexUsage.usage = &provider.CodexWeeklyUsage{RemainingPercent: 69, ResetsAt: now.Add(time.Minute)}
	if i.codexWeeklyLabelLocked(now.Add(time.Minute)) != "weekly:?" {
		t.Fatal("expired window still visible")
	}
	i.codexUsage.usage.ResetsAt = time.Time{}
	if i.codexWeeklyLabelLocked(now.Add(codexUsageInterval)) != "weekly:?" {
		t.Fatal("stale sample still visible")
	}
}
