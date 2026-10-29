package agent

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bnema/zut/packages/agent/daemon"
	"github.com/bnema/zut/packages/agent/daemonclient"
	"github.com/bnema/zut/packages/agent/internal/flock"
	"github.com/bnema/zut/packages/agent/scheduler"
)

const (
	defaultIdleGCHours   = 24
	daemonStopGrace      = 10 * time.Second
	daemonStopWaitLimit  = 15 * time.Second
	daemonClientDeadline = 10 * time.Second
)

// runDaemonCommand handles `zut daemon run|status|stop|restart|start|install|uninstall|logs`.
func runDaemonCommand(rawArgs []string, version string) (handled bool, err error) {
	if len(rawArgs) == 0 || rawArgs[0] != "daemon" {
		return false, nil
	}
	sub := ""
	if len(rawArgs) > 1 {
		sub = rawArgs[1]
	}
	tail := []string{}
	if len(rawArgs) > 2 {
		tail = rawArgs[2:]
	}
	switch sub {
	case "", "help", "-h", "--help":
		printDaemonHelp(os.Stdout)
		return true, nil
	case "run":
		return true, daemonRun(tail, version)
	case "status":
		return true, daemonStatus(os.Stdout, version)
	case "stop":
		return true, daemonStop(os.Stdout, version)
	case "restart":
		return true, daemonRestart(os.Stdout, version)
	case "start":
		return true, daemonStart(os.Stdout, version)
	case "install":
		return true, daemonInstall(os.Stdout, tail)
	case "uninstall":
		return true, daemonUninstall(os.Stdout, tail)
	case "logs":
		return true, daemonLogsCommand(os.Stdout, tail)
	default:
		printDaemonHelp(os.Stderr)
		return true, fmt.Errorf("unknown daemon subcommand %q", sub)
	}
}

func printDaemonHelp(w io.Writer) {
	fmt.Fprint(w, `zut daemon - manage the local session daemon

Usage:
  zut daemon run [--idle-gc-hours N]  run the daemon in the foreground
  zut daemon status                   show pid, version, uptime and sessions
  zut daemon stop                     stop the daemon (sessions restore on next start)
  zut daemon restart                  stop, then start the daemon in the background
  zut daemon start                    start the daemon in the background if needed
  zut daemon install [--now]          install a systemd user unit (Linux)
  zut daemon uninstall [--now]        remove the systemd user unit (Linux)
  zut daemon logs [-f]                print (or follow) the daemon log

The socket is $XDG_RUNTIME_DIR/zut/daemon.sock (fallback $ZUT_HOME/run/daemon.sock),
overridable with ZUT_DAEMON_SOCKET. Logs: $ZUT_HOME/logs/daemon.log.
`)
}

// daemonRun is the foreground daemon: log file (+ stderr), HTTP proxy from
// config, daemon lock, persistent scheduler, session factory. SIGINT/SIGTERM
// stop it gracefully; SIGHUP is ignored. This is the only place that installs
// daemon signal handlers.
func daemonRun(args []string, version string) error {
	fs := flag.NewFlagSet("zut daemon run", flag.ContinueOnError)
	idleHours := fs.Float64("idle-gc-hours", defaultIdleGCHours, "close detached idle sessions after N hours (0 disables)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("zut daemon run: unexpected arguments %v", fs.Args())
	}
	home := ZutHome()
	logger, logCloser, err := daemon.OpenLog(home, stderrUnlessLog(home))
	if err != nil {
		return err
	}
	defer logCloser.Close()
	applyConfiguredHTTPProxy()

	signal.Ignore(syscall.SIGHUP)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = runDaemonServer(ctx, daemonServerOptions{
		Home:       home,
		SocketPath: daemon.DefaultSocketPath(home),
		Version:    version,
		IdleGC:     time.Duration(*idleHours * float64(time.Hour)),
		Logger:     logger,
	})
	if err != nil {
		logger.Error("daemon exited", "err", err)
	}
	return err
}

// stderrUnlessLog avoids writing every line twice when the daemon was
// spawned with stderr already redirected to daemon.log.
func stderrUnlessLog(home string) io.Writer {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return io.Discard
	}
	if li, err := os.Stat(daemon.LogPath(home)); err == nil && os.SameFile(fi, li) {
		return io.Discard
	}
	return os.Stderr
}

type daemonServerOptions struct {
	Home, SocketPath, Version string
	IdleGC                    time.Duration
	Logger                    *slog.Logger
	// Ready is closed once the daemon listens and restore ran.
	Ready chan<- struct{}
}

// daemonServer is the persistent scheduler, session factory and daemon of
// one `zut daemon run`.
type daemonServer struct {
	factory *sessionFactory
	daemon  *daemon.Daemon
	opts    daemon.Options
	ready   chan<- struct{}
}

func newDaemonServer(o daemonServerOptions) (*daemonServer, error) {
	engine, err := scheduler.NewPersistentEngine(time.Now, scheduler.FileStore{Path: filepath.Join(o.Home, "daemon", "schedules.json")})
	if err != nil {
		return nil, fmt.Errorf("load schedules: %w", err)
	}
	f := newSessionFactory(o.Version, o.Home, engine, o.Logger)
	d := daemon.New(f)
	f.dir = d
	opts := daemon.Options{SocketPath: o.SocketPath, Home: o.Home, IdleGC: o.IdleGC, Version: o.Version, Logger: o.Logger}
	return &daemonServer{factory: f, daemon: d, opts: opts, ready: o.Ready}, nil
}

// run serves until ctx ends or a client stops the daemon, then returns once
// the scheduler stopped and every session's cleanup ran.
func (s *daemonServer) run(ctx context.Context) error {
	ready := make(chan struct{})
	opts := s.opts
	opts.Ready = ready
	schedCtx, stopSched := context.WithCancel(context.Background())
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		select {
		case <-ready:
		case <-schedCtx.Done():
			return
		}
		if s.ready != nil {
			close(s.ready)
		}
		// Started after restore so restored sessions receive their tasks.
		if err := s.factory.runScheduler(schedCtx); err != nil {
			s.factory.log.Error("scheduler stopped", "err", err)
		}
	}()
	// A signal (ctx) is the daemon stop path owned here; a client
	// daemon_stop is covered by sessionFactory.registered, since shutdown
	// keeps sessions registered while closing them.
	stopWatch := context.AfterFunc(ctx, s.factory.beginStop)
	defer stopWatch()
	runErr := s.daemon.Run(ctx, opts)
	s.factory.beginStop()
	stopSched()
	<-schedDone
	s.factory.wait()
	return runErr
}

// stop begins a graceful daemon stop (as a signal would).
func (s *daemonServer) stop() {
	s.factory.beginStop()
	s.daemon.Stop()
}

func runDaemonServer(ctx context.Context, o daemonServerOptions) error {
	s, err := newDaemonServer(o)
	if err != nil {
		return err
	}
	return s.run(ctx)
}

// daemonClient dials the running daemon without spawning one.
func daemonClient(ctx context.Context, version string) (*daemonclient.Client, error) {
	return daemonclient.Dial(ctx, daemonclient.SocketPath(os.Getenv), daemonclient.Options{ClientVersion: version, ClientKind: "cli"})
}

func daemonStatus(w io.Writer, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), daemonClientDeadline)
	defer cancel()
	c, err := daemonClient(ctx, version)
	if err != nil {
		state, serr := probeDaemonState(err, ZutHome())
		if serr != nil {
			return serr
		}
		fmt.Fprintln(w, "zut daemon: "+state)
		return nil
	}
	defer c.Disconnect()
	st, err := c.Status(ctx)
	if err != nil {
		return fmt.Errorf("daemon status: %w", err)
	}
	fmt.Fprintf(w, "zut daemon: running\npid: %d\nversion: %s\nuptime: %s\nsessions: %d\n",
		st.PID, st.Version, (time.Duration(st.UptimeMS) * time.Millisecond).Round(time.Second), st.Sessions)
	return nil
}

// probeDaemonState classifies a failed status dial with one probe and no
// lock acquisition (status never creates daemon.lock). Only "no socket" or
// "connection refused" can mean not running; then a live process in
// daemon.pid means the daemon is starting or stopping.
func probeDaemonState(dialErr error, home string) (string, error) {
	if !daemonAbsent(dialErr) {
		return "", fmt.Errorf("zut daemon not reachable: %w", dialErr)
	}
	if pidAlive(readDaemonPID(home)) {
		return "starting or stopping", nil
	}
	return "not running", nil
}

// readDaemonPID returns the pid recorded in $ZUT_HOME/daemon/daemon.pid, or 0.
func readDaemonPID(home string) int {
	b, err := os.ReadFile(filepath.Join(home, "daemon", "daemon.pid"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// daemonLockGrace is how long an unreachable daemon may still hold its lock
// (e.g. it is starting up or shutting down) before it counts as wedged.
const daemonLockGrace = 2 * time.Second

// daemonNotRunning classifies a dial failure. Only "no socket" or "connection
// refused" can mean not running, and then only once the daemon lock is free;
// any other error, or a lock still held after daemonLockGrace, is reported as
// an unreachable daemon. It returns (true, nil) when no daemon runs.
func daemonNotRunning(dialErr error, home string) (bool, error) {
	if !daemonAbsent(dialErr) {
		return false, fmt.Errorf("zut daemon not reachable: %w", dialErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), daemonLockGrace)
	defer cancel()
	if err := waitDaemonLockFree(ctx, home); err != nil {
		return false, fmt.Errorf("zut daemon not reachable but lock held (%s): %w", filepath.Join(home, "daemon", "daemon.lock"), dialErr)
	}
	return true, nil
}

func daemonStop(w io.Writer, version string) error {
	stopped, err := stopDaemon(version)
	if err != nil {
		return err
	}
	if stopped {
		fmt.Fprintln(w, "zut daemon: stopped")
	} else {
		fmt.Fprintln(w, "zut daemon: not running")
	}
	return nil
}

// stopDaemon requests a graceful stop and waits until the daemon released
// its lock, so a following start cannot race the old process.
func stopDaemon(version string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), daemonStopWaitLimit)
	defer cancel()
	c, err := daemonClient(ctx, version)
	if err != nil {
		if running, rerr := daemonNotRunning(err, ZutHome()); !running {
			return false, rerr
		}
		return false, nil
	}
	err = c.Stop(ctx, daemonStopGrace)
	_ = c.Disconnect()
	if err != nil {
		return false, fmt.Errorf("daemon stop: %w", err)
	}
	return true, waitDaemonLockFree(ctx, ZutHome())
}

func waitDaemonLockFree(ctx context.Context, home string) error {
	path := filepath.Join(home, "daemon", "daemon.lock")
	for {
		l, err := flock.TryLock(path)
		if err == nil {
			return l.Close()
		}
		if !errors.Is(err, flock.ErrBusy) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("daemon did not stop: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func daemonRestart(w io.Writer, version string) error {
	if _, err := stopDaemon(version); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), daemonClientDeadline)
	defer cancel()
	c, err := daemonclient.EnsureDaemon(ctx, daemonclient.EnsureOptions{Options: daemonclient.Options{ClientVersion: version, ClientKind: "cli"}})
	if err != nil {
		return err
	}
	defer c.Disconnect()
	st, err := c.Status(ctx)
	if err != nil {
		return fmt.Errorf("daemon status: %w", err)
	}
	fmt.Fprintf(w, "zut daemon: restarted (pid %d)\n", st.PID)
	return nil
}

// daemonStart spawns the daemon in the background unless one already runs.
func daemonStart(w io.Writer, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), daemonClientDeadline)
	defer cancel()
	c, err := daemonclient.EnsureDaemon(ctx, daemonclient.EnsureOptions{Options: daemonclient.Options{ClientVersion: version, ClientKind: "cli"}})
	if err != nil {
		return err
	}
	defer c.Disconnect()
	st, err := c.Status(ctx)
	if err != nil {
		return fmt.Errorf("daemon status: %w", err)
	}
	fmt.Fprintf(w, "zut daemon: running\npid: %d\nsocket: %s\n", st.PID, daemonclient.SocketPath(os.Getenv))
	return nil
}

const daemonLogPollInterval = 250 * time.Millisecond

func daemonLogsCommand(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("zut daemon logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "follow appended lines until interrupted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("zut daemon logs: unexpected arguments %v", fs.Args())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return daemonLogs(ctx, w, daemon.LogPath(ZutHome()), *follow)
}

// daemonLogs copies the log to w; with follow it then polls the file size
// and copies appended bytes until ctx ends (a truncated log restarts at 0).
func daemonLogs(ctx context.Context, w io.Writer, path string, follow bool) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !follow {
			return fmt.Errorf("no daemon log at %s", path)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	var off int64
	for {
		if f != nil {
			fi, err := f.Stat()
			if err != nil {
				f.Close()
				return err
			}
			if fi.Size() < off {
				off = 0
			}
			if fi.Size() > off {
				n, err := io.Copy(w, io.NewSectionReader(f, off, fi.Size()-off))
				off += n
				if err != nil {
					f.Close()
					return err
				}
			}
		}
		if !follow {
			return f.Close()
		}
		select {
		case <-ctx.Done():
			if f != nil {
				f.Close()
			}
			return nil
		case <-time.After(daemonLogPollInterval):
		}
		if f == nil {
			if f, err = os.Open(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			} else if err != nil {
				f = nil
			}
		}
	}
}
