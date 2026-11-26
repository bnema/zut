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
	path  string // session expected to become current
	hit   *sessionindex.HitRef
	terms []string
	note  string // status shown once the jump ran (e.g. after a fork)
}

// finderOpenItem validates the picked session, closes the finder, records
// the pending jump, and resumes (or asks how to resume) the session.
func (i *Interactive) finderOpenItem(item finderItem, hit int) {
	ctx := i.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	meta, err := core.ManagedSessionMeta(ctx, i.sessionsRoot(), item.Key)
	if err != nil {
		i.setFinderStatusErr("finder: cannot open session: " + err.Error())
		return
	}
	exists := false
	if meta.CWD != "" {
		if st, statErr := os.Stat(meta.CWD); statErr == nil && st.IsDir() {
			exists = true
		}
	}
	if !(item.Live && i.portMode.Load()) && i.finderResumeBusy() {
		return
	}
	i.mu.Lock()
	plan := resumePlan{
		Path:       item.Key,
		LiveID:     item.LiveID,
		TargetCWD:  meta.CWD,
		CurrentCWD: i.cfg.CWD,
		CWDExists:  exists,
		Live:       item.Live,
	}
	jump := &pendingFinderJump{path: item.Key}
	if i.finder != nil {
		jump.terms, _ = sessionindex.ParseQuery(i.finder.Query())
	}
	if hit >= 0 && hit < len(item.Hits) {
		if ref, ok := item.Hits[hit].Ref.(sessionindex.HitRef); ok {
			jump.hit = &ref
		}
	}
	i.pendingFinderJump = jump
	i.mu.Unlock()
	i.closeFinder()
	i.openFinderResume(plan)
}

// finderExecuteResume runs the chosen way to open plan.
func (i *Interactive) finderExecuteResume(plan resumePlan, choice resumeChoice) {
	port := i.portMode.Load()
	attach := choice == resumeChoiceAttach || (choice == resumeChoiceResume && port && plan.Live && plan.LiveID != "")
	if !attach && choice != resumeChoiceCancel && i.finderResumeBusy() {
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
	default:
		i.dropPendingFinderJump()
	}
}

func (i *Interactive) finderFork(plan resumePlan, port bool) {
	ctx := i.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	newPath, err := core.ForkSessionToCWD(ctx, plan.Path, i.sessionsRoot(), plan.CurrentCWD, i.cfg.Version)
	if err != nil {
		i.dropPendingFinderJump()
		i.mu.Lock()
		i.statusErr, i.statusOK = "fork: "+err.Error(), ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	// Same messages in the fork, so the hit fingerprint still resolves.
	note := fmt.Sprintf("forked into %s (new session)", friendlyPath(plan.CurrentCWD))
	i.mu.Lock()
	if i.pendingFinderJump == nil {
		i.pendingFinderJump = &pendingFinderJump{}
	}
	i.pendingFinderJump.path = newPath
	i.pendingFinderJump.note = note
	i.mu.Unlock()
	if port {
		i.portResumeSession(newPath, plan.CurrentCWD)
	} else {
		i.applySessionSelection(newPath)
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
	if has && err != nil {
		i.pendingFinderJump = nil
		has = false
	}
	i.mu.Unlock()
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

// runPendingFinderJump starts the in-chat search for the pending jump when
// current is the session it was waiting for. The pending jump is always
// consumed.
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
	focus, note := len(msgs)-1, jump.note
	if jump.hit != nil {
		if idx, ok := resolveHitInView(msgs, jump.hit.Fingerprint, jump.hit.MsgSeq, jump.hit.TotalMsgs); ok {
			focus = idx
		} else {
			note = joinStatus(note, "match is in compacted history")
		}
	}
	if len(jump.terms) > 0 {
		i.startChatSearch(jump.terms, focus)
	}
	if note == "" {
		return
	}
	i.mu.Lock()
	if i.chatSearch.active {
		i.statusErr = ""
		i.statusOK = joinStatus(note, i.chatSearch.hint)
		i.chatSearch.hint = i.statusOK
	} else if i.statusErr == "" {
		i.statusOK = note
	}
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
