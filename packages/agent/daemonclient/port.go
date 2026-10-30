package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/agent/daemonproto"
	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

// SubscriberBuffer bounds undelivered events per subscription. Like the
// session controller, a full buffer ends the subscription with a final
// EvAttach{detached, "slow_consumer"} event instead of blocking the read loop.
const SubscriberBuffer = 256

// maxSlowResubscribes bounds consecutive re-attaches after daemon-side
// slow_consumer drops with no delivered progress in between; one more such
// drop ends the stream with a final slow_consumer event.
const maxSlowResubscribes = 3

// Reconnect backoff bounds for subscriptions after transport loss.
const (
	reconnectMin = 50 * time.Millisecond
	reconnectMax = 400 * time.Millisecond
)

// infoTimeout bounds the Info fallback request when nothing is cached.
const infoTimeout = 2 * time.Second

// detachTimeout bounds the best-effort detach sent when a stream's ctx ends.
const detachTimeout = 2 * time.Second

var _ session.Port = (*RemotePort)(nil)

// RemotePort implements session.Port over the daemon "port" request.
type RemotePort struct {
	c           *Client
	id          session.ID
	controlling bool

	mu   sync.Mutex
	info session.Info
	have bool
}

// portFrame keeps params typed so the frame codec preserves interface content.
type portFrame struct {
	Type    string     `json:"type"`
	Session session.ID `json:"session"`
	Method  string     `json:"method"`
	Params  any        `json:"params"`
}

func (p *RemotePort) port(ctx context.Context, method string, params, out any) error {
	if params == nil {
		params = struct{}{}
	}
	err := p.c.call(ctx, portFrame{Type: "port", Session: p.id, Method: method, Params: params}, out)
	if err != nil && strings.Contains(err.Error(), "unsupported port method") {
		// An older daemon predates this method; restarting it picks up the
		// client's version.
		return fmt.Errorf("%w; run \"zut daemon restart\"", err)
	}
	return err
}

func (p *RemotePort) setInfo(info session.Info) {
	p.mu.Lock()
	p.info, p.have = info, true
	p.mu.Unlock()
}

// Info returns the latest known Info: cached from attach results and EvInfo
// events, otherwise fetched with a short timeout (zero Info on failure).
func (p *RemotePort) Info() session.Info {
	p.mu.Lock()
	info, have := p.info, p.have
	p.mu.Unlock()
	if have {
		return info
	}
	ctx, cancel := context.WithTimeout(context.Background(), infoTimeout)
	defer cancel()
	if err := p.port(ctx, "info", nil, &info); err == nil {
		p.setInfo(info)
	}
	return info
}

// Snapshot implements session.Port.
func (p *RemotePort) Snapshot(ctx context.Context) (session.Snapshot, error) {
	var s session.Snapshot
	err := p.port(ctx, "snapshot", nil, &s)
	if err == nil {
		p.setInfo(s.Info)
	}
	return s, err
}

// Subscribe attaches and streams events after afterSeq. ctx scopes the
// stream: when it ends the channel closes and, for a controlling port, a
// best-effort detach is sent. An initial ErrSeqTooOld is returned. After
// transport loss it redials and re-attaches after the last delivered Seq, so
// no event is skipped or repeated; if the daemon no longer retains it (or its
// sequence restarted), a synthetic EvMessagesReplaced (Seq = the fresh
// snapshot Seq the stream resumes after) tells the consumer to refetch.
// A daemon-side slow_consumer drop re-attaches the same way (bounded by
// maxSlowResubscribes without progress). detached_by_other, closed, local
// slow-consumer overflow and refused re-attach endings close the channel
// after a final event.
func (p *RemotePort) Subscribe(ctx context.Context, afterSeq uint64) (<-chan session.Event, error) {
	s := &subscription{port: p, last: afterSeq, out: make(chan session.Event, SubscriberBuffer+1), stop: make(chan struct{}), resub: make(chan struct{}, 1)}
	c := p.c
	conn, err := c.conn(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, daemonproto.ErrClosed
	}
	if old := c.subs[p.id]; old != nil {
		defer old.finish(nil)
	}
	c.subs[p.id] = s
	c.routines.Add(1)
	c.mu.Unlock()
	if err := s.attach(ctx, conn); err != nil {
		s.finish(nil)
		c.routines.Done()
		return nil, err
	}
	go func() {
		defer c.routines.Done()
		s.supervise(ctx, conn)
	}()
	return s.out, nil
}

func (c *Client) dropSub(s *subscription) {
	c.mu.Lock()
	if c.subs[s.port.id] == s {
		delete(c.subs, s.port.id)
	}
	c.mu.Unlock()
}

// onNotification runs on the read loop: it routes without blocking.
func (c *Client) onNotification(n daemonproto.Notification) {
	c.mu.Lock()
	s := c.subs[n.Session]
	c.mu.Unlock()
	if s == nil {
		return
	}
	switch n.Type {
	case "event":
		var en daemonproto.EventNotification
		if json.Unmarshal(n.Raw, &en) != nil {
			return
		}
		ev, err := daemonproto.DecodeEvent(en)
		if err != nil {
			return
		}
		s.deliver(ev)
	case "detached_by_other":
		s.finish(&session.Event{Session: n.Session, At: time.Now(), Kind: session.EvAttach, Data: session.AttachEvent{State: session.AttachStateDetached, Reason: "detached_by_other"}})
	case "closed":
		var cn daemonproto.ClosedNotification
		_ = json.Unmarshal(n.Raw, &cn)
		s.finish(&session.Event{Session: n.Session, At: time.Now(), Kind: session.EvClosed, Data: session.ClosedEvent{Successor: cn.Successor}})
	}
}

type subscription struct {
	port *RemotePort

	mu   sync.Mutex
	last uint64
	out  chan session.Event
	done bool
	stop chan struct{}
	// resub asks supervise to re-attach after the daemon dropped this
	// subscriber as a slow consumer.
	resub chan struct{}
}

// deliver forwards ev unless already delivered, ending slow consumers. A
// slow_consumer detach received from the daemon is not forwarded: it only
// means the daemon dropped the subscriber, so supervise re-attaches. A full
// local out buffer is the client's own overflow and stays final.
func (s *subscription) deliver(ev session.Event) {
	s.mu.Lock()
	if s.done || ev.Seq <= s.last {
		s.mu.Unlock()
		return
	}
	if isSlowConsumer(ev) {
		s.mu.Unlock()
		select {
		case s.resub <- struct{}{}:
		default:
		}
		return
	}
	if len(s.out) >= cap(s.out)-1 {
		s.mu.Unlock()
		s.finish(&session.Event{Seq: ev.Seq, Session: ev.Session, At: ev.At, Kind: session.EvAttach, Data: session.AttachEvent{State: session.AttachStateDetached, Reason: "slow_consumer"}})
		return
	}
	s.last = ev.Seq
	s.out <- ev
	terminal := ev.Kind == session.EvClosed
	s.mu.Unlock()
	if info, ok := ev.Data.(session.InfoEvent); ok {
		s.port.setInfo(info.Info)
	}
	if terminal {
		s.finish(nil)
	}
}

func isSlowConsumer(ev session.Event) bool {
	a, ok := ev.Data.(session.AttachEvent)
	return ev.Kind == session.EvAttach && ok && a.Reason == "slow_consumer"
}

// slowResubscribeAllowed reports whether a daemon slow_consumer drop may be
// retried. progressAt is the last Seq seen at the previous drop and stalls
// counts consecutive drops without delivered progress since then.
func (s *subscription) slowResubscribeAllowed(progressAt *uint64, stalls *int) bool {
	cur := s.lastSeq()
	if cur > *progressAt {
		*stalls = 0
	} else {
		*stalls++
	}
	*progressAt = cur
	return *stalls <= maxSlowResubscribes
}

// finish closes the stream once, after an optional final event. The extra
// buffer slot guarantees the final event never blocks.
func (s *subscription) finish(final *session.Event) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	s.done = true
	if final != nil {
		s.out <- *final
	}
	close(s.out)
	close(s.stop)
	s.mu.Unlock()
	s.port.c.dropSub(s)
}

func (s *subscription) lastSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *subscription) attach(ctx context.Context, conn *daemonproto.Conn) error {
	var info session.Info
	err := mapErr(conn.Call(ctx, daemonproto.AttachRequest{Type: "attach", Session: s.port.id, AfterSeq: s.lastSeq(), Controlling: s.port.controlling}, &info))
	if err == nil {
		s.port.setInfo(info)
	}
	return err
}

// reattach resumes after the last delivered Seq, or after a fresh snapshot
// with a synthetic EvMessagesReplaced when that Seq is no longer retained or
// the daemon's sequence restarted below it.
func (s *subscription) reattach(ctx context.Context, conn *daemonproto.Conn) error {
	var snap session.Snapshot
	if err := mapErr(conn.Call(ctx, daemonproto.SnapshotRequest{Type: "snapshot", Session: s.port.id}, &snap)); err != nil {
		return err
	}
	if snap.Seq >= s.lastSeq() {
		err := s.attach(ctx, conn)
		if !errors.Is(err, session.ErrSeqTooOld) {
			return err
		}
		if err := mapErr(conn.Call(ctx, daemonproto.SnapshotRequest{Type: "snapshot", Session: s.port.id}, &snap)); err != nil {
			return err
		}
	}
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return nil
	}
	if len(s.out) >= cap(s.out)-1 {
		s.mu.Unlock()
		s.finish(&session.Event{Seq: snap.Seq, Session: s.port.id, At: time.Now(), Kind: session.EvAttach, Data: session.AttachEvent{State: session.AttachStateDetached, Reason: "slow_consumer"}})
		return nil
	}
	s.last = snap.Seq
	s.out <- session.Event{Seq: snap.Seq, Session: s.port.id, At: time.Now(), Kind: session.EvMessagesReplaced, Data: session.MessagesReplacedEvent{}}
	s.mu.Unlock()
	return s.attach(ctx, conn)
}

// transient reports transport-level failures worth redialing for.
func transient(err error) bool {
	var ne net.Error
	return errors.Is(err, daemonproto.ErrClosed) || errors.As(err, &ne) || absent(err)
}

// supervise ends the stream on ctx, reconnects after transport loss and
// re-attaches after a daemon-side slow_consumer drop. Any non-transport
// failure (the daemon answered and refused) is terminal.
func (s *subscription) supervise(ctx context.Context, conn *daemonproto.Conn) {
	defer s.finish(nil)
	progressAt, stalls := s.lastSeq(), 0
	for {
		var err error
		select {
		case <-ctx.Done():
			s.detachOnCancel()
			return
		case <-s.stop:
			return
		case <-s.resub:
			if !s.slowResubscribeAllowed(&progressAt, &stalls) {
				s.finish(&session.Event{Seq: s.lastSeq(), Session: s.port.id, At: time.Now(), Kind: session.EvAttach, Data: session.AttachEvent{State: session.AttachStateDetached, Reason: "slow_consumer"}})
				return
			}
			if err = s.reattach(ctx, conn); err == nil {
				continue
			}
		case <-conn.Done():
		}
		// The old connection is gone and the reconnect re-attaches after the
		// last Seq, so a drop signalled on it is stale. Drain it before the
		// new attach so a drop raised on the new connection is kept.
		select {
		case <-s.resub:
		default:
		}
		delay := reconnectMin
		for {
			if err == nil || transient(err) {
				var next *daemonproto.Conn
				next, err = s.port.c.conn(ctx)
				if err == nil {
					err = s.reattach(ctx, next)
					if err == nil {
						conn = next
						break
					}
				}
			}
			if ctx.Err() != nil {
				s.detachOnCancel()
				return
			}
			if s.port.c.isClosed() {
				return
			}
			if !transient(err) {
				s.finish(&session.Event{Seq: s.lastSeq(), Session: s.port.id, At: time.Now(), Kind: session.EvClosed, Data: session.ClosedEvent{Reason: err.Error()}})
				return
			}
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				s.detachOnCancel()
				return
			case <-s.stop:
				t.Stop()
				return
			case <-t.C:
			}
			delay = min(delay*2, reconnectMax)
		}
	}
}

// detachOnCancel releases control when a controlling stream's ctx ends.
// It is best effort and bounded; Disconnect cancels it.
func (s *subscription) detachOnCancel() {
	c := s.port.c
	if !s.port.controlling || c.isClosed() {
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, detachTimeout)
	defer cancel()
	_ = c.Detach(ctx, s.port.id)
}

func (c *Client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Submit implements session.Port.
func (p *RemotePort) Submit(ctx context.Context, in session.Input) error {
	return p.port(ctx, "submit", struct {
		In session.Input `json:"in"`
	}{in}, nil)
}

// Abort implements session.Port.
func (p *RemotePort) Abort(ctx context.Context) error { return p.port(ctx, "abort", nil, nil) }

// Compact implements session.Port.
func (p *RemotePort) Compact(ctx context.Context) error { return p.port(ctx, "compact", nil, nil) }

// CancelQueued implements session.Port.
func (p *RemotePort) CancelQueued(ctx context.Context, id string) error {
	return p.port(ctx, "cancel_queued", struct {
		ID string `json:"id"`
	}{id}, nil)
}

// SlideBackQueued implements session.Port.
func (p *RemotePort) SlideBackQueued(ctx context.Context) (session.Input, error) {
	var in session.Input
	err := p.port(ctx, "slide_back_queued", nil, &in)
	return in, err
}

// ClearQueuedUser implements session.Port. Daemons predating the method
// answer daemonproto.UnsupportedPortMethod; that is mapped to
// ErrUnknownRequest so callers can degrade gracefully.
func (p *RemotePort) ClearQueuedUser(ctx context.Context) error {
	err := p.port(ctx, "clear_queued_user", nil, nil)
	if isUnsupportedPortMethod(err) {
		return fmt.Errorf("%w: %w", session.ErrUnknownRequest, err)
	}
	return err
}

// isUnsupportedPortMethod reports a daemon rejecting an unknown port method.
func isUnsupportedPortMethod(err error) bool {
	var re *daemonproto.RemoteError
	return err != nil && !errors.Is(err, session.ErrUnknownRequest) && errors.As(err, &re) && strings.Contains(re.Msg, daemonproto.UnsupportedPortMethod)
}

// AnswerConfirmation implements session.Port.
func (p *RemotePort) AnswerConfirmation(ctx context.Context, reqID string, decision core.ConfirmDecision) error {
	return p.port(ctx, "answer_confirmation", struct {
		ReqID    string               `json:"req_id"`
		Decision core.ConfirmDecision `json:"decision"`
	}{reqID, decision}, nil)
}

// AnswerQuestion implements session.Port.
func (p *RemotePort) AnswerQuestion(ctx context.Context, reqID string, answers []tools.QuestionAnswer) error {
	return p.port(ctx, "answer_question", struct {
		ReqID   string                 `json:"req_id"`
		Answers []tools.QuestionAnswer `json:"answers"`
	}{reqID, answers}, nil)
}

// ResolveDeferred implements session.Port.
func (p *RemotePort) ResolveDeferred(ctx context.Context, id, action string) error {
	return p.port(ctx, "resolve_deferred", struct {
		ID     string `json:"id"`
		Action string `json:"action"`
	}{id, action}, nil)
}

// SetModel implements session.Port.
func (p *RemotePort) SetModel(ctx context.Context, providerName, model string) error {
	return p.port(ctx, "set_model", struct {
		ProviderName string `json:"provider_name"`
		Model        string `json:"model"`
	}{providerName, model}, nil)
}

// SetReasoning implements session.Port.
func (p *RemotePort) SetReasoning(ctx context.Context, level string) error {
	return p.port(ctx, "set_reasoning", struct {
		Level string `json:"level"`
	}{level}, nil)
}

type enabledParams struct {
	Enabled bool `json:"enabled"`
}

// SetFastMode implements session.Port.
func (p *RemotePort) SetFastMode(ctx context.Context, enabled bool) error {
	return p.port(ctx, "set_fast_mode", enabledParams{enabled}, nil)
}

// ApplyProfile implements session.Port.
func (p *RemotePort) ApplyProfile(ctx context.Context, n int) error {
	return p.port(ctx, "apply_profile", struct {
		N int `json:"n"`
	}{n}, nil)
}

// AssignProfile implements session.Port.
func (p *RemotePort) AssignProfile(ctx context.Context, slot int, profile session.QuickModelShortcut, activate bool) error {
	return p.port(ctx, "assign_profile", struct {
		Slot     int                        `json:"slot"`
		Profile  session.QuickModelShortcut `json:"profile"`
		Activate bool                       `json:"activate"`
	}{slot, profile, activate}, nil)
}

// ClearProfile implements session.Port.
func (p *RemotePort) ClearProfile(ctx context.Context, slot int) error {
	return p.port(ctx, "clear_profile", struct {
		Slot int `json:"slot"`
	}{slot}, nil)
}

// ApplySettings implements session.Port.
func (p *RemotePort) ApplySettings(ctx context.Context, patch session.SettingsPatch) error {
	return p.port(ctx, "apply_settings", struct {
		Patch session.SettingsPatch `json:"patch"`
	}{patch}, nil)
}

// SetGoal implements session.Port.
func (p *RemotePort) SetGoal(ctx context.Context, command session.GoalCommand) error {
	return p.port(ctx, "set_goal", struct {
		Command session.GoalCommand `json:"command"`
	}{command}, nil)
}

// ChangeCWD implements session.Port.
func (p *RemotePort) ChangeCWD(ctx context.Context, path string) error {
	return p.port(ctx, "change_cwd", struct {
		Path string `json:"path"`
	}{path}, nil)
}

// SetJail implements session.Port.
func (p *RemotePort) SetJail(ctx context.Context, enabled bool) error {
	return p.port(ctx, "set_jail", enabledParams{enabled}, nil)
}

// SetTitle implements session.Port.
func (p *RemotePort) SetTitle(ctx context.Context, title string) error {
	return p.port(ctx, "set_title", struct {
		Title string `json:"title"`
	}{title}, nil)
}

// AppendShellContext implements session.Port.
func (p *RemotePort) AppendShellContext(ctx context.Context, text string) error {
	return p.port(ctx, "append_shell_context", struct {
		Text string `json:"text"`
	}{text}, nil)
}

// ToggleSkillPin implements session.Port.
func (p *RemotePort) ToggleSkillPin(ctx context.Context, name string, global bool) error {
	return p.port(ctx, "toggle_skill_pin", struct {
		Name   string `json:"name"`
		Global bool   `json:"global"`
	}{name, global}, nil)
}

// ReloadExtensions implements session.Port.
func (p *RemotePort) ReloadExtensions(ctx context.Context) error {
	return p.port(ctx, "reload_extensions", nil, nil)
}

// InvokeExtensionCommand implements session.Port.
func (p *RemotePort) InvokeExtensionCommand(ctx context.Context, name, args string) error {
	return p.port(ctx, "invoke_extension_command", struct {
		Name string `json:"name"`
		Args string `json:"args"`
	}{name, args}, nil)
}

// ExtensionPanelInput implements session.Port.
func (p *RemotePort) ExtensionPanelInput(ctx context.Context, ext, panelID string, key extproto.PanelKeyFromHost) error {
	return p.port(ctx, "extension_panel_input", struct {
		Ext     string                    `json:"ext"`
		PanelID string                    `json:"panel_id"`
		Key     extproto.PanelKeyFromHost `json:"key"`
	}{ext, panelID, key}, nil)
}

// ReloadCredentials implements session.Port.
func (p *RemotePort) ReloadCredentials(ctx context.Context) error {
	return p.port(ctx, "reload_credentials", nil, nil)
}

// ResidentAction implements session.Port.
func (p *RemotePort) ResidentAction(ctx context.Context, command session.ResidentCommand) error {
	return p.port(ctx, "resident_action", struct {
		Command session.ResidentCommand `json:"command"`
	}{command}, nil)
}

// SessionsQuery implements session.Port.
func (p *RemotePort) SessionsQuery(ctx context.Context) (session.Tree, error) {
	var t session.Tree
	err := p.port(ctx, "sessions_query", nil, &t)
	return t, err
}

// ResidentList implements session.Port.
func (p *RemotePort) ResidentList(ctx context.Context, offset, limit int) ([]subagents.ResidentSnapshot, int, error) {
	var data daemonproto.ResidentListData
	err := p.port(ctx, "resident_list", daemonproto.ResidentListParams{Offset: offset, Limit: limit}, &data)
	return data.Residents, data.Total, err
}

// ResidentHistory implements session.Port.
func (p *RemotePort) ResidentHistory(ctx context.Context, childID, olderCursor string, limit int) (subagents.ResidentHistoryPage, error) {
	var page subagents.ResidentHistoryPage
	err := p.port(ctx, "resident_history", daemonproto.ResidentHistoryParams{ChildID: childID, OlderCursor: olderCursor, Limit: limit}, &page)
	return page, err
}
