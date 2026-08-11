// Package orchestration owns the deterministic policy between manager turns
// and delegated worker lifecycles. It contains no goroutines, timers, or
// provider work; presentation modes translate asynchronous callbacks into
// events and execute the returned actions.
package orchestration

import "github.com/bnema/zut/packages/agent/subagents"

// Coordinator collects one sealed worker wave per manager turn. A terminal
// worker cannot wake the manager until the owning turn is sealed. Queued user
// input wins the next wake; an active goal is used only when no user input or
// worker result remains.
type Coordinator struct {
	managerActive bool
	waveSealed    bool
	stopped       bool
	goalActive    bool
	workers       map[string]*worker
	workerOrder   []string
	queuedUser    []string
}

type worker struct {
	terminal   bool
	completion subagents.Completion
}

type EventKind uint8

const (
	EventManagerStarted EventKind = iota + 1
	EventManagerFinished
	EventWorkerRegistered
	EventWorkerFinished
	EventUserInput
	EventGoalChanged
	EventCancelled
	EventShutdown
)

type Event struct {
	Kind       EventKind
	WorkerID   string
	Completion subagents.Completion
	Text       string
	GoalActive bool
}

type ActionKind uint8

const (
	ActionRunManager ActionKind = iota + 1
	ActionWait
	ActionStop
)

type WakeReason uint8

const (
	WakeUser WakeReason = iota + 1
	WakeWorkers
	WakeGoal
)

type Action struct {
	Kind        ActionKind
	Reason      WakeReason
	Text        string
	Completions []subagents.Completion
}

type Result struct{ Actions []Action }

func New() *Coordinator {
	return &Coordinator{workers: make(map[string]*worker)}
}

// AcceptsUserInput reports whether a sealed worker wave is awaiting its next
// manager turn. Ordinary interactive turns continue to use their existing
// queueing path; this only gives user input priority over a worker wake.
func (c *Coordinator) AcceptsUserInput() bool {
	return c != nil && !c.stopped && !c.managerActive && c.waveSealed && len(c.workerOrder) != 0
}

func (c *Coordinator) Apply(event Event) Result {
	if c == nil || c.stopped {
		return Result{}
	}
	switch event.Kind {
	case EventManagerStarted:
		c.managerActive = true
		c.waveSealed = false
		c.workers = make(map[string]*worker)
		c.workerOrder = nil
	case EventManagerFinished:
		c.managerActive = false
		c.waveSealed = true
		if c.hasPendingWorkers() {
			return Result{Actions: []Action{{Kind: ActionWait}}}
		}
		return c.wakeIfIdle()
	case EventWorkerRegistered:
		if !c.managerActive || c.waveSealed || event.WorkerID == "" {
			return Result{}
		}
		if _, exists := c.workers[event.WorkerID]; !exists {
			c.workers[event.WorkerID] = &worker{}
			c.workerOrder = append(c.workerOrder, event.WorkerID)
		}
	case EventWorkerFinished:
		w, exists := c.workers[event.WorkerID]
		if !exists || w.terminal {
			return Result{}
		}
		w.terminal = true
		w.completion = event.Completion
		if !c.managerActive && c.waveSealed && !c.hasPendingWorkers() {
			return c.wakeIfIdle()
		}
	case EventUserInput:
		if event.Text == "" {
			return Result{}
		}
		c.queuedUser = append(c.queuedUser, event.Text)
		return c.wakeIfIdle()
	case EventGoalChanged:
		c.goalActive = event.GoalActive
	case EventCancelled, EventShutdown:
		c.stopped = true
		c.queuedUser = nil
		return Result{Actions: []Action{{Kind: ActionStop}}}
	}
	return Result{}
}

func (c *Coordinator) wakeIfIdle() Result {
	if c.managerActive || c.stopped || c.hasPendingWorkers() {
		return Result{}
	}
	completions := c.completedWorkers()
	if len(c.queuedUser) != 0 {
		text := c.queuedUser[0]
		c.queuedUser = c.queuedUser[1:]
		return c.startManager(WakeUser, text, completions)
	}
	if len(completions) != 0 {
		return c.startManager(WakeWorkers, "", completions)
	}
	if c.goalActive {
		return c.startManager(WakeGoal, "", nil)
	}
	return Result{}
}

func (c *Coordinator) startManager(reason WakeReason, text string, completions []subagents.Completion) Result {
	c.managerActive = true
	return Result{Actions: []Action{{Kind: ActionRunManager, Reason: reason, Text: text, Completions: completions}}}
}

func (c *Coordinator) hasPendingWorkers() bool {
	for _, worker := range c.workers {
		if !worker.terminal {
			return true
		}
	}
	return false
}

func (c *Coordinator) completedWorkers() []subagents.Completion {
	if c.hasPendingWorkers() {
		return nil
	}
	completions := make([]subagents.Completion, 0, len(c.workerOrder))
	for _, workerID := range c.workerOrder {
		if worker := c.workers[workerID]; worker != nil && worker.terminal {
			completions = append(completions, worker.completion)
		}
	}
	return completions
}
