package modes

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// residentChildSession owns a bounded child transcript read through the
// session port. It deliberately has no UI lock or disk path: callers load
// pages asynchronously and refresh them when the session publishes a changed
// resident summary. Only finalized history is shown; the daemon does not
// stream a child's in-flight text.
type residentChildSession struct {
	mu                  sync.Mutex
	port                session.Port
	childID             string
	summary             session.ResidentSummary
	hasSummary          bool
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
	cacheWidth            int
	cacheMessagesRevision uint64
	cacheMessageCount     int
	cacheRows             []string
	loadCtx               context.Context
	cancelLoad            context.CancelFunc
}

// residentHistoryTimeout bounds one daemon page read.
const residentHistoryTimeout = 10 * time.Second

func newResidentChildSession(port session.Port, childID string, theme tui.Theme) *residentChildSession {
	ctx, cancel := context.WithCancel(context.Background())
	return &residentChildSession{port: port, childID: childID, view: tui.View{Theme: theme, ExpandAll: true}, composer: tui.NewEditor("follow-up > "), loadCtx: ctx, cancelLoad: cancel}
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

// SetSummary records the session-published summary shown in the header and
// reports whether it changed since the last call.
func (s *residentChildSession) SetSummary(summary session.ResidentSummary, ok bool) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := ok != s.hasSummary || summary != s.summary
	s.summary, s.hasSummary = summary, ok
	return changed
}

func (s *residentChildSession) historyPage(cursor string, limit int) (subagents.ResidentHistoryPage, error) {
	if s == nil || s.port == nil {
		return subagents.ResidentHistoryPage{}, fmt.Errorf("resident child session: unavailable")
	}
	parent := s.loadCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, residentHistoryTimeout)
	defer cancel()
	return s.port.ResidentHistory(ctx, s.childID, cursor, limit)
}

func (s *residentChildSession) LoadRecent(limit int) error {
	page, err := s.historyPage("", limit)
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
	s.mu.Unlock()
}

func (s *residentChildSession) LoadOlder(limit int) error {
	if s == nil || s.port == nil {
		return fmt.Errorf("resident child session: unavailable")
	}
	s.mu.Lock()
	cursor := s.olderCursor
	s.mu.Unlock()
	if cursor == "" {
		return nil
	}
	page, err := s.historyPage(cursor, limit)
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
	s.mu.Unlock()
	return nil
}

func (s *residentChildSession) renderedHistory(width int) []string {
	s.mu.Lock()
	if s.cacheWidth == width && s.cacheMessagesRevision == s.view.MessagesRevision && s.cacheMessageCount == len(s.view.Messages) && s.cacheRows != nil {
		rows := s.cacheRows
		s.mu.Unlock()
		return rows
	}
	view := s.view.CloneForRender()
	messagesRevision := s.view.MessagesRevision
	s.mu.Unlock()
	rows := view.Build(width)
	s.mu.Lock()
	if s.view.MessagesRevision == messagesRevision {
		s.cacheWidth, s.cacheMessagesRevision, s.cacheMessageCount, s.cacheRows = width, messagesRevision, len(s.view.Messages), rows
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
// candidate; the controller leaves it intact until the session accepts it.
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

// Render keeps all transcript rendering in tui.View. History is loaded by the
// controller before this is called; Render never calls the session port.
func (s *residentChildSession) Render(width, height int) []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	loading, loadErr := s.loading, s.err
	summary, hasSummary := s.summary, s.hasSummary
	var editorLines []string
	if s.composer != nil {
		editorLines, _, _ = s.composer.Render(width)
	}
	s.mu.Unlock()
	header := fmt.Sprintf("  Resident subagent %s", s.childID)
	if hasSummary {
		header += "  " + residentDisplayState(subagents.ResidentState(summary.State))
		if profile := sanitizeSessionTreeText(summary.Profile); profile != "" {
			header += "  " + profile
		}
		if workspace := sanitizeSessionTreeText(summary.Workspace); workspace != "" {
			header += "  " + workspace
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
