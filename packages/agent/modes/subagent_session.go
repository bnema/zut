package modes

import (
	"fmt"
	"strings"
	"sync"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/tui"
)

// residentChildSession owns a bounded child transcript assembled from the
// resident journal plus the child's immutable live projection. It deliberately
// has no UI lock or disk path: callers load pages asynchronously and discard
// them when their own dialog generation no longer matches.
type residentChildSession struct {
	mu          sync.Mutex
	manager     *subagents.ResidentManager
	childID     string
	olderCursor string
	view        tui.View
	composer    *tui.Editor
	loading     bool
	err         string
	// scrollOffset is measured from the rendered live tail. Keeping the
	// viewport bottom-relative preserves the same finalized content when an
	// older page is prepended or the active turn grows.
	scrollOffset int
	unread       int
	lastRows     int
}

func newResidentChildSession(manager *subagents.ResidentManager, childID string, theme tui.Theme) *residentChildSession {
	return &residentChildSession{manager: manager, childID: childID, view: tui.View{Theme: theme, ExpandAll: true}, composer: tui.NewEditor("follow-up > ")}
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
	s.mu.Lock()
	s.view.Messages = messages
	s.view.MessagesRevision++
	s.olderCursor = page.OlderCursor
	s.loading = false
	s.err = ""
	s.refreshLiveLocked()
	s.mu.Unlock()
	return nil
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
	s.loading = false
	s.err = ""
	s.refreshLiveLocked()
	s.mu.Unlock()
	return nil
}

func (s *residentChildSession) refreshLive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLiveLocked()
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
		return
	}
	s.view.Streaming = live.AssistantText
	s.view.StreamingActive = live.State == subagents.ResidentRunning && live.AssistantText != ""
	s.view.ToolCalls = make([]tui.ToolCallView, len(live.Tools))
	for index, tool := range live.Tools {
		s.view.ToolCalls[index] = tui.ToolCallView{ID: tool.ID, Name: tool.Name, Args: tui.ShortArgs(tool.Name, tool.Args), RawJSONBuf: string(tool.Args), Streaming: tool.State == subagents.ResidentLiveToolComposing, Revision: live.Revision}
	}
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

func (s *residentChildSession) FinishLoad(err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.loading = false
	if err != nil {
		s.err = err.Error()
	}
	s.mu.Unlock()
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
	view := s.View()
	if view == nil {
		return []string{"  resident subagent unavailable"}
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
	history := view.Build(width)
	s.mu.Lock()
	if s.lastRows != 0 && len(history) > s.lastRows && s.scrollOffset > 0 {
		s.scrollOffset += len(history) - s.lastRows
		s.unread++
	}
	s.lastRows = len(history)
	maxRows := height - len(lines) - len(editorLines) - 3
	if maxRows < 3 {
		maxRows = 3
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
	lines = append(lines, "  Enter: send follow-up/resume   Esc: close   PgUp: load older history")
	return lines
}
