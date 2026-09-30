package modes

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/mattn/go-runewidth"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// The session timeline is a read-only inspector for the model context: the
// system prompt, the visible transcript, and every tool call with its result.
// It renders inside the shared floating pane, so the live transcript keeps
// updating behind it.
//
// Privacy: entries are built only from the transcript snapshot. Encrypted
// reasoning, provider thought signatures and image bytes are never rendered
// or exported, and nothing here logs.

const timelineTabCount = 5

var timelineTabNames = [timelineTabCount]string{"summary", "payload", "result", "schema", "timing"}

type timelineEntry struct {
	Kind     string    `json:"kind"`
	Turn     int       `json:"turn,omitempty"`
	Label    string    `json:"label"`
	Summary  string    `json:"summary"`
	Time     time.Time `json:"time,omitempty"`
	Duration string    `json:"duration,omitempty"`
	Payload  string    `json:"payload,omitempty"`
	Result   string    `json:"result,omitempty"`
	Schema   string    `json:"schema,omitempty"`
	IsError  bool      `json:"is_error,omitempty"`
	ToolCall string    `json:"tool_call_id,omitempty"`
}

// timelineCacheKey identifies an unchanged snapshot: the agent, its transcript
// revision, and a content hash of the system prompt and tool specs (which
// change without bumping the transcript revision). The zero value disables
// caching (used by tests and callers without a live agent).
type timelineCacheKey struct {
	agent  *core.Agent
	rev    uint64
	prompt uint64
}

// timelineEstimates are the token estimates shown in the context header. They
// depend only on the snapshot, so they are computed once per snapshot.
type timelineEstimates struct {
	system, tools, messages int
	ok                      bool
}

type timelineData struct {
	System      string
	Tools       []provider.Tool
	Messages    []provider.Message
	ContextUsed int
	ContextMax  int
	Key         timelineCacheKey
	Est         timelineEstimates
}

type timelineView struct {
	active    bool
	cursor    int
	tab       int
	detailTop int
	searching bool
	filter    string
	notice    string

	// snap is the last complete snapshot; it is reused while snapKey matches
	// so an idle redraw copies neither the transcript nor the tool specs.
	snapValid bool
	snapKey   timelineCacheKey
	snap      timelineData

	cacheValid  bool
	cacheKey    timelineCacheKey
	cacheAll    []timelineEntry
	cacheFilter string
	cacheList   []timelineEntry
	cacheListOK bool
}

type timelineAction struct {
	Export bool
	Close  bool
}

func newTimelineView() *timelineView { return &timelineView{} }

func (v *timelineView) Active() bool { return v != nil && v.active }

// entries returns the (optionally filtered) timeline rows for data, reusing
// the previous build while the transcript revision is unchanged.
func (v *timelineView) entries(data timelineData) []timelineEntry {
	if data.Key == (timelineCacheKey{}) || !v.cacheValid || v.cacheKey != data.Key {
		v.cacheAll = buildTimelineEntries(data)
		v.cacheKey = data.Key
		v.cacheValid = data.Key != (timelineCacheKey{})
		v.cacheListOK = false
	}
	if !v.cacheListOK || v.cacheFilter != v.filter {
		v.cacheList = filterTimelineEntries(v.cacheAll, v.filter)
		v.cacheFilter = v.filter
		v.cacheListOK = true
	}
	return v.cacheList
}

func (v *timelineView) Open(data timelineData) {
	v.active = true
	v.searching = false
	v.filter = ""
	v.notice = ""
	v.tab = 0
	v.detailTop = 0
	v.cacheValid = false
	v.cursor = len(v.entries(data)) - 1
	if v.cursor < 0 {
		v.cursor = 0
	}
}

func (v *timelineView) Close() {
	v.active = false
	v.searching = false
	v.filter = ""
	v.notice = ""
	v.detailTop = 0
	v.cacheValid = false
	v.cacheAll, v.cacheList = nil, nil
	v.cacheListOK = false
	v.snapValid = false
	v.snap = timelineData{}
}

func (v *timelineView) SetNotice(s string) { v.notice = s }

func (v *timelineView) HandleKey(k tui.Key, data timelineData) timelineAction {
	entries := v.entries(data)
	clampTimelineCursor(v, len(entries))

	previousCursor, previousTab := v.cursor, v.tab
	switch k.Kind {
	case tui.KeyLeft:
		if v.detailTop > 0 {
			v.detailTop--
		}
	case tui.KeyRight:
		v.detailTop++
	case tui.KeyHome:
		v.detailTop = 0
	case tui.KeyEnd:
		v.detailTop = int(^uint(0) >> 1)
	case tui.KeyUp:
		if v.cursor > 0 {
			v.cursor--
		}
	case tui.KeyDown:
		if v.cursor < len(entries)-1 {
			v.cursor++
		}
	case tui.KeyPageUp:
		v.cursor -= 8
		if v.cursor < 0 {
			v.cursor = 0
		}
	case tui.KeyPageDown:
		v.cursor += 8
		clampTimelineCursor(v, len(entries))
	case tui.KeyTab:
		v.tab = (v.tab + 1) % timelineTabCount
	case tui.KeyShiftTab:
		v.tab = (v.tab + timelineTabCount - 1) % timelineTabCount
	case tui.KeyCtrlE:
		return timelineAction{Export: true}
	case tui.KeyBackspace:
		if v.searching && v.filter != "" {
			r := []rune(v.filter)
			v.filter = string(r[:len(r)-1])
			v.cursor = 0
			v.detailTop = 0
		}
	case tui.KeyPaste:
		if v.searching {
			v.filter += sanitizeTimelineLine(singleLinePaste(k.Paste))
			v.cursor = 0
			v.detailTop = 0
		}
	case tui.KeyRune:
		if !v.searching && k.Rune == '/' {
			v.searching = true
			v.notice = ""
		} else if v.searching && !k.Ctrl && !k.Alt && !k.Super && !unicode.IsControl(k.Rune) {
			v.filter += string(k.Rune)
			v.cursor = 0
			v.detailTop = 0
		}
	case tui.KeyCtrlC:
		// Ctrl+C always leaves the view, like every other floating dialog.
		v.Close()
		return timelineAction{Close: true}
	case tui.KeyEsc:
		if v.searching {
			if v.filter != "" {
				v.filter = ""
				v.cursor = 0
				v.detailTop = 0
			} else {
				v.searching = false
			}
			break
		}
		v.Close()
		return timelineAction{Close: true}
	}
	if v.cursor != previousCursor || v.tab != previousTab {
		v.detailTop = 0
	}
	return timelineAction{}
}

func clampTimelineCursor(v *timelineView, n int) {
	if n == 0 {
		v.cursor = 0
		return
	}
	if v.cursor >= n {
		v.cursor = n - 1
	}
	if v.cursor < 0 {
		v.cursor = 0
	}
}

// Render draws the inspector for a pane content area of width x height. The
// first line is a frame header and the last a frame rule so the floating pane
// can lift the header into its border like other dialogs. After padDialogFrame
// and floatingDialogBody, the body never exceeds height rows: optional rows
// (hint, context, tabs, notice) are dropped before the list or detail shrink,
// down to a single list row.
func (v *timelineView) Render(th tui.Theme, width, height int, data timelineData) []string {
	if !v.Active() {
		return nil
	}
	if width < 1 {
		width = 1
	}
	entries := v.entries(data)
	clampTimelineCursor(v, len(entries))

	// padDialogFrame may add one blank row after the header and one before
	// the rule; both stay in the pane body.
	avail := max(0, height-2)
	noticeRows := 0
	if v.notice != "" && avail >= 4 {
		noticeRows = 1
	}
	avail -= noticeRows
	tabRows, hintRows, contextRows := 0, 0, 0
	if avail >= 3 {
		tabRows = 1
		extra := avail - 3
		hintRows = min(1, extra)
		extra -= hintRows
		contextRows = min(2, extra)
	}
	rest := avail - tabRows - hintRows - contextRows
	listRows := min(12, max(min(rest, 1), rest/3))
	detailRows := max(0, rest-listRows)

	lines := []string{frameHeader(th, "timeline", width)}
	if contextRows > 0 {
		lines = append(lines, renderTimelineContext(th, width, data)[:contextRows]...)
	}
	if hintRows > 0 {
		hint := "↑/↓ select  tab details  ←/→ scroll detail  home/end  / search  ctrl+e export  esc close"
		if v.searching {
			hint = "search: " + sanitizeTimelineLine(v.filter) + "_  (esc clears)"
		}
		lines = append(lines, th.FGColor(th.Muted, fitTimelineLine(hint, width)))
	}
	if listRows > 0 {
		if len(entries) == 0 {
			lines = append(lines, th.FGColor(th.Muted, fitTimelineLine("  no matching events", width)))
		} else {
			start := v.cursor - listRows/2
			if start < 0 {
				start = 0
			}
			end := start + listRows
			if end > len(entries) {
				end = len(entries)
				start = max(0, end-listRows)
			}
			for idx := start; idx < end; idx++ {
				line := formatTimelineRow(entries[idx], width)
				if idx == v.cursor {
					lines = append(lines, th.PadHighlight(line, width))
				} else {
					lines = append(lines, timelineColor(th, entries[idx], line))
				}
			}
		}
	}
	if tabRows > 0 {
		lines = append(lines, renderTimelineTabs(th, width, v.tab))
	}
	if detailRows > 0 && len(entries) > 0 {
		detail := timelineDetailRows(th, width, entries[v.cursor], v.tab)
		maxTop := max(0, len(detail)-detailRows)
		if v.detailTop > maxTop {
			v.detailTop = maxTop
		}
		lines = append(lines, detail[v.detailTop:min(v.detailTop+detailRows, len(detail))]...)
	}
	if noticeRows > 0 {
		lines = append(lines, th.FGColor(th.Tool, fitTimelineLine(sanitizeTimelineLine(v.notice), width)))
	}
	lines = append(lines, frameRule(th, width))
	return lines
}

// sanitizeTimelineLine makes untrusted transcript text one safe terminal row:
// escape sequences (CSI, OSC), control characters, tabs and CR/LF are removed
// or replaced before the text is measured, truncated or wrapped.
func sanitizeTimelineLine(s string) string {
	return sanitizeSessionTreeText(s)
}

// sanitizeTimelineBlock sanitizes multi-line text row by row, keeping line
// breaks (LF, CRLF and lone CR) and each row's leading indentation.
func sanitizeTimelineBlock(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	rows := strings.Split(s, "\n")
	for n, row := range rows {
		indent := 0
	indentation:
		for _, r := range row {
			switch r {
			case ' ':
				indent++
			case '\t':
				indent += 4
			default:
				break indentation
			}
		}
		rows[n] = strings.Repeat(" ", indent) + sanitizeSessionTreeText(row)
	}
	return strings.Join(rows, "\n")
}

// buildTimelineEntries turns a transcript snapshot into ordered rows. Each
// tool call is paired with the next unclaimed result carrying its ID, so an ID
// a provider reuses across turns keeps every call attached to its own result.
func buildTimelineEntries(data timelineData) []timelineEntry {
	toolSchemas := make(map[string]string, len(data.Tools))
	for _, tool := range data.Tools {
		toolSchemas[tool.Name] = prettyTimelineJSON(tool.Schema)
	}

	entries := make([]timelineEntry, 0, len(data.Messages)+1)
	if strings.TrimSpace(data.System) != "" {
		entries = append(entries, timelineEntry{
			Kind: "system", Label: "SYSTEM", Summary: firstTimelineLine(data.System), Payload: data.System,
		})
	}
	// pending maps a call ID to the FIFO of entry indexes still awaiting a result.
	pending := map[string][]int{}
	callTimes := map[int]time.Time{}
	turn := 0
	var previousTime time.Time
	for _, message := range data.Messages {
		if message.Role == provider.RoleUser {
			turn++
		}
		for _, content := range message.Content {
			if block, ok := content.(provider.ReasoningBlock); ok && strings.TrimSpace(block.Summary) != "" {
				entries = append(entries, timelineEntry{
					Kind: "reasoning", Turn: turn, Label: "REASONING", Summary: firstTimelineLine(block.Summary),
					Time: message.Time, Payload: block.Summary,
				})
			}
		}
		textParts := timelineMessageText(message)
		if len(textParts) > 0 {
			entry := timelineEntry{
				Kind: string(message.Role), Turn: turn, Label: strings.ToUpper(string(message.Role)),
				Summary: firstTimelineLine(strings.Join(textParts, "\n")), Time: message.Time,
				Payload: prettyTimelineValue(timelineMessagePayload(message)),
			}
			if !previousTime.IsZero() && !message.Time.IsZero() && message.Time.After(previousTime) {
				entry.Duration = message.Time.Sub(previousTime).Round(time.Millisecond).String()
			}
			entries = append(entries, entry)
			if !message.Time.IsZero() {
				previousTime = message.Time
			}
		}
		for _, content := range message.Content {
			switch block := content.(type) {
			case provider.ToolCallBlock:
				entries = append(entries, timelineEntry{
					Kind: "tool", Turn: turn, Label: "TOOL " + block.Name, Summary: summarizeToolArguments(block.Arguments),
					Time: message.Time, Payload: prettyTimelineJSON(block.Arguments), Schema: toolSchemas[block.Name], ToolCall: block.ID,
				})
				idx := len(entries) - 1
				pending[block.ID] = append(pending[block.ID], idx)
				callTimes[idx] = message.Time
			case provider.ToolResultBlock:
				queue := pending[block.CallID]
				result := prettyTimelineValue(timelineResultPayload(block))
				if len(queue) == 0 {
					// A result with no open call (for example after a
					// compaction boundary) stays visible, marked unpaired.
					entries = append(entries, timelineEntry{
						Kind: "tool", Turn: turn, Label: "TOOL RESULT", Summary: "(unpaired result)",
						Time: message.Time, Result: result, IsError: block.IsError, ToolCall: block.CallID,
					})
					continue
				}
				idx := queue[0]
				pending[block.CallID] = queue[1:]
				entries[idx].Result = result
				entries[idx].IsError = block.IsError
				if started := callTimes[idx]; !started.IsZero() && message.Time.After(started) {
					entries[idx].Duration = message.Time.Sub(started).Round(time.Millisecond).String()
				}
			}
		}
	}
	return entries
}

func timelineMessageText(message provider.Message) []string {
	var out []string
	for _, content := range message.Content {
		switch block := content.(type) {
		case provider.TextBlock:
			if strings.TrimSpace(block.Text) != "" {
				out = append(out, block.Text)
			}
		case provider.ImageBlock:
			out = append(out, fmt.Sprintf("[image %s, %d bytes]", block.MimeType, len(block.Data)))
		}
	}
	return out
}

// timelineMessagePayload describes a message without provider-private data:
// reasoning shows only its readable summary (never the encrypted content), text
// omits thought signatures, and images show only type and size.
func timelineMessagePayload(message provider.Message) any {
	blocks := make([]any, 0, len(message.Content))
	for _, content := range message.Content {
		switch block := content.(type) {
		case provider.TextBlock:
			blocks = append(blocks, map[string]any{"type": "text", "text": block.Text})
		case provider.ReasoningBlock:
			blocks = append(blocks, map[string]any{"type": "reasoning", "summary": block.Summary})
		case provider.ImageBlock:
			blocks = append(blocks, map[string]any{"type": "image", "mime_type": block.MimeType, "bytes": len(block.Data)})
		}
	}
	return map[string]any{"role": message.Role, "content": blocks, "meta": message.Meta}
}

func timelineResultPayload(result provider.ToolResultBlock) any {
	parts := make([]any, 0, len(result.Content))
	for _, content := range result.Content {
		switch block := content.(type) {
		case provider.TextBlock:
			parts = append(parts, map[string]any{"type": "text", "text": block.Text})
		case provider.ImageBlock:
			parts = append(parts, map[string]any{"type": "image", "mime_type": block.MimeType, "bytes": len(block.Data)})
		default:
			parts = append(parts, fmt.Sprintf("%T", content))
		}
	}
	return map[string]any{"content": parts, "is_error": result.IsError}
}

func filterTimelineEntries(entries []timelineEntry, filter string) []timelineEntry {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return entries
	}
	out := make([]timelineEntry, 0, len(entries))
	for _, entry := range entries {
		haystack := strings.ToLower(strings.Join([]string{entry.Label, entry.Summary, entry.Payload, entry.Result}, "\n"))
		if strings.Contains(haystack, filter) {
			out = append(out, entry)
		}
	}
	return out
}

func renderTimelineContext(th tui.Theme, width int, data timelineData) []string {
	est := data.Est
	if !est.ok {
		est = computeTimelineEstimates(data.System, data.Tools, data.Messages)
	}
	system, tools, messages := est.system, est.tools, est.messages

	usage := fmt.Sprintf("context %s", compactTimelineNumber(data.ContextUsed))
	if data.ContextMax > 0 {
		pct := float64(data.ContextUsed) * 100 / float64(data.ContextMax)
		usage = fmt.Sprintf("context %s / %s (%.1f%%)", compactTimelineNumber(data.ContextUsed), compactTimelineNumber(data.ContextMax), pct)
	}
	composition := fmt.Sprintf("estimated composition: system %s  tools %s  messages %s", compactTimelineNumber(system), compactTimelineNumber(tools), compactTimelineNumber(messages))
	return []string{tui.Bold(fitTimelineLine(usage, width)), th.FGColor(th.Muted, fitTimelineLine(composition, width))}
}

func computeTimelineEstimates(system string, tools []provider.Tool, messages []provider.Message) timelineEstimates {
	toolBytes, _ := json.Marshal(tools)
	return timelineEstimates{
		system:   estimateTimelineTokens(system),
		tools:    estimateTimelineTokens(string(toolBytes)),
		messages: estimateTimelineMessageTokens(messages),
		ok:       true,
	}
}

func formatTimelineRow(entry timelineEntry, width int) string {
	stamp := "        "
	if !entry.Time.IsZero() {
		stamp = entry.Time.Format("15:04:05")
	}
	turn := "   "
	if entry.Turn > 0 {
		turn = fmt.Sprintf("%3d", entry.Turn)
	}
	line := fmt.Sprintf("%s  %s  %-16s  %s", stamp, turn, sanitizeTimelineLine(entry.Label), sanitizeTimelineLine(entry.Summary))
	return fitTimelineLine(line, width)
}

func timelineColor(th tui.Theme, entry timelineEntry, line string) string {
	color := th.Muted
	switch entry.Kind {
	case "user":
		color = th.User
	case "assistant", "reasoning":
		color = th.Assistant
	case "tool":
		color = th.Tool
	case "system":
		color = th.Warning
	}
	if entry.IsError {
		color = th.Error
	}
	return th.FGColor(color, line)
}

func renderTimelineTabs(th tui.Theme, width, selected int) string {
	if selected < 0 || selected >= len(timelineTabNames) || width < 1 {
		return ""
	}
	start, end := selected, selected+1
	for start > 0 || end < len(timelineTabNames) {
		grew := false
		if start > 0 && timelineTabsWidth(start-1, end) <= width {
			start--
			grew = true
		}
		if end < len(timelineTabNames) && timelineTabsWidth(start, end+1) <= width {
			end++
			grew = true
		}
		if !grew {
			break
		}
	}

	tabs := make([]string, 0, end-start)
	for idx := start; idx < end; idx++ {
		name := timelineTabNames[idx]
		if idx == selected {
			tabs = append(tabs, tui.Bold(th.FGColor(th.Accent, "["+name+"]")))
		} else {
			tabs = append(tabs, th.FGColor(th.Muted, " "+name+" "))
		}
	}
	return strings.Join(tabs, "  ")
}

func timelineTabsWidth(start, end int) int {
	width := 2 * (end - start - 1)
	for idx := start; idx < end; idx++ {
		width += runewidth.StringWidth(timelineTabNames[idx]) + 2
	}
	return width
}

func timelineDetailRows(th tui.Theme, width int, entry timelineEntry, tab int) []string {
	var detail string
	switch tab {
	case 0:
		detail = fmt.Sprintf("type: %s\nturn: %d\nstatus: %s\nsummary: %s", entry.Kind, entry.Turn, timelineStatus(entry), entry.Summary)
	case 1:
		detail = entry.Payload
	case 2:
		detail = entry.Result
	case 3:
		detail = entry.Schema
	case 4:
		started := "not recorded"
		if !entry.Time.IsZero() {
			started = entry.Time.Format(time.RFC3339Nano)
		}
		duration := entry.Duration
		if duration == "" {
			duration = "not recorded"
		}
		detail = "started: " + started + "\nduration: " + duration + "\nsource: transcript timestamps"
	}
	detail = sanitizeTimelineBlock(detail)
	if strings.TrimSpace(detail) == "" {
		detail = "not available for this event"
	}
	if entry.Kind == "reasoning" && (tab == 0 || tab == 1) {
		return renderDialogMarkdownRows(sanitizeTimelineBlock(entry.Payload), th, width)
	}
	var rows []string
	for _, line := range strings.Split(detail, "\n") {
		for _, wrapped := range tui.WrapANSILine(line, max(1, width-2)) {
			rows = append(rows, "  "+th.FGColor(th.ToolOut, wrapped))
		}
	}
	return rows
}

func timelineStatus(entry timelineEntry) string {
	if entry.IsError {
		return "error"
	}
	if entry.Kind == "tool" && entry.Result == "" {
		return "result unavailable"
	}
	return "completed"
}

// Export writes the timeline as JSON into dir (the default export directory
// when empty). The file is created exclusively with owner-only permissions so
// an existing file or symlink is never overwritten or widened.
func (v *timelineView) Export(dir string, data timelineData) (string, error) {
	if dir == "" {
		dir = defaultExportDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create export directory: %w", err)
	}
	payload := struct {
		ExportedAt  time.Time       `json:"exported_at"`
		ContextUsed int             `json:"context_used"`
		ContextMax  int             `json:"context_max"`
		Events      []timelineEntry `json:"events"`
	}{time.Now(), data.ContextUsed, data.ContextMax, buildTimelineEntries(data)}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode timeline: %w", err)
	}
	name := "zut-timeline-" + time.Now().Format("20060102-150405.000000000") + ".json"
	path := filepath.Join(dir, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write timeline: %w", err)
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write timeline: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("write timeline: %w", err)
	}
	return path, nil
}

func firstTimelineLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return fitTimelineLine(line, 120)
		}
	}
	return "(empty)"
}

func summarizeToolArguments(raw json.RawMessage) string {
	var object map[string]any
	if json.Unmarshal(raw, &object) == nil {
		for _, key := range []string{"command", "path", "query", "pattern"} {
			if value, ok := object[key]; ok {
				return firstTimelineLine(fmt.Sprint(value))
			}
		}
	}
	return firstTimelineLine(string(raw))
}

func prettyTimelineJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return string(raw)
	}
	return prettyTimelineValue(value)
}

func prettyTimelineValue(value any) string {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func estimateTimelineTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len([]byte(s)) + 3) / 4
}

func estimateTimelineMessageTokens(messages []provider.Message) int {
	bytes := 0
	for _, message := range messages {
		bytes += len(message.Role) + 12
		for _, content := range message.Content {
			switch block := content.(type) {
			case provider.TextBlock:
				bytes += len(block.Text) + len(block.ThoughtSignature)
			case provider.ReasoningBlock:
				// Sizes only: the encrypted payload is counted toward the
				// estimate but never displayed.
				bytes += len(block.Summary) + len(block.Encrypted)
			case provider.ImageBlock:
				// JSON transports image bytes as base64, which expands by roughly 4/3.
				bytes += (len(block.Data)*4 + 2) / 3
			case provider.ToolCallBlock:
				bytes += len(block.ID) + len(block.Name) + len(block.Arguments)
			case provider.ToolResultBlock:
				bytes += len(block.CallID) + estimateTimelineResultBytes(block.Content)
			}
		}
	}
	return (bytes + 3) / 4
}

func estimateTimelineResultBytes(contents []provider.Content) int {
	bytes := 0
	for _, content := range contents {
		switch block := content.(type) {
		case provider.TextBlock:
			bytes += len(block.Text)
		case provider.ImageBlock:
			bytes += (len(block.Data)*4 + 2) / 3
		}
	}
	return bytes
}

func compactTimelineNumber(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fm", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

func fitTimelineLine(s string, width int) string {
	if width < 1 {
		return ""
	}
	if runewidth.StringWidth(s) <= width {
		return s
	}
	return runewidth.Truncate(s, width, "...")
}

// timelineDataLocked returns the timeline snapshot for the live agent using
// its locked accessors. Host-internal continuation prompts are dropped exactly
// as the chat transcript drops them. The transcript and tool specs are only
// copied when the transcript revision or the system/tool content hash changed;
// otherwise the cached snapshot (and its token estimates) is reused. The
// caller must hold i.mu.
func (i *Interactive) timelineDataLocked() timelineData {
	if i.timeline == nil {
		i.timeline = newTimelineView()
	}
	v := i.timeline
	var data timelineData
	if i.agent != nil {
		system, registry := i.agent.PromptConfig()
		specs := registry.Specs()
		// Read the revision before the messages: a concurrent append can only
		// make the cached snapshot look stale, never newer than it is.
		key := timelineCacheKey{agent: i.agent, rev: i.agent.Revision(), prompt: timelinePromptHash(system, specs)}
		if v.snapValid && v.snapKey == key {
			data = v.snap
		} else {
			data = timelineData{System: system, Tools: specs, Key: key}
			data.Messages = filterHiddenTranscriptMessages(i.agent.Messages())
			data.Est = computeTimelineEstimates(system, specs, data.Messages)
			v.snapKey, v.snap, v.snapValid = key, data, true
		}
	}
	data.ContextUsed = i.lastCtxInput
	if model, err := provider.FindModel(i.cfg.Provider, i.cfg.Model); err == nil {
		data.ContextMax = model.ContextWindow
	}
	return data
}

// timelinePromptHash fingerprints the system prompt and tool specs by content,
// so a same-length edit to either still invalidates the cached snapshot.
func timelinePromptHash(system string, tools []provider.Tool) uint64 {
	h := fnv.New64a()
	write := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	write(system)
	for _, tool := range tools {
		write(tool.Name)
		write(tool.Description)
		_, _ = h.Write(tool.Schema)
		_, _ = h.Write([]byte{0})
		if tool.Deferred {
			write("deferred")
		}
	}
	return h.Sum64()
}

// openTimeline shows the inspector in the floating pane.
func (i *Interactive) openTimeline() {
	i.mu.Lock()
	if i.timeline == nil {
		i.timeline = newTimelineView()
	}
	i.timeline.Open(i.timelineDataLocked())
	i.statusErr = ""
	i.statusOK = ""
	i.mu.Unlock()
	i.invalidate()
}

// handleTimelineKey routes one key to the open timeline. View state is shared
// with the renderer, so it is only touched under i.mu; the export's file I/O
// runs outside the lock on the snapshot taken with the key.
func (i *Interactive) handleTimelineKey(k tui.Key) {
	i.mu.Lock()
	data := i.timelineDataLocked()
	act := i.timeline.HandleKey(k, data)
	i.mu.Unlock()
	if act.Export {
		path, err := i.timeline.Export("", data)
		i.mu.Lock()
		if err != nil {
			i.timeline.SetNotice("export failed: " + err.Error())
		} else {
			i.timeline.SetNotice("exported to " + friendlyPath(path))
		}
		i.mu.Unlock()
	}
	i.invalidate()
}
