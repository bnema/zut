package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/agent/extensions"
	"github.com/bnema/zut/packages/agent/extproto"
)

// extState retains terminal-free chrome and extension ownership of notes.
// Note has no owner field on the public contract, so ownership stays internal.
type extState struct {
	mu             sync.Mutex
	mailbox        []func()
	wake           chan struct{}
	port           Port
	domainJobs     chan func(context.Context, Port) error
	ctx            context.Context
	cancel         context.CancelFunc
	stopped        bool
	dispatcherDone chan struct{}
	overload       string
	lastOverload   time.Time
	clock          func() time.Time
	chrome         ExtChrome
	notes          []ownedExtNote
	alertOwner     string
}
type ownedExtNote struct {
	ext  string
	note Note
}

func cloneChrome(in ExtChrome) ExtChrome {
	out := ExtChrome{Statuses: make(map[string]map[string]ExtStatus), Widgets: make(map[string]map[string]ExtWidget)}
	for ext, statuses := range in.Statuses {
		out.Statuses[ext] = make(map[string]ExtStatus)
		for key, status := range statuses {
			out.Statuses[ext][key] = status
		}
	}
	for ext, widgets := range in.Widgets {
		out.Widgets[ext] = make(map[string]ExtWidget)
		for id, widget := range widgets {
			widget.Lines = append([]string(nil), widget.Lines...)
			out.Widgets[ext][id] = widget
		}
	}
	for _, panel := range in.Panels {
		panel.Spec.Lines = append([]string(nil), panel.Spec.Lines...)
		out.Panels = append(out.Panels, panel)
	}
	if in.Alert != nil {
		alert := *in.Alert
		out.Alert = &alert
	}
	return out
}
func (s *extState) snapshotInto(out *Snapshot) {
	out.ExtChrome = cloneChrome(s.chrome)
	for _, n := range s.notes {
		out.Notes = append(out.Notes, n.note)
	}
}

// extHooks separates HostHooks.Submit from the Port method of the same name.
type extHooks struct{ c *Controller }

var (
	_ extensions.HostHooks                = extHooks{}
	_ extensions.InteractiveToolHostHooks = extHooks{}
	_ extensions.AlertHostHooks           = extHooks{}
	_ extensions.ExtensionChromeHooks     = extHooks{}
)

// ExtensionHooks returns the adapter assembly passes to the extension manager.
func (c *Controller) ExtensionHooks() extensions.HostHooks { return extHooks{c} }

// mutate admits at most 1024 hooks without waiting for the actor. Overload
// reporting has one coalesced slot and is limited to one note per second.
func (h extHooks) mutate(fn func()) { h.admit("extensions", fn) }
func (h extHooks) admit(owner string, fn func()) {
	s := &h.c.ext
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	if len(s.mailbox) >= 1024 {
		s.markOverload(owner)
	} else {
		s.mailbox = append(s.mailbox, fn)
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// markOverload requires mu. A single slot bounds reporting even under floods.
func (s *extState) markOverload(owner string) {
	if s.overload == "" && s.clock().Sub(s.lastOverload) >= time.Second {
		s.overload = owner
	}
}
func (c *Controller) extInit(parent context.Context) {
	s := &c.ext
	s.wake = make(chan struct{}, 1)
	s.clock = c.deps.Clock
	s.domainJobs = make(chan func(context.Context, Port) error, 64)
	s.dispatcherDone = make(chan struct{})
	s.ctx, s.cancel = context.WithCancel(parent)
	go func() {
		defer close(s.dispatcherDone)
		for {
			select {
			case <-s.ctx.Done():
				return
			case job := <-s.domainJobs:
				s.mu.Lock()
				if s.stopped || s.ctx.Err() != nil {
					s.mu.Unlock()
					return
				}
				port := s.port
				if port == nil {
					port = c
				}
				s.mu.Unlock()
				if err := job(s.ctx, port); err != nil {
					extHooks{c}.Notify("extensions", "error", err.Error())
				}
			}
		}
	}()
}

// extShutdown closes admission atomically, discards queued hooks and cancels
// running domain work. Close may call it repeatedly without waiting for work.
func (c *Controller) extShutdown() {
	s := &c.ext
	s.mu.Lock()
	s.stopped = true
	s.mailbox = nil
	s.overload = ""
	s.cancel()
	for {
		select {
		case <-s.domainJobs:
		default:
			s.mu.Unlock()
			return
		}
	}
}
func (c *Controller) extDrain() {
	s := &c.ext
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	jobs := s.mailbox
	s.mailbox = nil
	owner := s.overload
	s.overload = ""
	if owner != "" {
		s.lastOverload = s.clock()
	}
	s.mu.Unlock()
	for _, job := range jobs {
		s.mu.Lock()
		stopped := s.stopped
		s.mu.Unlock()
		if stopped {
			return
		}
		job()
	}
	if owner != "" {
		extHooks{c}.note(owner, "error", "extension "+owner+" overloaded: hook dropped", false)
	}
}

// domain is actor-only and uses one lifetime-owned dispatcher, never one
// goroutine per request. A full domain queue drops work without blocking.
func (h extHooks) domain(fn func(context.Context, Port) error) {
	s := &h.c.ext
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	select {
	case s.domainJobs <- fn:
	default:
		s.markOverload("extensions")
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// maxExtNotes bounds retained extension notes per session; oldest drop first.
const maxExtNotes = 200

func (h extHooks) appendNote(n ownedExtNote) {
	notes := append(h.c.ext.notes, n)
	if len(notes) > maxExtNotes {
		notes = append([]ownedExtNote(nil), notes[len(notes)-maxExtNotes:]...)
	}
	h.c.ext.notes = notes
}
func (h extHooks) note(owner, level, text string, prefix bool) {
	if prefix {
		text = "[" + owner + "] " + text
	}
	n := Note{Level: level, Text: text}
	h.appendNote(ownedExtNote{owner, n})
	h.c.publish(EvNote, NoteEvent{Note: n})
}
func (h extHooks) chrome() {
	h.c.publish(EvExtChrome, ExtChromeEvent{ExtChrome: cloneChrome(h.c.ext.chrome)})
}
func (h extHooks) Notify(ext, level, text string) {
	if text == "" {
		return
	}
	h.admit(ext, func() { h.note(ext, level, text, true) })
}
func (h extHooks) Display(ext, text string) {
	if text == "" {
		return
	}
	h.admit(ext, func() {
		h.appendNote(ownedExtNote{ext, Note{Level: "info", Text: "[" + ext + "] " + text}})
		h.c.publish(EvExtDisplay, ExtDisplayEvent{Ext: ext, Text: text})
	})
}

// ClearNotes publishes a snapshot refresh because the event contract has no
// note-removal payload. It does not alter persisted transcript messages.
func (h extHooks) ClearNotes(ext string) {
	h.admit(ext, func() { h.clearNotes(ext); h.c.publish(EvMessagesReplaced, MessagesReplacedEvent{}) })
}
func (h extHooks) clearNotes(ext string) {
	var kept []ownedExtNote
	for _, n := range h.c.ext.notes {
		if n.ext != ext {
			kept = append(kept, n)
		}
	}
	h.c.ext.notes = kept
}
func (h extHooks) Submit(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	h.mutate(func() {
		h.domain(func(ctx context.Context, p Port) error {
			return p.Submit(ctx, Input{Text: text, Origin: InputOriginExtension})
		})
	})
}
func (h extHooks) Insert(text string) {
	h.mutate(func() { h.c.publish(EvExtInsert, ExtInsertEvent{Text: text}) })
}
func (h extHooks) SupportsInteractiveTools() bool { return h.c.Info().Attach == AttachStateAttached }

// SubmitSlash delegates attached routing to the client. Detached routing only
// recognizes domain commands backed by Port; UI commands remain review notes.
func (h extHooks) SubmitSlash(text string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return
	}
	h.mutate(func() {
		if h.c.attach == AttachStateAttached {
			h.c.publish(EvExtInsert, ExtInsertEvent{Text: text, Submit: true})
			return
		}
		h.detachedSlash(text)
	})
}
func (h extHooks) detachedSlash(text string) {
	fields := strings.Fields(text)
	arg := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	switch fields[0] {
	case "/compact":
		h.domain(func(ctx context.Context, p Port) error { return p.Compact(ctx) })
	case "/model":
		p, m, ok := strings.Cut(arg, "/")
		if !ok {
			m = p
			p = ""
		}
		if m == "" {
			h.note("extensions", "info", text, true)
			return
		}
		h.domain(func(ctx context.Context, port Port) error { return port.SetModel(ctx, p, m) })
	case "/goal":
		cmd := GoalCommand{Op: "set", Objective: arg}
		switch arg {
		case "pause", "resume", "clear", "status", "history":
			cmd = GoalCommand{Op: arg}
		}
		if arg == "" {
			cmd.Op = "status"
		}
		h.domain(func(ctx context.Context, p Port) error { return p.SetGoal(ctx, cmd) })
	default:
		h.note("extensions", "info", text, true)
		return
	}
}
func (h extHooks) SetStatus(ext, key, level, text string) {
	if strings.TrimSpace(ext) == "" || strings.TrimSpace(key) == "" {
		return
	}
	h.admit(ext, func() {
		s := &h.c.ext.chrome
		if s.Statuses == nil {
			s.Statuses = map[string]map[string]ExtStatus{}
		}
		if s.Statuses[ext] == nil {
			s.Statuses[ext] = map[string]ExtStatus{}
		}
		if strings.TrimSpace(text) == "" {
			delete(s.Statuses[ext], key)
			if len(s.Statuses[ext]) == 0 {
				delete(s.Statuses, ext)
			}
		} else {
			s.Statuses[ext][key] = ExtStatus{Level: level, Text: text}
		}
		h.chrome()
	})
}
func (h extHooks) SetWidget(ext, id, position, title string, lines []string) {
	if strings.TrimSpace(ext) == "" || strings.TrimSpace(id) == "" {
		return
	}
	lines = append([]string(nil), lines...)
	h.admit(ext, func() {
		s := &h.c.ext.chrome
		if s.Widgets == nil {
			s.Widgets = map[string]map[string]ExtWidget{}
		}
		if s.Widgets[ext] == nil {
			s.Widgets[ext] = map[string]ExtWidget{}
		}
		s.Widgets[ext][id] = ExtWidget{Position: extproto.NormalizeWidgetPosition(position), Title: title, Lines: lines}
		h.chrome()
	})
}
func (h extHooks) ClearWidget(ext, id string) {
	h.admit(ext, func() {
		delete(h.c.ext.chrome.Widgets[ext], id)
		if len(h.c.ext.chrome.Widgets[ext]) == 0 {
			delete(h.c.ext.chrome.Widgets, ext)
		}
		h.chrome()
	})
}
func clonePanel(p ExtPanel) ExtPanel { p.Spec.Lines = append([]string(nil), p.Spec.Lines...); return p }
func (h extHooks) OpenPanel(ext string, spec extproto.PanelSpec) {
	spec.Lines = append([]string(nil), spec.Lines...)
	h.admit(ext, func() {
		p := ExtPanel{Ext: ext, Spec: spec}
		h.c.ext.chrome.Panels = []ExtPanel{p}
		h.c.publish(EvExtPanel, ExtPanelEvent{Op: "open", Panel: clonePanel(p)})
	})
}
func (h extHooks) UpdatePanel(ext, id, title string, lines []string, footer string) {
	lines = append([]string(nil), lines...)
	h.admit(ext, func() {
		for i, p := range h.c.ext.chrome.Panels {
			if p.Ext == ext && p.Spec.ID == id {
				p.Spec.Title = title
				p.Spec.Lines = lines
				p.Spec.Footer = footer
				h.c.ext.chrome.Panels[i] = p
				h.c.publish(EvExtPanel, ExtPanelEvent{Op: "update", Panel: clonePanel(p)})
			}
		}
	})
}
func (h extHooks) ClosePanel(ext, id string) {
	h.admit(ext, func() {
		for i, p := range h.c.ext.chrome.Panels {
			if p.Ext == ext && p.Spec.ID == id {
				h.c.ext.chrome.Panels = append(h.c.ext.chrome.Panels[:i], h.c.ext.chrome.Panels[i+1:]...)
				h.c.publish(EvExtPanel, ExtPanelEvent{Op: "close", Panel: clonePanel(p)})
				break
			}
		}
	})
}
func (h extHooks) Alert(ext string, alert extproto.AlertRequest) {
	h.admit(ext, func() {
		copy := alert
		h.c.ext.chrome.Alert = &copy
		h.c.ext.alertOwner = ext
		h.c.publish(EvExtAlert, ExtAlertEvent{Alert: alert})
	})
}
func (h extHooks) ClearExtensionChrome(ext string) {
	h.admit(ext, func() {
		delete(h.c.ext.chrome.Statuses, ext)
		delete(h.c.ext.chrome.Widgets, ext)
		var panels []ExtPanel
		for _, p := range h.c.ext.chrome.Panels {
			if p.Ext != ext {
				panels = append(panels, p)
			}
		}
		h.c.ext.chrome.Panels = panels
		if h.c.ext.alertOwner == ext {
			h.c.ext.chrome.Alert = nil
			h.c.ext.alertOwner = ""
		}
		h.clearNotes(ext)
		h.chrome()
		h.c.publish(EvMessagesReplaced, MessagesReplacedEvent{})
	})
}

// ReloadExtensions performs blocking manager work outside the actor so manager
// callbacks may safely call the extension hooks during reload.
func (c *Controller) ReloadExtensions(ctx context.Context) error {
	if c.deps.Extensions == nil {
		return errors.New("extensions unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stats := c.deps.Extensions.Reload(ctx, 5*time.Second)
	err := errors.Join(stats.Errors...)
	level := "info"
	if err != nil {
		level = "error"
	}
	message := fmt.Sprintf("reloaded: %d stopped, %d loaded (%d ready)", stats.Stopped, stats.Loaded, stats.Ready)
	if len(stats.Errors) > 0 {
		message += fmt.Sprintf(", %d error(s)", len(stats.Errors))
		var details []string
		for _, failure := range stats.Errors {
			if failure != nil {
				details = append(details, failure.Error())
			}
		}
		if len(details) > 0 {
			message += ": " + strings.Join(details, "; ")
		}
	}
	c.ExtensionHooks().Notify("extensions", level, message)
	return err
}

// InvokeExtensionCommand applies existing extension command response actions.
func (c *Controller) InvokeExtensionCommand(ctx context.Context, name, args string) error {
	if c.deps.Extensions == nil {
		return errors.New("extensions unavailable")
	}
	if !c.deps.Extensions.HasCommand(name) {
		return fmt.Errorf("unknown extension command: %s", name)
	}
	resp, err := c.deps.Extensions.Invoke(ctx, name, args, 30*time.Second)
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return errors.New(resp.Error)
	}
	h := c.ExtensionHooks()
	owner := c.deps.Extensions.CommandOwner(name)
	if owner == "" {
		owner = name
	}
	switch resp.Action {
	case "open_panel":
		if resp.OpenPanel != nil {
			h.OpenPanel(owner, *resp.OpenPanel)
		}
	case "prompt":
		if strings.TrimSpace(resp.Prompt) != "" {
			return c.Submit(ctx, Input{Text: resp.Prompt, Origin: InputOriginExtension})
		}
	case "insert":
		h.Insert(resp.Insert)
	case "display":
		h.Display(owner, resp.Display)
	case "noop", "":
	default:
		return fmt.Errorf("extension /%s: unknown action %s", name, resp.Action)
	}
	return nil
}

// ExtensionPanelInput forwards keys, or a close frame when Type is panel_close.
func (c *Controller) ExtensionPanelInput(ctx context.Context, ext, id string, key extproto.PanelKeyFromHost) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.deps.Extensions == nil {
		return errors.New("extensions unavailable")
	}
	if key.Type == "panel_close" {
		if err := c.deps.Extensions.SendPanelClose(ext, id); err != nil {
			return err
		}
		c.ExtensionHooks().ClosePanel(ext, id)
		return nil
	}
	return c.deps.Extensions.SendPanelKey(ext, id, key.Key, key.Text)
}
