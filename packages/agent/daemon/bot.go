package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/session"
)

// BotHost runs protocol bots inside the daemon. The session factory provides
// it (the daemon detects it on its session.Factory), so the daemon stays
// free of protocol code. Implementations read credentials themselves and
// must never log them.
type BotHost interface {
	// CheckBot validates kind and its configuration before a session is
	// created (for example "not configured — run setup").
	CheckBot(kind string) error
	// RunBot serves kind against port until ctx ends or the session closes.
	RunBot(ctx context.Context, kind string, port session.Port) error
	// RunBridge mirrors kind's paired chat into port until ctx ends or the
	// session closes. ready receives nil once polling started, or the
	// startup error.
	RunBridge(ctx context.Context, kind string, port session.Port, ready func(error)) error
	// BotIdentity reports the bot username and paired user, if known.
	BotIdentity(kind string) (username string, pairedUserID int64)
}

// ErrBotRunning reports a bot_start for a kind that already runs, or a bridge
// and a bot service competing for the same bot.
var ErrBotRunning = errors.New("bot is already running")

// errNoBotHost reports bot requests on a daemon whose factory has no BotHost.
var errNoBotHost = errors.New("this daemon does not host bots")

// botStopReason closes a bot session on bot_stop.
const botStopReason = "bot_stopped"

// botStartTimeout bounds the bridge startup handshake.
const botStartTimeout = 30 * time.Second

type botRun struct {
	kind    string
	session session.ID
	started time.Time
	cancel  context.CancelFunc
	done    chan struct{}
}

// botState is the daemon's bot services and interactive bridges, keyed by
// kind (one long-poll consumer per bot token).
type botState struct {
	mu       sync.Mutex
	services map[string]*botRun
	bridges  map[string]*botRun
	lastErr  map[string]string
}

func newBotState() *botState {
	return &botState{services: map[string]*botRun{}, bridges: map[string]*botRun{}, lastErr: map[string]string{}}
}

func (d *Daemon) bots() *botState { return d.bot }

func (d *Daemon) botHost() (BotHost, error) {
	h, ok := d.factory.(BotHost)
	if !ok {
		return nil, errNoBotHost
	}
	return h, nil
}

// handleBot serves bot_start, bot_stop, bot_status and telegram_bridge.
func (c *client) handleBot(ctx context.Context, r daemonproto.Request) (any, error, bool) {
	d := c.d
	switch r.Type {
	case "bot_start":
		req, err := decodeFrame[daemonproto.BotStartRequest](r)
		if err != nil {
			return nil, err, true
		}
		v, err := d.botStart(req.Spec)
		return v, err, true
	case "bot_stop":
		req, err := decodeFrame[daemonproto.BotStopRequest](r)
		if err != nil {
			return nil, err, true
		}
		v, err := d.botStop(req.Kind)
		return v, err, true
	case "bot_status":
		req, err := decodeFrame[daemonproto.BotStatusRequest](r)
		if err != nil {
			return nil, err, true
		}
		v, err := d.botStatus(req.Kind)
		return v, err, true
	case "telegram_bridge":
		req, err := decodeFrame[daemonproto.TelegramBridgeRequest](r)
		if err != nil {
			return nil, err, true
		}
		if req.On {
			v, err := d.bridgeOn(ctx, "telegram", req.Session)
			return v, err, true
		}
		v, err := d.bridgeOff("telegram", req.Session)
		return v, err, true
	}
	return nil, nil, false
}

func botKind(kind string) string {
	if kind == "" {
		return "telegram"
	}
	return kind
}

// botStart creates one headless session and runs
// the bot service against it until bot_stop, daemon stop or session close.
func (d *Daemon) botStart(spec daemonproto.BotSpec) (daemonproto.BotStatusData, error) {
	kind := botKind(spec.Kind)
	host, err := d.botHost()
	if err != nil {
		return daemonproto.BotStatusData{}, err
	}
	if err := host.CheckBot(kind); err != nil {
		return daemonproto.BotStatusData{}, err
	}
	// Hold a shutdown token for the whole start so a start racing shutdown
	// finishes its cleanup (session close) before shutdown
	// returns.
	d.mu.Lock()
	if d.ctx == nil || d.stopping {
		d.mu.Unlock()
		return daemonproto.BotStatusData{}, ErrStopping
	}
	d.wg.Add(1)
	d.mu.Unlock()
	defer d.wg.Done()
	st := d.bots()
	st.mu.Lock()
	if st.services[kind] != nil {
		st.mu.Unlock()
		return daemonproto.BotStatusData{}, fmt.Errorf("%s %w; stop it first", kind, ErrBotRunning)
	}
	if st.bridges[kind] != nil {
		st.mu.Unlock()
		return daemonproto.BotStatusData{}, fmt.Errorf("%w: a session mirrors this %s bot; turn the bridge off first", ErrBotRunning, kind)
	}
	// Reserve the kind while the session is created.
	run := &botRun{kind: kind, started: d.now(), done: make(chan struct{})}
	st.services[kind] = run
	st.mu.Unlock()
	release := func() {
		st.mu.Lock()
		delete(st.services, kind)
		st.mu.Unlock()
	}

	e, err := d.botSession(spec)
	if err != nil {
		release()
		return daemonproto.BotStatusData{}, err
	}
	d.mu.Lock()
	if d.ctx == nil || d.stopping {
		d.mu.Unlock()
		release()
		// The session was created for this bot; do not leak it into a
		// stopping daemon.
		d.closeBotSession(e.id)
		return daemonproto.BotStatusData{}, ErrStopping
	}
	ctx, cancel := context.WithCancel(d.ctx)
	// Add under d.mu with the stopping check so shutdown's wg.Wait cannot
	// miss this goroutine.
	d.wg.Add(1)
	d.mu.Unlock()
	st.mu.Lock()
	run.session, run.cancel = e.id, cancel
	delete(st.lastErr, kind)
	st.mu.Unlock()
	d.log.Info("bot started", "kind", kind, "session", e.id)
	go func() {
		defer d.wg.Done()
		defer close(run.done)
		defer cancel()
		err := d.serveBot(ctx, host, run, spec, e)
		st.mu.Lock()
		if st.services[kind] == run {
			delete(st.services, kind)
		}
		if err != nil && ctx.Err() == nil {
			st.lastErr[kind] = err.Error()
		}
		st.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			d.log.Warn("bot stopped", "kind", kind, "err", err)
		} else {
			d.log.Info("bot stopped", "kind", kind)
		}
	}()
	return d.botStatus(kind)
}

// botSession creates a new headless session, including after a daemon restart.
func (d *Daemon) botSession(spec daemonproto.BotSpec) (*entry, error) {
	info, err := d.create(session.CreateSpec{CWD: spec.CWD, Env: spec.Env, Args: spec.Args, Headless: true})
	if err != nil {
		return nil, err
	}
	e := d.lookup(session.ID(info.ID))
	if e == nil {
		return nil, ErrUnknownSession
	}
	return e, nil
}

// serveBot runs the bot until ctx ends. When its session ends while the
// service is still wanted (idle GC, zut kill), a fresh session replaces it.
func (d *Daemon) serveBot(ctx context.Context, host BotHost, run *botRun, spec daemonproto.BotSpec, e *entry) error {
	for {
		err := host.RunBot(ctx, run.kind, e.h)
		if ctx.Err() != nil {
			return nil
		}
		if !errors.Is(err, session.ErrClosed) {
			return err
		}
		select {
		case <-e.h.Done():
		case <-ctx.Done():
			return nil
		}
		if d.isStopping() {
			return nil
		}
		d.log.Info("bot session ended; starting a new one", "kind", run.kind, "session", e.id)
		next, nerr := d.botSession(spec)
		if nerr != nil {
			return nerr
		}
		st := d.bots()
		st.mu.Lock()
		run.session = next.id
		st.mu.Unlock()
		e = next
	}
}

// closeBotSession closes a bot session created by a start that lost the
// race with shutdown.
func (d *Daemon) closeBotSession(id session.ID) {
	d.opMu.Lock()
	err := d.closeLocked(id, ReasonDaemonStop, "")
	d.opMu.Unlock()
	if err != nil && !errors.Is(err, ErrUnknownSession) {
		d.log.Warn("close bot session", "session", id, "err", err)
	}
}

// botStop stops kind's service and closes its session (the session file
// stays resumable).
func (d *Daemon) botStop(kind string) (daemonproto.BotStatusData, error) {
	kind = botKind(kind)
	st := d.bots()
	st.mu.Lock()
	run := st.services[kind]
	var cancel context.CancelFunc
	if run != nil {
		cancel = run.cancel
	}
	st.mu.Unlock()
	if run == nil {
		return d.botStatus(kind)
	}
	if cancel == nil {
		return daemonproto.BotStatusData{}, fmt.Errorf("%s bot is starting; retry", kind)
	}
	cancel()
	<-run.done
	st.mu.Lock()
	id := run.session
	st.mu.Unlock()

	d.opMu.Lock()
	err := d.closeLocked(id, botStopReason, "")
	d.opMu.Unlock()
	if err != nil && !errors.Is(err, ErrUnknownSession) {
		d.log.Warn("close bot session", "session", id, "err", err)
	}
	return d.botStatus(kind)
}

func (d *Daemon) botStatus(kind string) (daemonproto.BotStatusData, error) {
	kind = botKind(kind)
	host, err := d.botHost()
	if err != nil {
		return daemonproto.BotStatusData{}, err
	}
	st := d.bots()
	st.mu.Lock()
	out := daemonproto.BotStatusData{Kind: kind, LastError: st.lastErr[kind]}
	if run := st.services[kind]; run != nil {
		out.Running, out.Session, out.StartedAt = true, run.session, run.started
	}
	if br := st.bridges[kind]; br != nil {
		out.Bridge = &daemonproto.TelegramBridgeData{Active: true, Session: br.session}
	}
	st.mu.Unlock()
	out.Username, out.PairedUserID = host.BotIdentity(kind)
	if out.Bridge != nil {
		out.Bridge.Username, out.Bridge.PairedUserID = out.Username, out.PairedUserID
	}
	return out, nil
}

// bridgeOn starts mirroring kind's paired chat into id. It replaces a bridge
// bound to another session and fails while a bot service polls the same bot.
// The bridge ends with the session, bridge off or daemon stop; it does not
// depend on any attached client.
func (d *Daemon) bridgeOn(ctx context.Context, kind string, id session.ID) (daemonproto.TelegramBridgeData, error) {
	host, err := d.botHost()
	if err != nil {
		return daemonproto.TelegramBridgeData{}, err
	}
	e := d.lookup(id)
	if e == nil {
		return daemonproto.TelegramBridgeData{}, ErrUnknownSession
	}
	if err := host.CheckBot(kind); err != nil {
		return daemonproto.TelegramBridgeData{}, err
	}
	st := d.bots()
	st.mu.Lock()
	if st.services[kind] != nil {
		st.mu.Unlock()
		return daemonproto.TelegramBridgeData{}, fmt.Errorf("%w: the %s bot service is polling this bot; stop it first", ErrBotRunning, kind)
	}
	prev := st.bridges[kind]
	st.mu.Unlock()
	if prev != nil {
		if prev.session == id {
			return d.bridgeData(kind)
		}
		prev.cancel()
		<-prev.done
	}
	d.mu.Lock()
	if d.ctx == nil || d.stopping {
		d.mu.Unlock()
		return daemonproto.TelegramBridgeData{}, ErrStopping
	}
	runCtx, cancel := context.WithCancel(d.ctx)
	d.wg.Add(1) // released below if the bridge is refused
	d.mu.Unlock()
	run := &botRun{kind: kind, session: id, started: d.now(), cancel: cancel, done: make(chan struct{})}
	st.mu.Lock()
	if st.bridges[kind] != nil || st.services[kind] != nil {
		st.mu.Unlock()
		cancel()
		d.wg.Done()
		return daemonproto.TelegramBridgeData{}, fmt.Errorf("%s %w", kind, ErrBotRunning)
	}
	st.bridges[kind] = run
	st.mu.Unlock()
	ready := make(chan error, 1)
	go func() {
		defer d.wg.Done()
		defer close(run.done)
		defer cancel()
		err := host.RunBridge(runCtx, kind, e.h, func(err error) {
			select {
			case ready <- err:
			default:
			}
		})
		select {
		case ready <- err:
		default:
		}
		st.mu.Lock()
		if st.bridges[kind] == run {
			delete(st.bridges, kind)
		}
		st.mu.Unlock()
		d.log.Info("telegram bridge stopped", "session", id)
	}()
	wait, waitCancel := context.WithTimeout(ctx, botStartTimeout)
	defer waitCancel()
	select {
	case err := <-ready:
		if err != nil {
			cancel()
			<-run.done
			return daemonproto.TelegramBridgeData{}, err
		}
	case <-wait.Done():
		cancel()
		<-run.done
		return daemonproto.TelegramBridgeData{}, wait.Err()
	}
	d.log.Info("telegram bridge started", "session", id)
	return d.bridgeData(kind)
}

// bridgeOff stops kind's bridge when it is bound to id (any session when id
// is empty).
func (d *Daemon) bridgeOff(kind string, id session.ID) (daemonproto.TelegramBridgeData, error) {
	st := d.bots()
	st.mu.Lock()
	run := st.bridges[kind]
	st.mu.Unlock()
	if run != nil && (id == "" || run.session == id) {
		run.cancel()
		<-run.done
	}
	return d.bridgeData(kind)
}

func (d *Daemon) bridgeData(kind string) (daemonproto.TelegramBridgeData, error) {
	s, err := d.botStatus(kind)
	if err != nil {
		return daemonproto.TelegramBridgeData{}, err
	}
	if s.Bridge != nil {
		return *s.Bridge, nil
	}
	return daemonproto.TelegramBridgeData{Username: s.Username, PairedUserID: s.PairedUserID}, nil
}

// BotFactory is a session factory that also hosts bots. The daemon enables
// bot requests when its factory implements BotHost.
type BotFactory interface {
	session.Factory
	BotHost
}
