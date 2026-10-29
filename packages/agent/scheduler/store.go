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
	return nil
}

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
	e.mu.Lock()
	defer e.mu.Unlock()
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
	old := e.tasks
	e.tasks = restored
	if err := e.persistLocked(); err != nil {
		e.tasks = old
		return err
	}
	return nil
}

// Err reports the most recent persistence failure (nil after a successful save).
// Cancel retains the task and returns false on save failure. Completion keeps
// its updated in-memory state, and Run returns the failure to its host.
func (e *Engine) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.persistErr
}

func (e *Engine) persistLocked() error {
	if e.store == nil {
		return nil
	}
	tasks := make([]Task, 0, len(e.tasks))
	for _, task := range e.tasks {
		tasks = append(tasks, task)
	}
	sortTasks(tasks)
	e.persistErr = e.store.Save(tasks)
	return e.persistErr
}

// CancelSession removes all tasks owned by a closing session, atomically. A
// failed save leaves all tasks in place and returns an error to the host.
func (e *Engine) CancelSession(sessionID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	removed := make(map[string]Task)
	for id, task := range e.tasks {
		if task.SessionID == sessionID {
			removed[id] = task
			delete(e.tasks, id)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	if err := e.persistLocked(); err != nil {
		for id, task := range removed {
			e.tasks[id] = task
		}
		return err
	}
	e.signalWake()
	return nil
}
