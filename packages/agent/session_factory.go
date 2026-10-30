package agent

import (
	"context"
	"errors"
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
// controller and binds the late hooks. Create and the daemon's
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
	var extra []core.Tool
	if f.engine != nil {
		deps.Scheduler = sessionSchedule{engine: f.engine, id: string(opts.ID)}
		if daemonToolAllowed(spec.Args, tools.ScheduleToolName) {
			extra = append(extra, &tools.ScheduleTool{Engine: f.engine, SessionID: func() string { return string(opts.ID) }, Location: func() *time.Location { return time.Local }})
		}
	}
	if f.dir != nil && daemonToolAllowed(spec.Args, tools.SessionsToolName) {
		extra = append(extra, &tools.SessionsTool{Directory: sessionDirectory{dir: f.dir, from: opts.ID}})
	}
	if len(extra) > 0 {
		builder := extraToolsAgentBuilder{AgentBuilder: deps.AgentBuilder, tools: extra, last: new(atomic.Pointer[core.Agent])}
		deps.AgentBuilder = builder
		deps.ToolRefresher = extraToolsRefresher{ToolRefresher: deps.ToolRefresher, builder: builder}
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
// start after the daemon is ready.
func (f *sessionFactory) runScheduler(ctx context.Context) error {
	if f.engine == nil {
		return nil
	}
	f.engine.OnPersistError(func(err error) { f.log.Error("scheduler save failed", "err", err) })
	return f.engine.Run(ctx, f.executeTask)
}

// executeTask submits a due task to its session as queued scheduler input.
// A task for an unknown session is cancelled, except while the daemon stops
// (shutdown does not modify saved schedules).
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
// session is gone keeps its schedule.
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
	if h.OnActor(ctx) {
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
	// Daemon shutdown preserves saved schedules without restoring sessions;
	// only an explicit close drops the session's schedules.
	if h.f.engine != nil && reason != "" && reason != daemon.ReasonDaemonStop {
		if err := h.f.engine.CancelSession(string(h.id)); err != nil {
			h.f.log.Warn("cancel session schedules", "session", h.id, "err", err)
		}
	}
}

// daemonToolAllowed mirrors runInteractive's schedulerAllowed rule for a
// daemon-scoped tool: off with --no-tools or an explicit --tools list that
// omits name.
func daemonToolAllowed(raw []string, name string) bool {
	args, err := ParseArgs(raw)
	if err != nil {
		return false
	}
	return !args.NoTools && (!args.ToolsSet && len(args.Tools) == 0 || toolListContains(args.Tools, name))
}

// extraToolsAgentBuilder installs the daemon-scoped tools (schedule,
// sessions) on every agent the assembly builds; the daemon owns their
// backends, not the assembly.
type extraToolsAgentBuilder struct {
	session.AgentBuilder
	tools []core.Tool
	// last is the most recent agent, re-equipped after tool refreshes.
	last *atomic.Pointer[core.Agent]
}

func (b extraToolsAgentBuilder) install(ag *core.Agent, p, m string, err error) (*core.Agent, string, string, error) {
	if ag != nil {
		reg := ag.ToolsSnapshot()
		for _, tool := range b.tools {
			reg[tool.Name()] = tool
		}
		ag.SetTools(reg)
		b.last.Store(ag)
	}
	return ag, p, m, err
}

func (b extraToolsAgentBuilder) BuildAgent() (*core.Agent, string, string, error) {
	return b.install(b.AgentBuilder.BuildAgent())
}

func (b extraToolsAgentBuilder) BuildAgentFor(p, m string) (*core.Agent, string, string, error) {
	return b.install(b.AgentBuilder.BuildAgentFor(p, m))
}

func (b extraToolsAgentBuilder) BuildAgentForRescue(p, m string) (*core.Agent, string, string, error) {
	return b.install(b.AgentBuilder.BuildAgentForRescue(p, m))
}

// extraToolsRefresher re-adds the daemon-scoped tools after RefreshTools
// rebuilt the live agent's registry.
type extraToolsRefresher struct {
	session.ToolRefresher
	builder extraToolsAgentBuilder
}

func (r extraToolsRefresher) RefreshTools() error {
	err := r.ToolRefresher.RefreshTools()
	if ag := r.builder.last.Load(); ag != nil {
		reg := ag.ToolsSnapshot()
		for _, tool := range r.builder.tools {
			if _, ok := reg[tool.Name()]; !ok {
				r.builder.install(ag, "", "", nil)
				break
			}
		}
	}
	return err
}

// sessionDirectory adapts the daemon directory to tools.SessionDirectory,
// bound to the sending session and mapping the D10 guard errors.
type sessionDirectory struct {
	dir  session.Directory
	from session.ID
}

func (d sessionDirectory) List(ctx context.Context) ([]tools.SessionEntry, error) {
	infos := d.dir.List(ctx)
	out := make([]tools.SessionEntry, 0, len(infos))
	for _, info := range infos {
		out = append(out, tools.SessionEntry{ID: info.ID, Title: info.Title, CWD: info.CWD, Attach: string(info.Attach), Run: string(info.Run), Activity: info.Activity.Kind, Model: info.Model})
	}
	return out, nil
}

func (d sessionDirectory) Tree(ctx context.Context) (string, error) {
	return renderSessionTree(d.dir.Tree(ctx), d.from), nil
}

func (d sessionDirectory) Send(ctx context.Context, to, text string) error {
	err := d.dir.Send(ctx, d.from, session.ID(to), text)
	switch {
	case errors.Is(err, session.ErrNotAttached):
		return tools.ErrSenderDetached
	case errors.Is(err, session.ErrLoopGuard):
		return tools.ErrSenderLoop
	}
	return err
}

// renderSessionTree renders workspace → session → plan step, goal and
// residents; self marks the calling session.
func renderSessionTree(tree session.Tree, self session.ID) string {
	var b strings.Builder
	for _, ws := range tree.Workspaces {
		fmt.Fprintf(&b, "%s\n", tools.SanitizeSessionText(ws.RepoRoot))
		for _, node := range ws.Sessions {
			info := node.Info
			title := tools.SanitizeSessionText(info.Title)
			if title == "" {
				title = "(untitled)"
			}
			mark := ""
			if session.ID(info.ID) == self {
				mark = " (this session)"
			}
			fmt.Fprintf(&b, "  %s %s [%s, %s, %s]%s\n", info.ID, title, info.Attach, info.Run, tools.SanitizeSessionText(info.Activity.Kind), mark)
			if info.PlanStep != nil {
				fmt.Fprintf(&b, "    plan step %d/%d: %s\n", info.PlanStep.Index, info.PlanStep.Total, tools.SanitizeSessionText(info.PlanStep.Text))
			}
			if info.Goal != nil {
				fmt.Fprintf(&b, "    goal (%s): %s\n", tools.SanitizeSessionText(info.Goal.Status), tools.SanitizeSessionText(info.Goal.Objective))
			}
			for _, r := range node.Residents {
				fmt.Fprintf(&b, "    resident %s %s [%s]: %s\n", tools.SanitizeSessionText(r.ID), tools.SanitizeSessionText(r.Profile), tools.SanitizeSessionText(r.State), tools.SanitizeSessionText(r.Task))
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
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
