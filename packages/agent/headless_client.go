package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bnema/zut/packages/agent/daemonclient"
	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
)

// headlessCloseReason is the daemon close reason of a finished headless run.
const headlessCloseReason = "headless_done"

// headlessIO carries the process boundary of a headless client so tests can
// drive it without touching os.Stdout or the real daemon socket.
type headlessIO struct {
	stdin          *os.File
	stdout, stderr io.Writer
	env            []string
	ensure         daemonclient.EnsureOptions
}

func processHeadlessIO(version string) headlessIO {
	return headlessIO{
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, env: os.Environ(),
		ensure: daemonclient.EnsureOptions{Options: daemonclient.Options{ClientVersion: version, ClientKind: "headless", Warnings: os.Stderr}},
	}
}

// runPrintClient runs -p as a daemon client: the final assistant text on
// stdout, diagnostics on stderr. rawArgs are the CLI args forwarded to the
// daemon session (CreateSpec.Args).
func runPrintClient(ctx context.Context, args Args, rawArgs []string, version string) error {
	return runHeadlessClient(ctx, processHeadlessIO(version), ModePrint, args, rawArgs, version)
}

// runStreamClient runs --stream as a daemon client.
func runStreamClient(ctx context.Context, args Args, rawArgs []string, version string) error {
	return runHeadlessClient(ctx, processHeadlessIO(version), ModeStream, args, rawArgs, version)
}

// runJSONClient runs --json as a daemon client.
func runJSONClient(ctx context.Context, args Args, rawArgs []string, version string) error {
	return runHeadlessClient(ctx, processHeadlessIO(version), ModeJSON, args, rawArgs, version)
}

// headlessRenderer adapts one mode formatter to the client event loop.
type headlessRenderer interface {
	handle(core.AgentEvent)
	// finish runs once the session is idle and returns the run error.
	finish(turnErr error) error
}

type printClientRenderer struct {
	r           *modes.PrintRenderer
	out         io.Writer
	orchestrate bool
}

func (p *printClientRenderer) handle(ev core.AgentEvent) {
	if _, ok := ev.(core.EvUserMessage); ok && p.orchestrate {
		// Orchestrated print keeps only the last parent turn's answer.
		p.r.ResetText()
	}
	p.r.Handle(ev)
}

func (p *printClientRenderer) finish(turnErr error) error {
	if turnErr != nil {
		return turnErr
	}
	if p.orchestrate {
		_, err := fmt.Fprintln(p.out, p.r.Text())
		return err
	}
	return p.r.WriteFinal(p.out)
}

type streamClientRenderer struct{ r *modes.StreamRenderer }

func (s *streamClientRenderer) handle(ev core.AgentEvent) {
	s.r.Handle(ev)
	if _, ok := ev.(core.EvDone); ok {
		// One agent prompt (or continuation) ended: terminate its text.
		s.r.Finish()
	}
}

func (s *streamClientRenderer) finish(turnErr error) error {
	if turnErr != nil {
		return turnErr
	}
	s.r.Finish()
	return nil
}

type jsonClientRenderer struct{ w *modes.JSONWriter }

func (j *jsonClientRenderer) handle(ev core.AgentEvent) { j.w.Handle(ev) }

func (j *jsonClientRenderer) finish(turnErr error) error {
	j.w.WriteError(turnErr)
	return errors.Join(turnErr, j.w.Err())
}

func headlessPrompt(ctx context.Context, args Args, stdin *os.File) (string, error) {
	label := string(args.Mode)
	if args.Orchestrate {
		prompt := args.Prompt
		if prompt == "" && stdin != nil {
			piped, err := readStdinFileContext(ctx, stdin)
			if err != nil {
				return "", fmt.Errorf("read %s prompt from stdin: %w", label, err)
			}
			prompt = strings.TrimSpace(piped)
		}
		if prompt == "" {
			return "", fmt.Errorf("%s mode requires a prompt (arg or stdin)", label)
		}
		return prompt, nil
	}
	var piped string
	if stdin != nil {
		piped, _ = readAllPipe(stdin)
	}
	prompt := combinePromptInput(piped, args.Prompt)
	if prompt == "" {
		return "", fmt.Errorf("%s mode requires a prompt (arg or stdin)", label)
	}
	return prompt, nil
}

func readStdinFileContext(ctx context.Context, f *os.File) (string, error) {
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, err := readAllPipe(f)
		done <- result{text, err}
	}()
	select {
	case r := <-done:
		return r.text, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// headlessSession is one created, controlled daemon session.
type headlessSession struct {
	client *daemonclient.Client
	id     session.ID
	port   *daemonclient.RemotePort
	info   session.Info
}

// openHeadlessSession ensures the daemon, creates a headless session for
// args and attaches to it as the controlling client. Headless sessions are
// created with NoYolo off: they have no prompt to answer confirmations.
func openHeadlessSession(ctx context.Context, hio headlessIO, args Args, rawArgs []string) (*headlessSession, error) {
	client, err := daemonclient.EnsureDaemon(ctx, hio.ensure)
	if err != nil {
		return nil, err
	}
	spec := session.CreateSpec{CWD: args.CWD, Env: append([]string{}, hio.env...), Args: headlessCreateArgs(rawArgs), Headless: true, Zutfile: args.zutfile}
	created, err := client.CreateWithOutput(ctx, spec)
	if err != nil {
		_ = client.Disconnect()
		return nil, fmt.Errorf("create daemon session: %w", err)
	}
	info := created.Info
	// zutfile entry.pre shell output is shown once, to the creating client.
	if created.PreOutput != "" {
		fmt.Fprint(hio.stderr, created.PreOutput)
		if !strings.HasSuffix(created.PreOutput, "\n") {
			fmt.Fprintln(hio.stderr)
		}
	}
	id := session.ID(info.ID)
	return &headlessSession{client: client, id: id, port: client.Port(id, true), info: info}, nil
}

// headlessCreateArgs drops flags that only the client honours. --no-yolo is
// dropped so the daemon never waits on a confirmation no one can answer.
func headlessCreateArgs(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, a := range raw {
		if a == "--" {
			break
		}
		switch a {
		case "--no-yolo", "--keep-session":
			continue
		}
		out = append(out, a)
	}
	return out
}

// end closes the session unless keep is set, then disconnects.
func (h *headlessSession) end(keep bool) error {
	var err error
	if !keep {
		ctx, cancel := context.WithTimeout(context.Background(), headlessShutdownTimeout)
		err = h.client.Close(ctx, h.id, headlessCloseReason)
		cancel()
	}
	return errors.Join(err, h.client.Disconnect())
}

// abort cancels the running turn on the daemon (Ctrl+C).
func (h *headlessSession) abort() {
	ctx, cancel := context.WithTimeout(context.Background(), headlessShutdownTimeout)
	defer cancel()
	_ = h.port.Abort(ctx)
}

// headlessWaiter decides when a submitted run is complete: the session is
// idle, nothing is queued (follow-ups, resident completions) and no goal is
// active. With --orchestrate it also waits until every resident child seen
// working in this run has its completion delivered to the parent, as the
// in-process completion tracker did.
type headlessWaiter struct {
	orchestrate bool
	started     bool
	idle        bool
	queued      int
	goalActive  bool
	// pendingResidents are children seen queued/running whose completion
	// has not yet appeared in a queued or delivered completion update.
	pendingResidents map[string]bool
	turnErr          error
}

func (w *headlessWaiter) observe(ev session.Event) {
	switch ev.Kind {
	case session.EvAgent:
		w.started = true
		switch e := ev.Agent.(type) {
		case core.EvUserMessage:
			w.turnErr = nil
			w.completed(modes.MessageText(e.Message))
		case core.EvTurnStart:
			w.turnErr = nil
		case core.EvTurnEnd:
			if e.Err != nil {
				w.turnErr = e.Err
			}
		}
	case session.EvQueue:
		if q, ok := ev.Data.(session.QueueEvent); ok {
			w.queued = len(q.Queue)
			for _, item := range q.Queue {
				w.completed(item.Input.Text)
			}
		}
	case session.EvInfo:
		if i, ok := ev.Data.(session.InfoEvent); ok {
			w.apply(i.Info)
		}
	}
}

func (w *headlessWaiter) apply(info session.Info) {
	w.idle = info.Run == session.RunStateIdle
	if !w.idle {
		w.started = true
	}
	w.goalActive = info.Goal != nil && info.Goal.Status == string(core.GoalActive)
	for _, r := range info.Residents {
		if r.State == string(subagents.ResidentRunning) || r.State == string(subagents.ResidentQueued) {
			if w.pendingResidents == nil {
				w.pendingResidents = map[string]bool{}
			}
			w.pendingResidents[r.ID] = true
		}
	}
}

// completed clears residents listed in a "[auto-subagents update]" text.
func (w *headlessWaiter) completed(text string) {
	if len(w.pendingResidents) == 0 || !strings.HasPrefix(text, "[auto-subagents update]") {
		return
	}
	for _, line := range strings.Split(text, "\n") {
		id, _, ok := strings.Cut(strings.TrimPrefix(line, "- "), ":")
		if ok && strings.HasPrefix(line, "- ") {
			delete(w.pendingResidents, id)
		}
	}
}

func (w *headlessWaiter) done() bool {
	if !w.started || !w.idle || w.queued > 0 || w.goalActive {
		return false
	}
	return !w.orchestrate || len(w.pendingResidents) == 0
}

// errHeadlessSessionEnded reports a session that closed before the run ended.
var errHeadlessSessionEnded = errors.New("daemon session ended before the run completed")

// pumpHeadless feeds session events to the renderer until the run completes.
func pumpHeadless(ctx context.Context, events <-chan session.Event, w *headlessWaiter, handle func(core.AgentEvent), pending func(session.PendingRequest)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return errHeadlessSessionEnded
			}
			w.observe(ev)
			switch ev.Kind {
			case session.EvAgent:
				if ev.Agent != nil {
					handle(ev.Agent)
				}
			case session.EvPending:
				if p, ok := ev.Data.(session.PendingEvent); ok && pending != nil {
					pending(p.Pending)
				}
			case session.EvClosed:
				return errHeadlessSessionEnded
			case session.EvAttach:
				if a, ok := ev.Data.(session.AttachEvent); ok && a.State == session.AttachStateDetached && a.Reason != "" {
					return fmt.Errorf("daemon session detached: %s", a.Reason)
				}
			}
			// Only an Info event states that the session is idle: the queue
			// empties just before its next item starts, so a queue event
			// alone must not end the run.
			if ev.Kind == session.EvInfo && w.done() {
				return nil
			}
		}
	}
}

// declineHeadlessPending answers requests a headless run cannot show:
// questions and rescue pickers get an empty answer (the tool or rescue then
// fails, as without an interactive terminal), confirmations are allowed
// because headless runs never ask (no-yolo has no effect).
func declineHeadlessPending(port session.Port) func(session.PendingRequest) {
	return func(p session.PendingRequest) {
		ctx, cancel := context.WithTimeout(context.Background(), headlessShutdownTimeout)
		defer cancel()
		if p.Kind == "confirm" {
			_ = port.AnswerConfirmation(ctx, p.ID, core.ConfirmDecision{Allow: true})
			return
		}
		_ = port.AnswerQuestion(ctx, p.ID, nil)
	}
}

// headlessSignalContext cancels on Ctrl+C/SIGTERM/SIGHUP. After the first
// signal, default handling is restored so a second one terminates.
func headlessSignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}

func runHeadlessClient(parent context.Context, hio headlessIO, mode Mode, args Args, rawArgs []string, version string) (runErr error) {
	ctx, stop := headlessSignalContext(parent)
	defer stop()
	if args.NoYolo {
		fmt.Fprintf(hio.stderr, "warning: --no-yolo has no effect in %s mode (no interactive prompt available); tools will run without confirmation\n", mode)
	}
	args.Mode = mode
	prompt, err := headlessPrompt(ctx, args, hio.stdin)
	if err != nil {
		return err
	}
	var r headlessRenderer
	switch mode {
	case ModePrint:
		r = &printClientRenderer{r: modes.NewPrintRenderer(hio.stderr), out: hio.stdout, orchestrate: args.Orchestrate}
	case ModeStream:
		r = &streamClientRenderer{r: modes.NewStreamRenderer(hio.stdout, hio.stderr)}
	case ModeJSON:
		r = &jsonClientRenderer{w: modes.NewJSONWriter(hio.stdout)}
	default:
		return fmt.Errorf("headless client: unsupported mode %q", mode)
	}

	h, err := openHeadlessSession(ctx, hio, args, rawArgs)
	if err != nil {
		if jr, ok := r.(*jsonClientRenderer); ok {
			jr.w.WriteError(err)
		}
		return err
	}
	defer func() { runErr = errors.Join(runErr, h.end(args.KeepSession)) }()

	subCtx, cancelSub := context.WithCancel(context.Background())
	defer cancelSub()
	events, err := h.port.Subscribe(subCtx, 0)
	if err != nil {
		return fmt.Errorf("attach daemon session: %w", err)
	}
	started := time.Now()
	if err := h.port.Submit(ctx, session.Input{Text: prompt, Origin: session.InputOriginUser}); err != nil {
		return fmt.Errorf("submit prompt: %w", err)
	}
	w := &headlessWaiter{orchestrate: args.Orchestrate}
	err = pumpHeadless(ctx, events, w, r.handle, declineHeadlessPending(h.port))
	if err != nil {
		if ctx.Err() != nil {
			// Ctrl+C: abort the daemon turn; end() then closes the session.
			h.abort()
		}
		if jr, ok := r.(*jsonClientRenderer); ok {
			jr.w.WriteError(err)
		}
		return err
	}
	if err := r.finish(w.turnErr); err != nil {
		return err
	}
	if mode == ModePrint && args.StatsPath != "" {
		info := h.port.Info()
		usage := r.(*printClientRenderer).r.Usage()
		return writePrintStats(args.StatsPath, info.Provider, info.Model, usage, time.Since(started))
	}
	return nil
}
