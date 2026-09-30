package modes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func awaitModelRefresh(t *testing.T, i *Interactive) modelRefreshResult {
	t.Helper()
	select {
	case result := <-i.modelRefresh:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("refresh result never delivered")
	}
	return modelRefreshResult{}
}

func TestModelPickerRefreshesCustomDiscoveryWithoutLlama(t *testing.T) {
	calls := make(chan struct{}, 2)
	i := &Interactive{
		cfg: InteractiveConfig{
			CustomDiscoveryConfigured: func() bool { return true },
			RefreshCustomProviderModels: func(ctx context.Context) error {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("custom refresh has no deadline")
				}
				calls <- struct{}{}
				return nil
			},
			RefreshLlamaCPPModels: func(context.Context) error { t.Error("llama refreshed while unconfigured"); return nil },
		},
		modelRefresh: make(chan modelRefreshResult, 1),
		modelDialog:  newModelDialog(),
	}

	i.runSlash(context.Background(), "/model")
	if i.modelDialog.Active() || i.statusOK != "Refreshing models" || !i.modelRefreshing {
		t.Fatalf("picker opened early or status wrong: active=%v status=%q", i.modelDialog.Active(), i.statusOK)
	}
	result := awaitModelRefresh(t, i)
	<-calls
	i.openModelPickerAfterRefresh(result.err)
	if !i.modelDialog.Active() {
		t.Fatal("picker did not open after refresh")
	}
}

func TestModelPickerCustomDiscoveryOptOutOpensImmediately(t *testing.T) {
	i := &Interactive{
		cfg: InteractiveConfig{
			CustomDiscoveryConfigured:   func() bool { return false },
			RefreshCustomProviderModels: func(context.Context) error { t.Error("unexpected refresh"); return nil },
		},
		modelRefresh: make(chan modelRefreshResult, 1),
		modelDialog:  newModelDialog(),
	}
	i.runSlash(context.Background(), "/model")
	if !i.modelDialog.Active() || i.modelRefreshing {
		t.Fatal("picker did not open immediately without opt-in")
	}
}

func TestModelPickerRunsLlamaAndCustomIndependentlyAndJoinsErrors(t *testing.T) {
	var order []string
	llamaErr, customErr := errors.New("llama down"), errors.New("custom down")
	i := &Interactive{
		llamaConfigured: true,
		cfg: InteractiveConfig{
			RefreshLlamaCPPModels: func(ctx context.Context) error {
				order = append(order, "llama")
				return llamaErr
			},
			CustomDiscoveryConfigured: func() bool { return true },
			RefreshCustomProviderModels: func(ctx context.Context) error {
				// A failed sibling must not consume this refresh's own context.
				if ctx.Err() != nil {
					t.Errorf("custom refresh context already done: %v", ctx.Err())
				}
				order = append(order, "custom")
				return customErr
			},
		},
		modelRefresh: make(chan modelRefreshResult, 1),
		modelDialog:  newModelDialog(),
	}
	i.runSlash(context.Background(), "/model")
	result := awaitModelRefresh(t, i)
	if strings.Join(order, ",") != "llama,custom" {
		t.Fatalf("refresh order = %v", order)
	}
	if !errors.Is(result.err, llamaErr) || !errors.Is(result.err, customErr) {
		t.Fatalf("errors not joined: %v", result.err)
	}
	i.openModelPickerAfterRefresh(result.err)
	if !i.modelDialog.Active() {
		t.Fatal("picker unusable after refresh errors")
	}
	if !strings.HasPrefix(i.statusErr, "model refresh: ") || !strings.Contains(i.statusErr, "llama down") || !strings.Contains(i.statusErr, "custom down") {
		t.Fatalf("status = %q", i.statusErr)
	}
}

func TestModelPickerRefreshDoesNotLeakWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	i := &Interactive{
		cfg: InteractiveConfig{
			CustomDiscoveryConfigured: func() bool { return true },
			RefreshCustomProviderModels: func(ctx context.Context) error {
				cancel()
				close(done)
				return nil
			},
		},
		// Unbuffered and never read: delivery must give up on ctx.Done.
		modelRefresh: make(chan modelRefreshResult),
		modelDialog:  newModelDialog(),
	}
	i.runSlash(ctx, "/model")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh not invoked")
	}
	// The goroutine must exit without a receiver; a leaked sender would keep
	// this send from ever being received by a fresh reader.
	select {
	case r := <-i.modelRefresh:
		t.Fatalf("result delivered after cancellation: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
}
