package daemon

import (
	"context"
	"fmt"
	"sort"

	"github.com/bnema/zut/packages/agent/session"
)

var _ session.Directory = (*Daemon)(nil)

// List returns Info for every open session, ordered by ID.
func (d *Daemon) List(context.Context) []session.Info {
	es := d.entries()
	out := make([]session.Info, 0, len(es))
	for _, e := range es {
		out = append(out, e.h.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Tree groups open sessions by Info.RepoRoot, falling back to CWD.
func (d *Daemon) Tree(ctx context.Context) session.Tree {
	groups := map[string][]session.SessionNode{}
	var roots []string
	for _, info := range d.List(ctx) {
		root := info.RepoRoot
		if root == "" {
			root = info.CWD
		}
		if _, ok := groups[root]; !ok {
			roots = append(roots, root)
		}
		groups[root] = append(groups[root], session.SessionNode{Info: info, Residents: info.Residents})
	}
	sort.Strings(roots)
	tree := session.Tree{Workspaces: make([]session.WorkspaceNode, 0, len(roots))}
	for _, r := range roots {
		tree.Workspaces = append(tree.Workspaces, session.WorkspaceNode{RepoRoot: r, Sessions: groups[r]})
	}
	return tree
}

// Send validates D10 and queues text for asynchronous delivery to the
// target as an inter_session input. The sender must be attached and its
// current turn must not originate from an inter-session message. Delivery
// never waits on a session loop, so a session may call Send from its own
// actor; a full delivery queue returns session.ErrBusy.
func (d *Daemon) Send(ctx context.Context, from, to session.ID, text string) error {
	src := d.lookup(from)
	if src == nil {
		return fmt.Errorf("sender %s: %w", from, ErrUnknownSession)
	}
	info := src.h.Info()
	if info.Attach != session.AttachStateAttached {
		return session.ErrNotAttached
	}
	if info.CurrentOrigin == session.InputOriginInterSession {
		return session.ErrLoopGuard
	}
	dst := d.lookup(to)
	if dst == nil {
		return fmt.Errorf("target %s: %w", to, ErrUnknownSession)
	}
	sender := &session.Sender{SessionID: from, Title: info.Title, CWD: info.CWD}
	in := session.Input{
		Text:   fmt.Sprintf("[message from session %s (%s, %s)]\n%s", from, info.Title, info.CWD, text),
		Mode:   session.SubmitModeAuto,
		Origin: session.InputOriginInterSession,
		From:   sender,
	}
	return d.enqueueDelivery(ctx, dst, in)
}

// enqueueDelivery queues in on the target's own bounded queue, starting its
// worker lazily, so a stuck target cannot delay deliveries to other targets.
func (d *Daemon) enqueueDelivery(ctx context.Context, dst *entry, in session.Input) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopping || d.ctx == nil || d.reg[dst.id] != dst {
		return fmt.Errorf("target %s: %w", dst.id, ErrUnknownSession)
	}
	select {
	case <-dst.h.Done():
		return fmt.Errorf("target %s: %w", dst.id, session.ErrClosed)
	default:
	}
	q := d.deliverers[dst.id]
	if q == nil {
		q = make(chan session.Input, deliveryQueueSize)
		d.deliverers[dst.id] = q
		d.goLoop(func(ctx context.Context) { d.deliveryWorker(ctx, dst, q) })
	}
	select {
	case q <- in:
		return nil
	default:
		return fmt.Errorf("inter-session delivery queue for %s full: %w", dst.id, session.ErrBusy)
	}
}

// deliveryWorker submits queued inputs to one target until the daemon stops
// or the target ends; it unregisters its queue under d.mu before exiting so
// no input is queued to a dead worker.
func (d *Daemon) deliveryWorker(ctx context.Context, dst *entry, q chan session.Input) {
	defer func() {
		d.mu.Lock()
		if d.deliverers[dst.id] == q {
			delete(d.deliverers, dst.id)
		}
		d.mu.Unlock()
	}()
	done := dst.h.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case in := <-q:
			sctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
			if err := dst.h.Submit(sctx, in); err != nil {
				d.log.Warn("inter-session delivery failed", "session", dst.id, "err", err)
			}
			cancel()
		}
	}
}
