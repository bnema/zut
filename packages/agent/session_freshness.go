package agent

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// openOrCreateSessionState distinguishes creation from restoration even when
// the restored transcript is empty. Fresh runs may preload pinned skills.
func openOrCreateSessionState(ctx context.Context, args Args, r Resolved, ag *core.Agent, version string) (*core.Session, bool, error) {
	if args.NoSess {
		ag.SessionID()
		return nil, true, nil
	}
	root := agentSessionsRoot(ZutHome(), args)
	// Explicit paths and UUIDs may identify empty sessions. Don't delete them
	// before deciding whether this is a restoration.
	if args.ResumeSessionID == "" && args.Session == "" {
		core.PruneEmptySessions(root, args.CWD)
	}
	var (
		sess     *core.Session
		messages []provider.Message
		err      error
		fresh    bool
	)
	switch {
	case args.Session != "":
		sess, messages, err = core.OpenSession(args.Session)
		if errors.Is(err, os.ErrNotExist) {
			sess, err = core.NewSessionAtPath(args.Session, args.CWD, r.Provider, r.Model, version)
			messages, fresh = nil, true
		}
	case args.Continue:
		if path := core.LatestSession(root, args.CWD); path != "" {
			sess, messages, err = core.OpenSession(path)
		}
	case args.Resume:
		var path string
		if args.ResumeSessionID != "" {
			path, err = core.FindManagedSessionByID(ctx, ZutHome(), args.ResumeSessionID)
			if err != nil {
				return nil, false, err
			}
			if path == "" {
				return nil, false, fmt.Errorf("session %q not found", args.ResumeSessionID)
			}
		} else {
			path, err = pickSession(root, args.CWD)
			if err != nil {
				return nil, false, err
			}
		}
		if path != "" {
			sess, messages, err = core.OpenSession(path)
			if err == nil && args.ResumeSessionID != "" && sess.ID != args.ResumeSessionID {
				return nil, false, errors.Join(fmt.Errorf("session %q changed during resume", args.ResumeSessionID), sess.Close())
			}
		}
	}
	if err != nil {
		return nil, false, err
	}
	if sess == nil {
		sess, err = core.NewSession(root, args.CWD, r.Provider, r.Model, version)
		if err != nil {
			return nil, false, err
		}
		fresh = true
	}
	if err := ag.BindSessionID(sess.Meta.ID); err != nil {
		return nil, false, errors.Join(err, sess.Close())
	}
	ag.SetSessionTimeContext(sess.Meta.Started, sess.Meta.Timezone, sess.Meta.TimezoneOffset)
	if !fresh {
		ag.SetMessages(messages)
		if cumulative, last, err := core.SessionUsageDetail(sess.Path); err == nil {
			ag.SeedCost(cumulative)
			ag.SeedLastTurnUsage(last)
		}
	}
	return sess, fresh, nil
}
