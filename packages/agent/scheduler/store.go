package scheduler

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Store persists complete task snapshots. The engine serializes calls. Save
// must atomically replace the snapshot or leave the previous snapshot intact.
// Errors after replacement must wrap CommittedError so memory is not rolled back.
type Store interface {
	Load() ([]Task, error)
	Save([]Task) error
}

// FileStore stores schedules in a private JSON file. Path is normally
// $ZUT_HOME/daemon/schedules.json; the host supplies it explicitly so scheduling
// does not depend on process-global environment or working directory.
type FileStore struct{ Path string }

func (s FileStore) Load() ([]Task, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read schedules: %w", err)
	}
	var tasks []Task
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, fmt.Errorf("decode schedules: %w", err)
	}
	return tasks, nil
}

func (s FileStore) Save(tasks []Task) error {
	data, err := json.Marshal(tasks)
	if err != nil {
		return fmt.Errorf("encode schedules: %w", err)
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create schedules directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".schedules-*")
	if err != nil {
		return fmt.Errorf("create schedules file: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), s.Path)
	}
	if err != nil {
		return fmt.Errorf("save schedules: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return &CommittedError{Err: fmt.Errorf("sync schedules directory: %w", err)}
	}
	return nil
}

// CommittedError means replacement succeeded but durability could not be confirmed.
// The engine retains the new snapshot and reports the underlying error.
type CommittedError struct{ Err error }

func (e *CommittedError) Error() string { return e.Err.Error() }
func (e *CommittedError) Unwrap() error { return e.Err }

// NewPersistentEngine restores schedules with deadlines strictly after now.
// No queued/running state survives restart and missed occurrences are skipped.
func NewPersistentEngine(now func() time.Time, store Store) (*Engine, error) {
	if store == nil {
		return nil, fmt.Errorf("schedule store is required")
	}
	e := NewEngine(now)
	e.store = store
	if err := e.Load(); err != nil {
		return nil, err
	}
	return e, nil
}

// Load replaces tasks from the store. Call only before starting the engine.
// Named IANA zones retain DST rules. Custom fixed zones retain the offset
// captured in TimezoneOffset, since time.Location is not JSON serializable.
func (e *Engine) Load() error {
	e.persistMu.Lock()
	defer e.persistMu.Unlock()
	if e.store == nil {
		return nil
	}
	tasks, err := e.store.Load()
	if err != nil {
		return err
	}
	restored := make(map[string]Task, len(tasks))
	now := e.now()
	for _, task := range tasks {
		if task.ID == "" || task.SessionID == "" || task.Message == "" {
			return fmt.Errorf("invalid persisted schedule")
		}
		if _, exists := restored[task.ID]; exists {
			return fmt.Errorf("duplicate persisted schedule ID")
		}
		loc, err := time.LoadLocation(task.Timezone)
		if err != nil {
			loc = time.FixedZone(task.Timezone, task.TimezoneOffset)
		}
		task.schedule, err = ParseCron(task.Cron, loc)
		if err != nil {
			return fmt.Errorf("restore schedule: %w", err)
		}
		task.NextRun = task.schedule.Next(now)
		if task.NextRun.IsZero() {
			return fmt.Errorf("restored cron has no occurrence in the next eight years")
		}
		task.queued, task.running = false, false
		restored[task.ID] = task
	}
	return e.saveSnapshot(restored, false)
}

// Err reports the most recent persistence failure (nil after a successful save).
// Cancel returns false on save failure, retaining the task unless the error
// indicates replacement already committed. Completion keeps its updated
// in-memory state and Run retries the save on its next wake; Run never stops
// because of a persistence failure.
func (e *Engine) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.persistErr
}

// update serializes candidate snapshots without exposing them until Save returns.
// Dispatch also holds persistMu, so it cannot claim a task during a transaction.
func (e *Engine) update(keepOnError bool, change func(map[string]Task)) error {
	e.persistMu.Lock()
	defer e.persistMu.Unlock()
	e.mu.Lock()
	candidate := make(map[string]Task, len(e.tasks))
	for id, task := range e.tasks {
		candidate[id] = task
	}
	change(candidate)
	e.mu.Unlock()
	return e.saveSnapshot(candidate, keepOnError)
}

// saveSnapshot requires persistMu, but never holds mu during store I/O.
func (e *Engine) saveSnapshot(candidate map[string]Task, keepOnError bool) error {
	err := e.persistSnapshot(candidate, keepOnError)
	// Centralized wake covers failed Add/Cancel as well as completion and Load.
	e.signalWake()
	return err
}

// persistSnapshot saves and commits candidate without waking Run.
func (e *Engine) persistSnapshot(candidate map[string]Task, keepOnError bool) error {
	var err error
	if e.store != nil {
		tasks := make([]Task, 0, len(candidate))
		for _, task := range candidate {
			tasks = append(tasks, task)
		}
		sortTasks(tasks)
		err = e.store.Save(tasks)
	}
	var committed *CommittedError
	e.mu.Lock()
	durable := err == nil || errors.As(err, &committed)
	if durable || keepOnError {
		e.tasks = candidate
	}
	if durable {
		// Any replaced snapshot derives from e.tasks, so it carries prior
		// unsaved completion state too.
		e.unsaved = false
	} else if keepOnError {
		e.unsaved = true
	}
	e.persistErr = err
	e.mu.Unlock()
	return err
}

// CancelSession removes every task for a closing session. Already running
// callbacks are not interrupted; their completion cannot resurrect removed tasks.
func (e *Engine) CancelSession(sessionID string) error {
	return e.update(false, func(tasks map[string]Task) {
		for id, task := range tasks {
			if task.SessionID == sessionID {
				delete(tasks, id)
			}
		}
	})
}
