package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// controlledStore gates saves without holding engine state locks.
type controlledStore struct {
	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
	err     error
}

func (s *controlledStore) Load() ([]Task, error) { return nil, nil }
func (s *controlledStore) Save([]Task) error {
	s.mu.Lock()
	started, release, err := s.started, s.release, s.err
	s.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	return err
}
func (s *controlledStore) configure(started, release chan struct{}, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started, s.release, s.err = started, release, err
}
func scheduleInput() NewTaskInput {
	return NewTaskInput{SessionID: "a", Cron: "* * * * *", Message: "check", Location: time.UTC}
}
func awaitScheduleError(t *testing.T, done <-chan error, want error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Fatalf("error: %v, want %v", err, want)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return persistence failure")
	}
}

func TestScheduleBlockedStoreDoesNotBlockGetList(t *testing.T) {
	store := &controlledStore{}
	e, err := NewPersistentEngine(nil, store)
	if err != nil {
		t.Fatal(err)
	}
	task, err := e.Add(scheduleInput())
	if err != nil {
		t.Fatal(err)
	}
	for _, cancel := range []bool{false, true} {
		started, release := make(chan struct{}, 1), make(chan struct{})
		store.configure(started, release, nil)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if cancel {
				e.Cancel(task.ID)
			} else {
				_, _ = e.Add(scheduleInput())
			}
		}()
		<-started
		read := make(chan struct{})
		go func() {
			defer close(read)
			if _, ok := e.Get(task.ID); !ok {
				t.Error("uncommitted removal visible")
			}
			want := 1
			if cancel {
				want = 2
			}
			if got := len(e.List()); got != want {
				t.Errorf("uncommitted snapshot visible: %d, want %d", got, want)
			}
		}()
		select {
		case <-read:
		case <-time.After(time.Second):
			close(release)
			<-done
			t.Fatal("Get/List blocked on Save")
		}
		close(release)
		<-done
	}
}

func TestScheduleIdleRunReturnsFailedAddSave(t *testing.T) {
	store := &controlledStore{}
	e, err := NewPersistentEngine(nil, store)
	if err != nil {
		t.Fatal(err)
	}
	// Consume startup wake so the idle loop has no unrelated activity.
	<-e.wake
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- e.Run(ctx, func(context.Context, Task) error { t.Error("unexpected execution"); return nil })
	}()
	failure := errors.New("save failed")
	store.configure(nil, nil, failure)
	if _, err := e.Add(scheduleInput()); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	awaitScheduleError(t, done, failure)
	if len(e.List()) != 0 {
		t.Fatal("failed add committed")
	}
}

func TestScheduleCompletionSaveFailureReachesRun(t *testing.T) {
	store := &controlledStore{}
	now := time.Now()
	e, err := NewPersistentEngine(func() time.Time { return now }, store)
	if err != nil {
		t.Fatal(err)
	}
	task, err := e.Add(scheduleInput())
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	task.NextRun = now
	e.tasks[task.ID] = task
	e.mu.Unlock()
	failure := errors.New("completion save failed")
	store.configure(nil, nil, failure)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, func(context.Context, Task) error { return nil }) }()
	awaitScheduleError(t, done, failure)
	got, _ := e.Get(task.ID)
	if got.RunCount != 1 || got.running {
		t.Fatalf("completion lost: %+v", got)
	}
}

func TestScheduleCancelSessionQueuedRunning(t *testing.T) {
	now := time.Now()
	e := NewEngine(func() time.Time { return now })
	e.tasks["a"] = testTask(t, "a", now)
	e.tasks["b"] = testTask(t, "b", now)
	e.tasks["b"] = func() Task { task := e.tasks["b"]; task.SessionID = "a"; return task }()
	due := e.takeDue(now)
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		e.dispatchTask(context.Background(), due[0], func(context.Context, Task) error { close(started); <-release; return nil })
	}()
	<-started
	if err := e.CancelSession("a"); err != nil {
		t.Fatal(err)
	}
	e.dispatchTask(context.Background(), due[1], func(context.Context, Task) error { t.Error("cancelled queued task executed"); return nil })
	close(release)
	<-done
	if len(e.List()) != 0 {
		t.Fatal("running completion resurrected cancelled session")
	}
}

func TestSchedulePostRenameFailureDoesNotRollback(t *testing.T) {
	store := &controlledStore{}
	e, err := NewPersistentEngine(nil, store)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("directory sync failed")
	store.configure(nil, nil, &CommittedError{Err: failure})
	if _, err := e.Add(scheduleInput()); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	tasks := e.List()
	if len(tasks) != 1 {
		t.Fatal("committed add rolled back")
	}
	if e.Cancel(tasks[0].ID) {
		t.Fatal("sync failure not reported")
	}
	if len(e.List()) != 0 {
		t.Fatal("committed removal rolled back")
	}
}
