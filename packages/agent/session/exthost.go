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
	mu         sync.Mutex
	mailbox    []func()
	wake       chan struct{}
	port       Port
	domainTail <-chan struct{}
	chrome     ExtChrome
	notes      []ownedExtNote
	alertOwner string
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

// mutate never waits for the actor, including when a manager callback originates
// on that actor. The mailbox is lossless and preserves enqueue order.
func (h extHooks) mutate(fn func()) {
	s := &h.c.ext
	s.mu.Lock()
	s.mailbox = append(s.mailbox, fn)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *extState) init() { s.wake = make(chan struct{}, 1) }
func (s *extState) drain() {
	s.mu.Lock()
	jobs := s.mailbox
	s.mailbox = nil
	s.mu.Unlock()
	for _, job := range jobs {
		job()
	}
}

// domain runs Port calls off-actor, in mailbox order, because Port calls may
// themselves wait for the actor. Tests use the existing generated Port mock.
func (h extHooks) domain(fn func(Port) error) {
	previous := h.c.ext.domainTail
	done := make(chan struct{})
	h.c.ext.domainTail = done
	port := h.c.ext.port
	if port == nil {
		port = h.c
	}
	go func() {
		defer close(done)
		if previous != nil {
			select {
			case <-previous:
			case <-h.c.done:
				return
			}
		}
		if err := fn(port); err != nil {
			h.Notify("extensions", "error", err.Error())
		}
	}()
}
func (h extHooks) chrome() {
	h.c.publish(EvExtChrome, ExtChromeEvent{ExtChrome: cloneChrome(h.c.ext.chrome)})
}
func (h extHooks) Notify(ext, level, text string) {
	if text == "" {
		return
	}
	h.mutate(func() {
		n := Note{Level: level, Text: "[" + ext + "] " + text}
		h.c.ext.notes = append(h.c.ext.notes, ownedExtNote{ext, n})
		h.c.publish(EvNote, NoteEvent{Note: n})
	})
}
func (h extHooks) Display(ext, text string) {
	if text == "" {
		return
	}
	h.mutate(func() {
		h.c.ext.notes = append(h.c.ext.notes, ownedExtNote{ext, Note{Level: "info", Text: "[" + ext + "] " + text}})
		h.c.publish(EvExtDisplay, ExtDisplayEvent{Ext: ext, Text: text})
	})
}

// ClearNotes publishes a snapshot refresh because the event contract has no
// note-removal payload. It does not alter persisted transcript messages.
func (h extHooks) ClearNotes(ext string) {
	h.mutate(func() { h.clearNotes(ext); h.c.publish(EvMessagesReplaced, MessagesReplacedEvent{}) })
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
		h.domain(func(p Port) error {
			return p.Submit(context.Background(), Input{Text: text, Origin: InputOriginExtension})
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
		h.domain(func(p Port) error { return h.detachedSlash(p, text) })
	})
}
func (h extHooks) detachedSlash(pport Port, text string) error {
	fields := strings.Fields(text)
	arg := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	var err error
	switch fields[0] {
	case "/compact":
		err = pport.Compact(context.Background())
	case "/model":
		p, m, ok := strings.Cut(arg, "/")
		if !ok {
			m = p
			p = ""
		}
		if m == "" {
			h.Notify("extensions", "info", text)
			return nil
		}
		err = pport.SetModel(context.Background(), p, m)
	case "/goal":
		cmd := GoalCommand{Op: "set", Objective: arg}
		switch arg {
		case "pause", "resume", "clear", "status", "history":
			cmd = GoalCommand{Op: arg}
		}
		if arg == "" {
			cmd.Op = "status"
		}
		err = pport.SetGoal(context.Background(), cmd)
	default:
		h.Notify("extensions", "info", text)
		return nil
	}
	return err
}
func (h extHooks) SetStatus(ext, key, level, text string) {
	if strings.TrimSpace(ext) == "" || strings.TrimSpace(key) == "" {
		return
	}
	h.mutate(func() {
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
	h.mutate(func() {
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
	h.mutate(func() {
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
	h.mutate(func() {
		p := ExtPanel{Ext: ext, Spec: spec}
		h.c.ext.chrome.Panels = []ExtPanel{p}
		h.c.publish(EvExtPanel, ExtPanelEvent{Op: "open", Panel: clonePanel(p)})
	})
}
func (h extHooks) UpdatePanel(ext, id, title string, lines []string, footer string) {
	lines = append([]string(nil), lines...)
	h.mutate(func() {
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
	h.mutate(func() {
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
	h.mutate(func() {
		copy := alert
		h.c.ext.chrome.Alert = &copy
		h.c.ext.alertOwner = ext
		h.c.publish(EvExtAlert, ExtAlertEvent{Alert: alert})
	})
}
func (h extHooks) ClearExtensionChrome(ext string) {
	h.mutate(func() {
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
