package modes

import (
	"context"
	"fmt"
	"os"

	"github.com/bnema/zut/packages/agent/internal/sessionindex"
	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
)

// pendingFinderJump is the in-chat jump to run once the session opened from
// the finder becomes current. Guarded by Interactive.mu.
type pendingFinderJump struct {
	path     string // session expected to become current
	hit      *sessionindex.HitRef
	terms    []string
	note     string // status shown once the jump ran (e.g. after a fork)
	forkPath string // fork written for this open; removed if the resume fails
}

// finderOpenItem resolves the picked session off the input loop, then
// resumes it or asks how to. Picking the current session jumps in place.
func (i *Interactive) finderOpenItem(item finderItem, hit int) {
	jump := &pendingFinderJump{path: item.Key}
	i.mu.Lock()
	if i.finder != nil {
		jump.terms, _ = sessionindex.ParseQuery(i.finder.Query())
	}
	i.mu.Unlock()
	if hit >= 0 && hit < len(item.Hits) {
		if ref, ok := item.Hits[hit].Ref.(sessionindex.HitRef); ok {
			jump.hit = &ref
		}
	}
	port := i.portMode.Load()
	if i.isCurrentFinderSession(item.Key) || (port && item.Live && item.LiveID != "" && session.ID(item.LiveID) == i.SessionID()) {
		i.closeFinder()
		i.finderJumpHere(jump)
		return
	}
	if port && item.Live {
		// Live sessions attach; they need not live in this namespace's
		// root, so skip ManagedSessionMeta and use the daemon's cwd.
		i.mu.Lock()
		plan := resumePlan{Path: item.Key, LiveID: item.LiveID, TargetCWD: item.LiveCWD, CurrentCWD: i.cfg.CWD, Live: true}
		i.pendingFinderJump = jump
		i.mu.Unlock()
		i.closeFinder()
		i.openFinderResume(plan)
		return
	}
	if i.finderResumeBusy() {
		return
	}
	root, path := i.sessionsRoot(), item.Key
	i.closeFinder()
	i.runFinderAsync("opening session…", func(ctx context.Context) func() {
		meta, err := core.ManagedSessionMeta(ctx, root, path)
		if err != nil {
			return func() { i.setFinderStatusErr("finder: cannot open session: " + err.Error()) }
		}
		exists := false
		if meta.CWD != "" {
			if st, statErr := os.Stat(meta.CWD); statErr == nil && st.IsDir() {
				exists = true
			}
		}
		return func() {
			i.mu.Lock()
			plan := resumePlan{Path: path, TargetCWD: meta.CWD, CurrentCWD: i.cfg.CWD, CWDExists: exists}
			i.pendingFinderJump = jump
			i.statusOK = ""
			i.mu.Unlock()
			i.openFinderResume(plan)
		}
	})
}

// runFinderAsync runs work on a goroutine with sessionLoading set, so the
// busy guard refuses other resumes meanwhile, and applies its result on the
// Run loop.
func (i *Interactive) runFinderAsync(status string, work func(context.Context) func()) {
	ctx := i.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	i.mu.Lock()
	i.sessionLoading = true
	i.statusOK, i.statusErr = status, ""
	i.mu.Unlock()
	i.invalidate()
	go func() {
		apply := work(ctx)
		select {
		case i.loopCalls <- func() {
			i.mu.Lock()
			i.sessionLoading = false
			i.mu.Unlock()
			apply()
			i.invalidate()
		}:
		case <-ctx.Done():
		}
	}()
}

// isCurrentFinderSession reports whether path is the attached session.
func (i *Interactive) isCurrentFinderSession(path string) bool {
	cur := i.currentSessionPath()
	return cur != "" && path != "" && normalizedSessionPath(cur) == normalizedSessionPath(path)
}

// finderJumpHere runs jump in the current session without reloading it.
func (i *Interactive) finderJumpHere(jump *pendingFinderJump) {
	cur := i.currentSessionPath()
	jump.path = cur
	i.mu.Lock()
	i.pendingFinderJump = jump
	i.mu.Unlock()
	i.runPendingFinderJump(cur)
}

// finderExecuteResume runs the chosen way to open plan.
func (i *Interactive) finderExecuteResume(plan resumePlan, choice resumeChoice) {
	if choice == resumeChoiceCancel || choice == resumeChoiceNone {
		i.dropPendingFinderJump()
		return
	}
	port := i.portMode.Load()
	if i.isCurrentFinderSession(plan.Path) || (port && plan.Live && plan.LiveID != "" && session.ID(plan.LiveID) == i.SessionID()) {
		i.mu.Lock()
		jump := i.pendingFinderJump
		i.mu.Unlock()
		if jump == nil {
			jump = &pendingFinderJump{}
		}
		i.finderJumpHere(jump)
		return
	}
	attach := choice == resumeChoiceAttach || (choice == resumeChoiceResume && port && plan.Live && plan.LiveID != "")
	if !attach && i.finderResumeBusy() {
		return
	}
	switch choice {
	case resumeChoiceResume:
		switch {
		case !port:
			i.applySessionSelection(plan.Path)
		case plan.Live && plan.LiveID != "":
			i.portAttachSession(session.ID(plan.LiveID))
		default:
			i.portResumeSession(plan.Path, plan.CurrentCWD)
		}
	case resumeChoiceSwitch:
		if port {
			i.portResumeSession(plan.Path, plan.TargetCWD)
		} else {
			i.applySessionSelection(plan.Path)
		}
	case resumeChoiceAttach:
		if !port {
			i.dropPendingFinderJump()
			i.setFinderStatusErr("finder: attaching a live session needs the daemon")
			return
		}
		i.portAttachSession(session.ID(plan.LiveID))
	case resumeChoiceFork:
		i.finderFork(plan, port)
	}
}

// finderFork copies plan's session into the current directory off the
// input loop, then resumes the copy.
func (i *Interactive) finderFork(plan resumePlan, port bool) {
	if !port && i.cfg.LoadSession == nil {
		i.dropPendingFinderJump()
		i.setFinderStatusErr("session loading is not wired in this build")
		return
	}
	root, version := i.sessionsRoot(), i.cfg.Version
	i.runFinderAsync("forking session…", func(ctx context.Context) func() {
		newPath, err := core.ForkSessionToCWD(ctx, plan.Path, root, plan.CurrentCWD, version)
		if err != nil {
			return func() {
				i.dropPendingFinderJump()
				i.setFinderStatusErr("fork: " + err.Error())
			}
		}
		return func() {
			// Same messages in the fork, so the hit fingerprint still resolves.
			i.mu.Lock()
			if i.pendingFinderJump == nil {
				i.pendingFinderJump = &pendingFinderJump{}
			}
			i.pendingFinderJump.path = newPath
			i.pendingFinderJump.forkPath = newPath
			i.pendingFinderJump.note = fmt.Sprintf("forked into %s (new session)", friendlyPath(plan.CurrentCWD))
			i.statusOK = ""
			i.mu.Unlock()
			if port {
				i.portResumeSession(newPath, plan.CurrentCWD)
			} else {
				i.applySessionSelection(newPath)
			}
		}
	})
}

// failPendingFinderJumpLocked drops the pending jump after its resume failed
// and returns a fork file written for it, which the caller removes without
// holding i.mu.
func (i *Interactive) failPendingFinderJumpLocked() (orphan string) {
	if i.pendingFinderJump != nil {
		orphan = i.pendingFinderJump.forkPath
	}
	i.pendingFinderJump = nil
	return orphan
}

func removeFinderOrphanFork(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

func (i *Interactive) dropPendingFinderJump() {
	i.mu.Lock()
	i.pendingFinderJump = nil
	i.mu.Unlock()
}

// queuePendingFinderJump hands the pending jump to the Run loop after a
// local session load finished on a goroutine.
func (i *Interactive) queuePendingFinderJump(path string, err error) {
	i.mu.Lock()
	has := i.pendingFinderJump != nil
	orphan := ""
	if has && err != nil {
		orphan = i.failPendingFinderJumpLocked()
		has = false
	}
	i.mu.Unlock()
	removeFinderOrphanFork(orphan)
	if !has || i.loopCalls == nil {
		return
	}
	ctx := i.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case i.loopCalls <- func() { i.runPendingFinderJump(path) }:
	case <-ctx.Done():
	}
}

// runPendingFinderJump opens the search reader on the pending jump's hit
// when current is the session it was waiting for. The pending jump is
// always consumed.
func (i *Interactive) runPendingFinderJump(current string) {
	i.mu.Lock()
	jump := i.pendingFinderJump
	i.pendingFinderJump = nil
	if jump == nil {
		i.mu.Unlock()
		return
	}
	msgs := i.view.Messages
	i.mu.Unlock()
	if current == "" ||
		normalizedSessionPath(current) != normalizedSessionPath(jump.path) {
		return
	}
	note := jump.note
	// Only a message hit locates a message; title or session-level matches
	// open no reader.
	if jump.hit != nil && len(jump.terms) > 0 {
		focus, ok := resolveHitInView(msgs, jump.hit.Fingerprint, jump.hit.MsgSeq, jump.hit.TotalMsgs)
		if !ok {
			// Nearest other match to where the hit was.
			focus = max(0, len(msgs)-(jump.hit.TotalMsgs-jump.hit.MsgSeq))
			note = joinStatus(note, "match is in compacted history")
		}
		i.openSearchReader(jump.terms, focus)
	}
	if note == "" {
		return
	}
	i.mu.Lock()
	i.statusErr, i.statusOK = "", note
	i.mu.Unlock()
	i.invalidate()
}

func joinStatus(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + " · " + b
}

const finderResumeBusyMsg = "wait for the active work to finish before resuming a session"

// finderResumeBusy refuses a resume, switch, or fork while work is active,
// dropping the pending jump. A local build without session loading is let
// through so applySessionSelection reports that instead.
func (i *Interactive) finderResumeBusy() bool {
	if !i.portMode.Load() && i.cfg.LoadSession == nil {
		return false
	}
	if i.canResumeSessionSelection() {
		return false
	}
	i.dropPendingFinderJump()
	i.setFinderStatusErr(finderResumeBusyMsg)
	return true
}
