package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestEngineDispatchDueReschedulesTask(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, time.August, 15, 12, 0, 0, 0, loc)
	engine := NewEngine(func() time.Time { return now })
	created, err := engine.Add(NewTaskInput{SessionID: "session-1", Cron: "* * * * *", Message: "check status", Location: loc})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	done := make(chan Task, 1)
	now = now.Add(time.Minute)
	engine.dispatchDue(context.Background(), now, func(_ context.Context, task Task) error {
		done <- task
		return nil
	})
	select {
	case task := <-done:
		if task.ID != created.ID {
			t.Fatalf("executed task ID = %q, want %q", task.ID, created.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("due task was not executed")
	}

	eventually(t, func() bool {
		tasks := engine.List()
		return len(tasks) == 1 && tasks[0].RunCount == 1
	})
	tasks := engine.List()
	if want := now.Add(time.Minute); !tasks[0].NextRun.Equal(want) {
		t.Fatalf("NextRun = %v, want %v", tasks[0].NextRun, want)
	}
	if !tasks[0].LastRun.Equal(now) || tasks[0].RunCount != 1 {
		t.Fatalf("task after execution = %#v", tasks[0])
	}
}

func TestEngineRecordsExecutionFailure(t *testing.T) {
	now := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	engine := NewEngine(func() time.Time { return now })
	task, err := engine.Add(NewTaskInput{SessionID: "session-1", Cron: "* * * * *", Message: "run", Location: time.UTC})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	engine.dispatchDue(context.Background(), now.Add(time.Minute), func(context.Context, Task) error { return errors.New("provider unavailable") })
	eventually(t, func() bool {
		got, ok := engine.Get(task.ID)
		return ok && got.LastError == "provider unavailable"
	})
}

func TestEngineCancel(t *testing.T) {
	engine := NewEngine(nil)
	task, err := engine.Add(NewTaskInput{SessionID: "session-1", Cron: "@hourly", Message: "run", Location: time.UTC})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if !engine.Cancel(task.ID) {
		t.Fatal("Cancel() = false, want true")
	}
	if _, ok := engine.Get(task.ID); ok {
		t.Fatal("Get() found cancelled task")
	}
}

func TestEngineDispatchesLaterTaskWhileEarlierTaskBlocks(t *testing.T) {
	now := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	engine := NewEngine(func() time.Time { return now })
	first := testTask(t, "a", now)
	engine.tasks[first.ID] = first

	started := make(chan struct{})
	release := make(chan struct{})
	secondDone := make(chan struct{})
	engine.dispatchDue(context.Background(), now, func(_ context.Context, task Task) error {
		switch task.ID {
		case "a":
			close(started)
			<-release
		case "b":
			close(secondDone)
		}
		return nil
	})
	<-started
	if _, ok := engine.nextRun(); ok {
		t.Fatal("nextRun() considered an in-flight task due")
	}
	second := testTask(t, "b", now)
	engine.mu.Lock()
	engine.tasks[second.ID] = second
	engine.mu.Unlock()
	engine.dispatchDue(context.Background(), now, func(_ context.Context, task Task) error {
		if task.ID == "b" {
			close(secondDone)
		}
		return nil
	})
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("later due task waited for earlier blocked task")
	}
	close(release)
}

func TestEngineClaimsTaskOnceAcrossConcurrentDispatches(t *testing.T) {
	now := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	engine := NewEngine(func() time.Time { return now })
	engine.tasks["due"] = testTask(t, "due", now)

	started := make(chan struct{})
	release := make(chan struct{})
	var callsMu sync.Mutex
	calls := 0
	executor := func(context.Context, Task) error {
		callsMu.Lock()
		calls++
		callsMu.Unlock()
		close(started)
		<-release
		return nil
	}
	var dispatched sync.WaitGroup
	for range 2 {
		dispatched.Add(1)
		go func() {
			defer dispatched.Done()
			engine.dispatchDue(context.Background(), now, executor)
		}()
	}
	dispatched.Wait()
	<-started
	callsMu.Lock()
	gotCalls := calls
	callsMu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("executor calls = %d, want 1", gotCalls)
	}
	close(release)
}

func TestEngineCancelQueuedTaskBeforeCallback(t *testing.T) {
	now := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	engine := NewEngine(func() time.Time { return now })
	first := testTask(t, "a", now)
	second := testTask(t, "b", now)
	engine.tasks[first.ID] = first
	engine.tasks[second.ID] = second
	due := engine.takeDue(now)
	if len(due) != 2 {
		t.Fatalf("takeDue() returned %d tasks, want 2", len(due))
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		engine.dispatchTask(context.Background(), due[0], func(context.Context, Task) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	if !engine.Cancel(second.ID) {
		t.Fatal("Cancel(second) = false, want true")
	}
	calledSecond := false
	engine.dispatchTask(context.Background(), due[1], func(context.Context, Task) error {
		calledSecond = true
		return nil
	})
	close(release)
	wg.Wait()
	if calledSecond {
		t.Fatal("cancelled queued task invoked its executor")
	}
}

func TestEngineRunSkipsDueTasksWhenContextCancelled(t *testing.T) {
	now := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	engine := NewEngine(func() time.Time { return now })
	engine.tasks["due"] = testTask(t, "due", now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := engine.Run(ctx, func(context.Context, Task) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if called {
		t.Fatal("Run() invoked executor after cancellation")
	}
}

func testTask(t *testing.T, id string, now time.Time) Task {
	t.Helper()
	schedule, err := ParseCron("* * * * *", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return Task{ID: id, SessionID: id, Cron: "* * * * *", Message: id, NextRun: now, schedule: schedule}
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true before deadline")
}
