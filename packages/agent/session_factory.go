package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bnema/zut/packages/agent/daemon"
	"github.com/bnema/zut/packages/agent/scheduler"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

// sessionFactory implements session.Factory for the daemon: NewSessionDeps
// builds the terminal-free resources, the factory adds the daemon-scoped
// ones (per-session scheduler view, directory, deferred store), starts the
// controller and binds the late hooks. Create, Restore (D6) and the daemon's
// clear (D9, a Create) all go through build.
type sessionFactory struct {
	version string
	home    string
	engine  *scheduler.Engine
	log     *slog.Logger
	// dir is the daemon; set after daemon.New and before Run.
	dir session.Directory

	mu       sync.Mutex
	live     map[session.ID]*factoryHandle
	stopping atomic.Bool
	wg       sync.WaitGroup
}

var _ session.Factory = (*sessionFactory)(nil)

func newSessionFactory(version, home string, engine *scheduler.Engine, log *slog.Logger) *sessionFactory {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &sessionFactory{version: version, home: home, engine: engine, log: log, live: make(map[session.ID]*factoryHandle)}
}

// Create builds a new session with a fresh random ID.
func (f *sessionFactory) Create(ctx context.Context, spec session.CreateSpec) (session.Handle, error) {
	return f.build(ctx, spec, session.Options{ID: session.NewID()})
}

// Restore reopens a persisted session idle under its previous ID.
func (f *sessionFactory) Restore(ctx context.Context, rs session.RestoreSpec) (session.Handle, error) {
	if rs.ID == "" {
		return nil, fmt.Errorf("restore: session id is required")
	}
	spec := session.CreateSpec{CWD: rs.CWD, Env: rs.Env, Args: rs.Args, ResumePath: rs.SessionPath}
	return f.build(ctx, spec, session.Options{ID: rs.ID, InterruptedTurn: rs.InterruptedTurn})
}

// build owns the whole startup; any failure releases what was created. ctx
// is the daemon's session context: it bounds startup in NewSessionDeps and
// is the controller's lifetime.
func (f *sessionFactory) build(ctx context.Context, spec session.CreateSpec, opts session.Options) (session.Handle, error) {
	deps, cleanup, err := NewSessionDeps(ctx, spec, f.version)
	if err != nil {
		return nil, err
	}
	store, err := session.NewFileDeferredStore(f.home, opts.ID)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("open deferred store: %w", err)
	}
	deps.DeferredStore = store
	deps.Directory = f.dir
	if f.engine != nil {
		deps.Scheduler = sessionSchedule{engine: f.engine, id: string(opts.ID)}
		if scheduleToolAllowed(spec.Args) {
			tool := &tools.ScheduleTool{Engine: f.engine, SessionID: func() string { return string(opts.ID) }, Location: func() *time.Location { return time.Local }}
			deps.AgentBuilder = scheduleAgentBuilder{AgentBuilder: deps.AgentBuilder, tool: tool, last: new(atomic.Pointer[core.Agent])}
			deps.ToolRefresher = scheduleToolRefresher{ToolRefresher: deps.ToolRefresher, builder: deps.AgentBuilder.(scheduleAgentBuilder)}
		}
	}
	ctrl := session.NewControllerWithOptions(ctx, deps, opts)
	h := &factoryHandle{Controller: ctrl, f: f, id: opts.ID, cleanup: cleanup, done: make(chan struct{})}
	confirmer, asker := ctrl.HookBindings()
	if err := BindSessionHooks(deps, ctrl.ExtensionHooks(), confirmer, asker); err != nil {
		_ = ctrl.Close(context.Background(), "bind_failed")
		<-ctrl.Done()
		cleanup()
		return nil, err
	}
	f.mu.Lock()
	f.live[opts.ID] = h
	f.mu.Unlock()
	f.wg.Add(1)
	go h.watch()
	return h, nil
}

func (f *sessionFactory) lookup(id session.ID) *factoryHandle {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live[id]
}

// runScheduler routes due tasks to their session until ctx ends. It must
// start after the daemon restored sessions so restored targets exist.
func (f *sessionFactory) runScheduler(ctx context.Context) error {
	if f.engine == nil {
		return nil
	}
	return f.engine.Run(ctx, f.executeTask)
}

// executeTask submits a due task to its session as queued scheduler input.
// A task for an unknown session is cancelled, except while the daemon stops
// (sessions closing for restart keep their schedules, D6).
func (f *sessionFactory) executeTask(ctx context.Context, task scheduler.Task) error {
	h := f.lookup(session.ID(task.SessionID))
	if h == nil {
		if !f.stopping.Load() && ctx.Err() == nil && !f.registered(ctx, task.SessionID) {
			f.engine.Cancel(task.ID)
		}
		return nil
	}
	return h.Submit(ctx, session.Input{Text: task.Message, Mode: session.SubmitModeQueue, Origin: session.InputOriginScheduler})
}

// registered reports whether the daemon still lists id. A client-requested
// daemon_stop closes sessions while keeping them registered, so this guards
// that path in addition to beginStop.
func (f *sessionFactory) registered(ctx context.Context, id string) bool {
	if f.dir == nil {
		return false
	}
	for _, info := range f.dir.List(ctx) {
		if info.ID == id {
			return true
		}
	}
	return false
}

// beginStop marks the daemon as stopping: from now on a due task whose
// session is gone keeps its schedule (sessions closing for a restart are
// restored with their schedules, D6).
func (f *sessionFactory) beginStop() { f.stopping.Store(true) }

// wait blocks until every session watcher has run its cleanup.
func (f *sessionFactory) wait() { f.wg.Wait() }

// factoryHandle is the controller plus its assembly cleanup. Done closes
// only after cleanup ran, so the daemon never sees a session gone while its
// resources are still held.
type factoryHandle struct {
	*session.Controller
	f       *sessionFactory
	id      session.ID
	cleanup func()
	reason  atomic.Value // string
	done    chan struct{}
}

// Close records the reason (deciding whether schedules survive) and closes
// the controller. With an actor context (Controller.OnActor) it only requests
// the close; otherwise it waits for cleanup.
func (h *factoryHandle) Close(ctx context.Context, reason string) error {
	h.reason.CompareAndSwap(nil, reason)
	if err := h.Controller.Close(ctx, reason); err != nil {
		return err
	}
	if h.Controller.OnActor(ctx) {
		return nil
	}
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done closes once the controller stopped and cleanup completed.
func (h *factoryHandle) Done() <-chan struct{} { return h.done }

func (h *factoryHandle) watch() {
	defer h.f.wg.Done()
	defer close(h.done)
	<-h.Controller.Done()
	h.f.mu.Lock()
	if h.f.live[h.id] == h {
		delete(h.f.live, h.id)
	}
	h.f.mu.Unlock()
	h.cleanup()
	reason, _ := h.reason.Load().(string)
	// A daemon stop or a cancelled daemon context keeps the session open in
	// state.json; only an explicit close drops its schedules.
	if h.f.engine != nil && reason != "" && reason != daemon.ReasonDaemonStop {
		if err := h.f.engine.CancelSession(string(h.id)); err != nil {
			h.f.log.Warn("cancel session schedules", "session", h.id, "err", err)
		}
	}
}

// scheduleToolAllowed mirrors runInteractive's schedulerAllowed rule.
func scheduleToolAllowed(raw []string) bool {
	args, err := ParseArgs(raw)
	if err != nil {
		return false
	}
	return !args.NoTools && (!args.ToolsSet && len(args.Tools) == 0 || toolListContains(args.Tools, tools.ScheduleToolName))
}

// scheduleAgentBuilder installs the session-scoped schedule tool on every
// agent the assembly builds (the daemon owns the engine, not the assembly).
type scheduleAgentBuilder struct {
	session.AgentBuilder
	tool *tools.ScheduleTool
	// last is the most recent agent, re-equipped after tool refreshes.
	last *atomic.Pointer[core.Agent]
}

func (b scheduleAgentBuilder) install(ag *core.Agent, p, m string, err error) (*core.Agent, string, string, error) {
	if ag != nil {
		reg := ag.ToolsSnapshot()
		reg[tools.ScheduleToolName] = b.tool
		ag.SetTools(reg)
		b.last.Store(ag)
	}
	return ag, p, m, err
}

func (b scheduleAgentBuilder) BuildAgent() (*core.Agent, string, string, error) {
	return b.install(b.AgentBuilder.BuildAgent())
}

func (b scheduleAgentBuilder) BuildAgentFor(p, m string) (*core.Agent, string, string, error) {
	return b.install(b.AgentBuilder.BuildAgentFor(p, m))
}

func (b scheduleAgentBuilder) BuildAgentForRescue(p, m string) (*core.Agent, string, string, error) {
	return b.install(b.AgentBuilder.BuildAgentForRescue(p, m))
}

// scheduleToolRefresher re-adds the schedule tool after RefreshTools rebuilt
// the live agent's registry.
type scheduleToolRefresher struct {
	session.ToolRefresher
	builder scheduleAgentBuilder
}

func (r scheduleToolRefresher) RefreshTools() error {
	err := r.ToolRefresher.RefreshTools()
	if ag := r.builder.last.Load(); ag != nil {
		if _, ok := ag.ToolsSnapshot()[tools.ScheduleToolName]; !ok {
			r.builder.install(ag, "", "", nil)
		}
	}
	return err
}

// sessionSchedule is a session.ScheduleHost scoped to one session over the
// daemon's persistent engine.
type sessionSchedule struct {
	engine *scheduler.Engine
	id     string
}

func (s sessionSchedule) Add(in scheduler.NewTaskInput) (scheduler.Task, error) {
	in.SessionID = s.id
	return s.engine.Add(in)
}

func (s sessionSchedule) Cancel(id string) bool {
	task, ok := s.engine.Get(strings.TrimSpace(id))
	if !ok || task.SessionID != s.id {
		return false
	}
	return s.engine.Cancel(task.ID)
}

func (s sessionSchedule) List() []scheduler.Task {
	var out []scheduler.Task
	for _, task := range s.engine.List() {
		if task.SessionID == s.id {
			out = append(out, task)
		}
	}
	return out
}
