package modes

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// residentChildSession owns a bounded child transcript assembled from the
// resident journal plus the child's immutable live projection. It deliberately
// has no UI lock or disk path: callers load pages asynchronously and discard
// them when their own dialog generation no longer matches.
type residentChildSession struct {
	mu                  sync.Mutex
	manager             *subagents.ResidentManager
	childID             string
	olderCursor         string
	view                tui.View
	composer            *tui.Editor
	loading             bool
	recentReloadPending bool
	err                 string
	// scrollOffset is measured from the rendered live tail. Keeping the
	// viewport bottom-relative preserves the same finalized content when an
	// older page is prepended or the active turn grows.
	scrollOffset          int
	unread                int
	lastRows              int
	recentRows            int
	liveRevision          uint64
	livePresent           bool
	cacheWidth            int
	cacheMessagesRevision uint64
	cacheMessageCount     int
	cacheLiveRevision     uint64
	cacheLivePresent      bool
	cacheRows             []string
	loadCtx               context.Context
	cancelLoad            context.CancelFunc
}

func newResidentChildSession(manager *subagents.ResidentManager, childID string, theme tui.Theme) *residentChildSession {
	ctx, cancel := context.WithCancel(context.Background())
	return &residentChildSession{manager: manager, childID: childID, view: tui.View{Theme: theme, ExpandAll: true}, composer: tui.NewEditor("follow-up > "), loadCtx: ctx, cancelLoad: cancel}
}

func (s *residentChildSession) Close() {
	if s != nil && s.cancelLoad != nil {
		s.cancelLoad()
	}
}

func (s *residentChildSession) setTheme(theme tui.Theme) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.view.Theme = theme
	s.view.InvalidateRenderCache()
}

func (s *residentChildSession) LoadRecent(limit int) error {
	if s == nil || s.manager == nil {
		return fmt.Errorf("resident child session: unavailable")
	}
	page, err := s.manager.HistoryPage(s.childID, "", limit)
	if err != nil {
		return err
	}
	messages, err := subagents.ResidentHistoryMessages(page.Items)
	if err != nil {
		return err
	}
	s.replaceRecent(messages, page.OlderCursor)
	return nil
}

// ReloadRecent replaces only the newest bounded history page. Any older
// pages already loaded by PgUp remain intact, so following a completed turn
// does not discard the user's scrollback.
func (s *residentChildSession) ReloadRecent(limit int) error {
	return s.LoadRecent(limit)
}

// LoadAll progressively installs the newest page then prepends every older
// page. The callback runs after each durable page so the floating pane grows
// naturally while disk history is being reconstructed.
func (s *residentChildSession) LoadAll(limit int, progressed func()) error {
	if s.loadCtx != nil {
		select {
		case <-s.loadCtx.Done():
			return s.loadCtx.Err()
		default:
		}
	}
	if err := s.LoadRecent(limit); err != nil {
		return err
	}
	if progressed != nil {
		progressed()
	}
	for {
		if s.loadCtx != nil {
			select {
			case <-s.loadCtx.Done():
				return s.loadCtx.Err()
			default:
			}
		}
		s.mu.Lock()
		more := s.olderCursor != ""
		s.mu.Unlock()
		if !more {
			return nil
		}
		if err := s.LoadOlder(limit); err != nil {
			return err
		}
		if progressed != nil {
			progressed()
		}
	}
}

// ReloadAll rebuilds from one journal generation instead of replacing a
// projected-message suffix whose page boundary may not match tool projection.
func (s *residentChildSession) ReloadAll(limit int, progressed func()) error {
	s.mu.Lock()
	s.view.Messages = nil
	s.view.MessagesRevision++
	s.olderCursor = ""
	s.recentRows = 0
	// Keep a reader's bottom-relative viewport across a durable reload.
	// Render clamps it against the rebuilt rows; a tail follower remains at 0.
	s.mu.Unlock()
	return s.LoadAll(limit, progressed)
}

func (s *residentChildSession) replaceRecent(messages []provider.Message, olderCursor string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	older := len(s.view.Messages) - s.recentRows
	if older < 0 {
		older = 0
	}
	prefix := append([]provider.Message(nil), s.view.Messages[:older]...)
	s.view.Messages = append(prefix, messages...)
	s.view.MessagesRevision++
	s.olderCursor = olderCursor
	s.recentRows = len(messages)
	s.err = ""
	s.refreshLiveLocked()
	s.mu.Unlock()
}

func (s *residentChildSession) LoadOlder(limit int) error {
	if s == nil || s.manager == nil {
		return fmt.Errorf("resident child session: unavailable")
	}
	s.mu.Lock()
	cursor := s.olderCursor
	s.mu.Unlock()
	if cursor == "" {
		return nil
	}
	page, err := s.manager.HistoryPage(s.childID, cursor, limit)
	if err != nil {
		return err
	}
	messages, err := subagents.ResidentHistoryMessages(page.Items)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.view.Messages = append(messages, s.view.Messages...)
	s.view.MessagesRevision++
	s.olderCursor = page.OlderCursor
	s.err = ""
	s.refreshLiveLocked()
	s.mu.Unlock()
	return nil
}

func (s *residentChildSession) refreshLiveLocked() {
	if s == nil || s.manager == nil {
		return
	}
	live, ok := s.manager.Live(s.childID)
	if !ok {
		s.view.Streaming = ""
		s.view.StreamingActive = false
		s.view.ToolCalls = nil
		s.liveRevision = 0
		s.livePresent = false
		return
	}
	s.liveRevision = live.Revision
	s.livePresent = true
	s.view.Streaming = live.AssistantText
	s.view.StreamingActive = live.State == subagents.ResidentRunning && live.AssistantText != ""
	s.view.ToolCalls = make([]tui.ToolCallView, len(live.Tools))
	for index, tool := range live.Tools {
		s.view.ToolCalls[index] = tui.ToolCallView{ID: tool.ID, Name: tool.Name, Args: tui.ShortArgs(tool.Name, tool.Args), RawJSONBuf: string(tool.Args), Streaming: tool.State == subagents.ResidentLiveToolComposing, Revision: live.Revision}
	}
}

func (s *residentChildSession) renderedHistory(width int) []string {
	s.mu.Lock()
	s.refreshLiveLocked()
	if s.cacheWidth == width && s.cacheMessagesRevision == s.view.MessagesRevision && s.cacheMessageCount == len(s.view.Messages) && s.cacheLiveRevision == s.liveRevision && s.cacheLivePresent == s.livePresent && s.cacheRows != nil {
		rows := s.cacheRows
		s.mu.Unlock()
		return rows
	}
	view := s.view.CloneForRender()
	messagesRevision, liveRevision := s.view.MessagesRevision, s.liveRevision
	s.mu.Unlock()
	rows := view.Build(width)
	s.mu.Lock()
	if s.view.MessagesRevision == messagesRevision && s.liveRevision == liveRevision {
		s.cacheWidth, s.cacheMessagesRevision, s.cacheMessageCount, s.cacheLiveRevision, s.cacheLivePresent, s.cacheRows = width, messagesRevision, len(s.view.Messages), liveRevision, s.livePresent, rows
		s.view.AdoptRenderCacheFrom(view)
	}
	s.mu.Unlock()
	return rows
}

func (s *residentChildSession) View() *tui.View {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.refreshLiveLocked()
	defer s.mu.Unlock()
	return s.view.CloneForRender()
}

func (s *residentChildSession) BeginLoad() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loading {
		return false
	}
	s.loading = true
	s.err = ""
	return true
}

// RequestRecentReload reserves a newest-page refresh. If another history
// read is already in progress, it coalesces one refresh behind that read.
func (s *residentChildSession) RequestRecentReload() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loading {
		s.recentReloadPending = true
		return false
	}
	s.loading = true
	s.err = ""
	return true
}

// FinishLoad clears the current history operation and reports whether a
// terminal child update arrived while it was loading and needs one coalesced
// newest-page refresh.
func (s *residentChildSession) FinishLoad(err error) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	s.loading = false
	if err != nil {
		s.err = err.Error()
	}
	pending := s.recentReloadPending
	s.recentReloadPending = false
	s.mu.Unlock()
	return pending
}

// HandleKey updates only the child-local composer. It returns a durable prompt
// candidate; the controller leaves it intact until Manager.Resume succeeds.
func (s *residentChildSession) HandleKey(k tui.Key) (string, bool) {
	if s == nil || s.composer == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.composer.HandleKey(k) {
		return "", false
	}
	prompt := strings.TrimSpace(s.composer.SubmitValue())
	return prompt, prompt != ""
}

func (s *residentChildSession) HandleNavigation(k tui.Key) bool {
	if s == nil || s.composer == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.composer.HandleKey(k)
}

func (s *residentChildSession) FinishSubmission(err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if err != nil {
		s.err = err.Error()
	} else if s.composer != nil {
		s.composer.Clear()
		s.err = ""
	}
	s.mu.Unlock()
}

func (s *residentChildSession) Scroll(delta int) {
	if s == nil || delta == 0 {
		return
	}
	s.mu.Lock()
	s.scrollOffset += delta
	if s.scrollOffset < 0 {
		s.scrollOffset = 0
	}
	if s.scrollOffset == 0 {
		s.unread = 0
	}
	s.mu.Unlock()
}

func (s *residentChildSession) FollowTail() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.scrollOffset, s.unread = 0, 0
	s.mu.Unlock()
}

// Render keeps all transcript rendering in tui.View. Disk-backed history is
// loaded by the controller before this is called; this method consults only
// the immutable live projection maintained by the resident child.
func (s *residentChildSession) Render(width, height int) []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	loading, loadErr := s.loading, s.err
	var editorLines []string
	if s.composer != nil {
		editorLines, _, _ = s.composer.Render(width)
	}
	s.mu.Unlock()
	header := fmt.Sprintf("  Resident subagent %s", s.childID)
	if s.manager != nil {
		if snapshot, ok := s.manager.SnapshotFor(s.childID); ok {
			header += fmt.Sprintf("  %s  %s/%s", snapshot.State, snapshot.Provider, snapshot.Model)
			if snapshot.Profile != "" {
				header += "  " + snapshot.Profile
			}
			if snapshot.WorkspaceMode != "" {
				header += "  " + string(snapshot.WorkspaceMode)
			}
		}
	}
	lines := []string{header}
	if loading {
		lines = append(lines, "  Loading history…")
	}
	if loadErr != "" {
		lines = append(lines, "  history: "+loadErr)
	}
	history := s.renderedHistory(width)
	s.mu.Lock()
	if s.lastRows != 0 && len(history) > s.lastRows && s.scrollOffset > 0 {
		s.scrollOffset += len(history) - s.lastRows
		s.unread++
	}
	s.lastRows = len(history)
	maxRows := height - len(lines) - len(editorLines) - 3
	if maxRows < 0 {
		maxRows = 0
	}
	if s.scrollOffset > len(history)-maxRows {
		s.scrollOffset = len(history) - maxRows
		if s.scrollOffset < 0 {
			s.scrollOffset = 0
		}
	}
	end := len(history) - s.scrollOffset
	start := end - maxRows
	if start < 0 {
		start = 0
	}
	unread := s.unread
	s.mu.Unlock()
	lines = append(lines, history[start:end]...)
	if unread > 0 {
		lines = append(lines, fmt.Sprintf("  %d new updates below; PgDn to follow", unread))
	}
	lines = append(lines, "")
	lines = append(lines, editorLines...)
	lines = append(lines, "  Enter: send follow-up/resume   Esc: close   PgUp/PgDn: scroll history")
	if height <= 0 {
		return nil
	}
	if len(lines) > height {
		return lines[:height]
	}
	return lines
}
