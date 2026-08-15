package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEngineDispatchDueReschedulesTask(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, time.August, 15, 12, 0, 0, 0, loc)
	engine := NewEngine(func() time.Time { return now })
	created, err := engine.Add(NewTaskInput{
		SessionID: "session-1",
		Cron:      "* * * * *",
		Message:   "check status",
		Location:  loc,
	})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	var executed []Task
	now = now.Add(time.Minute)
	engine.dispatchDue(now, func(task Task) error {
		executed = append(executed, task)
		return nil
	})
	if len(executed) != 1 || executed[0].ID != created.ID {
		t.Fatalf("executed = %#v, want task %q", executed, created.ID)
	}

	tasks := engine.List()
	if len(tasks) != 1 {
		t.Fatalf("List() length = %d, want 1", len(tasks))
	}
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

	engine.dispatchDue(now.Add(time.Minute), func(Task) error { return errors.New("provider unavailable") })
	got, ok := engine.Get(task.ID)
	if !ok {
		t.Fatal("Get() did not return scheduled task")
	}
	if got.LastError != "provider unavailable" {
		t.Fatalf("LastError = %q", got.LastError)
	}
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

func TestEngineRunStopsWithContext(t *testing.T) {
	engine := NewEngine(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := engine.Run(ctx, func(Task) error { return nil }); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}
