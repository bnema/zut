package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Task is one recurring prompt owned by a session branch. Tasks are kept only
// in process memory; callers are responsible for stopping the engine with the
// host process.
type Task struct {
	ID        string
	SessionID string
	Cron      string
	Timezone  string
	Message   string
	CreatedAt time.Time
	NextRun   time.Time
	LastRun   time.Time
	RunCount  int
	LastError string

	schedule Cron
	queued   bool
	running  bool
}

// NewTaskInput describes a task to add to an Engine. Location is captured at
// creation rather than consulting time.Local during later evaluations.
type NewTaskInput struct {
	SessionID string
	Cron      string
	Message   string
	Location  *time.Location
}

// Engine owns due-task claiming through one timer loop. Executors run outside
// the lock, allowing independent session tasks to progress concurrently; the
// host remains responsible for serializing work for one session.
type Engine struct {
	mu    sync.Mutex
	now   func() time.Time
	tasks map[string]Task
	wake  chan struct{}
}

// NewEngine creates an empty scheduler. now is injectable for tests; nil uses
// time.Now.
func NewEngine(now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{now: now, tasks: make(map[string]Task), wake: make(chan struct{}, 1)}
}

// Add validates and records a new recurring task.
func (e *Engine) Add(input NewTaskInput) (Task, error) {
	if e == nil {
		return Task{}, fmt.Errorf("scheduler is not initialized")
	}
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.Cron = strings.TrimSpace(input.Cron)
	input.Message = strings.TrimSpace(input.Message)
	if input.SessionID == "" {
		return Task{}, fmt.Errorf("session ID is required")
	}
	if input.Message == "" {
		return Task{}, fmt.Errorf("scheduled message is required")
	}
	if input.Location == nil {
		return Task{}, fmt.Errorf("schedule timezone is required")
	}
	schedule, err := ParseCron(input.Cron, input.Location)
	if err != nil {
		return Task{}, err
	}
	now := e.now()
	next := schedule.Next(now)
	if next.IsZero() {
		return Task{}, fmt.Errorf("cron has no occurrence in the next eight years")
	}
	task := Task{
		ID:        uuid.NewString(),
		SessionID: input.SessionID,
		Cron:      input.Cron,
		Timezone:  input.Location.String(),
		Message:   input.Message,
		CreatedAt: now,
		NextRun:   next,
		schedule:  schedule,
	}
	e.mu.Lock()
	e.tasks[task.ID] = task
	e.mu.Unlock()
	e.signalWake()
	return task, nil
}

// Cancel removes a scheduled task. It returns false when the ID is unknown.
func (e *Engine) Cancel(id string) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	_, ok := e.tasks[id]
	if ok {
		delete(e.tasks, id)
	}
	e.mu.Unlock()
	if ok {
		e.signalWake()
	}
	return ok
}

// Get returns a task snapshot by ID.
func (e *Engine) Get(id string) (Task, bool) {
	if e == nil {
		return Task{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	task, ok := e.tasks[id]
	return task, ok
}

// List returns snapshots ordered by next execution and then ID.
func (e *Engine) List() []Task {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	tasks := make([]Task, 0, len(e.tasks))
	for _, task := range e.tasks {
		tasks = append(tasks, task)
	}
	e.mu.Unlock()
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].NextRun.Equal(tasks[j].NextRun) {
			return tasks[i].ID < tasks[j].ID
		}
		return tasks[i].NextRun.Before(tasks[j].NextRun)
	})
	return tasks
}

// Run waits for due tasks until ctx is cancelled. Each claimed task receives
// ctx and executes asynchronously; cancellation prevents new callbacks and is
// propagated to callbacks already in flight.
func (e *Engine) Run(ctx context.Context, executor func(context.Context, Task) error) error {
	if e == nil {
		return fmt.Errorf("scheduler is not initialized")
	}
	if executor == nil {
		return fmt.Errorf("scheduler executor is required")
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		now := e.now()
		e.dispatchDue(ctx, now, executor)
		next, ok := e.nextRun()
		if !ok {
			select {
			case <-ctx.Done():
				return nil
			case <-e.wake:
			}
			continue
		}
		wait := time.Until(next)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil
		case <-e.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}

// dispatchDue reserves every due task before launching an asynchronous
// dispatch. A reservation prevents concurrent Run callers from selecting the
// same task. dispatchTask claims it immediately before invoking the executor,
// so Cancel can still win while a task is waiting to begin.
func (e *Engine) dispatchDue(ctx context.Context, now time.Time, executor func(context.Context, Task) error) {
	if e == nil || executor == nil || ctx.Err() != nil {
		return
	}
	for _, task := range e.takeDue(now) {
		go e.dispatchTask(ctx, task, executor)
	}
}

func (e *Engine) takeDue(now time.Time) []Task {
	e.mu.Lock()
	defer e.mu.Unlock()
	due := make([]Task, 0)
	for id, task := range e.tasks {
		if task.queued || task.running || task.NextRun.After(now) {
			continue
		}
		task.queued = true
		e.tasks[id] = task
		due = append(due, task)
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].NextRun.Equal(due[j].NextRun) {
			return due[i].ID < due[j].ID
		}
		return due[i].NextRun.Before(due[j].NextRun)
	})
	return due
}

func (e *Engine) dispatchTask(ctx context.Context, queued Task, executor func(context.Context, Task) error) {
	if ctx.Err() != nil {
		e.releaseQueued(queued.ID, queued.NextRun)
		return
	}
	e.mu.Lock()
	task, ok := e.tasks[queued.ID]
	if !ok || !task.queued || !task.NextRun.Equal(queued.NextRun) {
		e.mu.Unlock()
		return // cancelled or superseded before it was claimed
	}
	task.queued = false
	task.running = true
	e.tasks[task.ID] = task
	e.mu.Unlock()
	if ctx.Err() != nil {
		e.finish(task.ID, task.NextRun, ctx.Err())
		return
	}
	err := executor(ctx, task)
	e.finish(task.ID, task.NextRun, err)
}

func (e *Engine) releaseQueued(id string, scheduledFor time.Time) {
	e.mu.Lock()
	task, ok := e.tasks[id]
	if !ok || !task.queued || !task.NextRun.Equal(scheduledFor) {
		e.mu.Unlock()
		return
	}
	task.queued = false
	e.tasks[id] = task
	e.mu.Unlock()
	e.signalWake()
}

func (e *Engine) finish(id string, scheduledFor time.Time, err error) {
	e.mu.Lock()
	task, ok := e.tasks[id]
	if !ok || !task.NextRun.Equal(scheduledFor) {
		e.mu.Unlock()
		return // cancelled or replaced while the executor ran
	}
	task.running = false
	task.LastRun = scheduledFor
	task.RunCount++
	if err != nil {
		task.LastError = err.Error()
	} else {
		task.LastError = ""
	}
	// Calculate from completion time, rather than repeatedly from the prior
	// deadline, so a sleeping machine or a slow task never creates a burst of
	// stale catch-up executions.
	task.NextRun = task.schedule.Next(e.now())
	if task.NextRun.IsZero() {
		delete(e.tasks, id)
	} else {
		e.tasks[id] = task
	}
	e.mu.Unlock()
	e.signalWake()
}

func (e *Engine) nextRun() (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var next time.Time
	for _, task := range e.tasks {
		if task.queued || task.running {
			continue
		}
		if next.IsZero() || task.NextRun.Before(next) {
			next = task.NextRun
		}
	}
	return next, !next.IsZero()
}

func (e *Engine) signalWake() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
