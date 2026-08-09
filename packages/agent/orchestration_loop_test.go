package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/agent/subagents"
)

type fakeCompletionBatches struct {
	batches [][]subagents.Completion
	errors  []error
	waits   int
}

type endlessCompletionBatches struct{ waits int }

func (f *endlessCompletionBatches) WaitIdle(context.Context) ([]subagents.Completion, error) {
	f.waits++
	return []subagents.Completion{{AgentID: "worker", Status: "completed"}}, nil
}

func (f *fakeCompletionBatches) WaitIdle(context.Context) ([]subagents.Completion, error) {
	index := f.waits
	f.waits++
	if index < len(f.errors) && f.errors[index] != nil {
		return nil, f.errors[index]
	}
	if index >= len(f.batches) {
		return nil, nil
	}
	return f.batches[index], nil
}

func TestRunHeadlessContinuationNoWorkerPath(t *testing.T) {
	tracker := &fakeCompletionBatches{batches: [][]subagents.Completion{{}}}
	called := false
	got, err := runHeadlessContinuation(context.Background(), tracker, "initial synthesis", func(context.Context, string) (string, error) {
		called = true
		return "unexpected", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if called || got != "initial synthesis" {
		t.Fatalf("no-worker result = %q, called=%v", got, called)
	}
}

func TestRunHeadlessContinuationBatchesFailuresAndSecondWave(t *testing.T) {
	tracker := &fakeCompletionBatches{batches: [][]subagents.Completion{
		{{AgentID: "worker-a", Status: "completed", Task: "inspect"}, {AgentID: "worker-b", Status: "failed", Task: "test", Error: "provider failed"}},
		{{AgentID: "worker-c", Status: "completed", Task: "dependent"}},
		{},
	}}
	var prompts []string
	got, err := runHeadlessContinuation(context.Background(), tracker, "initial", func(_ context.Context, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		return "synthesis " + string(rune('0'+len(prompts))), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "synthesis 2" || len(prompts) != 2 {
		t.Fatalf("result=%q prompts=%d, want second-wave synthesis", got, len(prompts))
	}
	if !strings.Contains(prompts[0], "worker-b") || !strings.Contains(prompts[0], "provider failed") {
		t.Fatalf("first completion batch omitted failed-worker evidence: %q", prompts[0])
	}
	if !strings.Contains(prompts[1], "worker-c") || strings.Contains(prompts[1], "worker-a") {
		t.Fatalf("second completion batch was not isolated: %q", prompts[1])
	}
	if tracker.waits != 3 {
		t.Fatalf("wait count=%d, want completion batches followed by empty terminal batch", tracker.waits)
	}
}

func TestRunHeadlessContinuationStopsAtWaveLimit(t *testing.T) {
	tracker := &endlessCompletionBatches{}
	calls := 0
	got, err := runHeadlessContinuation(context.Background(), tracker, "initial", func(context.Context, string) (string, error) {
		calls++
		return "synthesis", nil
	})
	if err == nil || !strings.Contains(err.Error(), "maximum of 32 completion waves") {
		t.Fatalf("error = %v, want wave-limit error", err)
	}
	if got != "synthesis" || calls != maxHeadlessCompletionWaves || tracker.waits != maxHeadlessCompletionWaves+1 {
		t.Fatalf("result=%q calls=%d waits=%d", got, calls, tracker.waits)
	}
}

func TestRunHeadlessContinuationCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	want := errors.New("canceled")
	tracker := &fakeCompletionBatches{errors: []error{want}}
	got, err := runHeadlessContinuation(ctx, tracker, "initial", func(context.Context, string) (string, error) {
		t.Fatal("parent should not run after cancellation")
		return "", nil
	})
	var waitErr *headlessCompletionWaitError
	if !errors.Is(err, want) || !errors.As(err, &waitErr) || got != "initial" {
		t.Fatalf("result=(%q,%v), wait error=%v, want initial and cancellation wait error", got, err, waitErr)
	}
}

func TestRunHeadlessContinuationParentErrorDoesNotRetry(t *testing.T) {
	tracker := &fakeCompletionBatches{batches: [][]subagents.Completion{{{AgentID: "worker", Status: "completed"}}}}
	want := errors.New("parent failed")
	calls := 0
	got, err := runHeadlessContinuation(context.Background(), tracker, "initial", func(context.Context, string) (string, error) {
		calls++
		return "partial", want
	})
	if !errors.Is(err, want) || got != "partial" || calls != 1 {
		t.Fatalf("result=(%q,%v), calls=%d, want one non-retried parent failure", got, err, calls)
	}
}
