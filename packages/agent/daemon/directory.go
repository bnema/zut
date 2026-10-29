package daemon

import (
	"context"
	"fmt"
	"sort"

	"github.com/bnema/zut/packages/agent/session"
)

var _ session.Directory = (*Daemon)(nil)

type delivery struct {
	target *entry
	in     session.Input
}

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
	select {
	case d.deliver <- delivery{target: dst, in: in}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return fmt.Errorf("inter-session delivery queue full: %w", session.ErrBusy)
	}
}

func (d *Daemon) deliveryLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-d.deliver:
			sctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
			if err := m.target.h.Submit(sctx, m.in); err != nil {
				d.log.Warn("inter-session delivery failed", "session", m.target.id, "err", err)
			}
			cancel()
		}
	}
}
