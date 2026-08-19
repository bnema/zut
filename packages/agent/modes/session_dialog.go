package modes

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/tui"
	"github.com/sahilm/fuzzy"
)

type sessionLoadEventKind uint8

const (
	sessionLoadStarted sessionLoadEventKind = iota
	sessionLoadEntry
	sessionLoadFinished
)

type sessionSearchEventKind uint8

const (
	sessionSearchCorpusReady sessionSearchEventKind = iota
	sessionSearchMatchesReady
)

type sessionSearchEvent struct {
	kind       sessionSearchEventKind
	generation uint64
	query      string
	segments   []core.SessionSearchSegment
	matches    map[string]sessionSearchMatch
	skipped    int
}

type sessionSearchMatch struct {
	count   int
	excerpt string
	indexes []int
	score   int
}

type sessionLoadEvent struct {
	kind       sessionLoadEventKind
	generation uint64
	total      int
	paths      []string
	index      int
	summary    core.SessionSummary
}

type sessionLoadSlot struct {
	loaded  bool
	summary core.SessionSummary
}

type sessionLoadJob struct {
	index int
	path  string
}

const maxSessionLoadWorkers = 8

// sessionDialog is the inline picker shown when the user runs /sessions.
type sessionDialog struct {
	active       bool
	sessions     []core.SessionSummary
	baseSessions []core.SessionSummary
	cursor       int
	renaming     bool
	rename       string

	allScope         bool
	root             string
	cwd              string
	paths            []string
	query            string
	searching        bool
	searchPending    bool
	searchReady      bool
	searchGeneration uint64
	searchContext    context.Context
	searchCancel     context.CancelFunc
	matchCancel      context.CancelFunc
	searchEvents     chan sessionSearchEvent
	searchSegments   []core.SessionSearchSegment
	searchMatches    map[string]sessionSearchMatch
	searchErr        string

	loading          bool
	loadingDone      int
	loadingTotal     int
	loadingStartedAt time.Time
	loadGeneration   uint64
	loadCancel       context.CancelFunc
	loadDone         chan struct{}
	// loadReadyThrough is the contiguous path prefix whose summaries have
	// arrived. It prevents an older worker result from appearing before a
	// newer session that is still being read.
	loadSlots        []sessionLoadSlot
	loadReadyThrough int

	// describeSession is replaceable in tests so cancellation can be
	// exercised without relying on file-size-dependent timing.
	describeSession func(context.Context, string) core.SessionSummary

	// MaxRows is the maximum number of session rows the dialog
	// will render in a single frame. Set by the host right before
	// Render based on the available chat space; if 0, the dialog
	// falls back to rendering every row (original behaviour).
	// When the list is longer than MaxRows the dialog scrolls so
	// the cursor stays visible and tags the first/last visible
	// entry with a muted "↑ N more" / "↓ N more" row so the user
	// knows there's offscreen content.
	MaxRows int

	// viewTop is the index of the first session currently drawn.
	// Adjusted to follow the cursor on up/down moves.
	viewTop int
}

// sessionDialogAction is returned by HandleKey.
type sessionDialogAction struct {
	Select      bool
	Path        string
	Close       bool
	Renamed     bool
	RenameTitle string
	ToggleScope bool
	StartSearch bool
	Err         error
}

func newSessionDialog() *sessionDialog { return &sessionDialog{} }

// Open shows the dialog immediately and loads session entries on bounded
// background workers. Results are delivered to the caller so the UI goroutine
// remains responsible for mutating dialog state and rendering it.
func (d *sessionDialog) Open(parent context.Context, root, cwd string, allScope ...bool) <-chan sessionLoadEvent {
	if d.loadCancel != nil {
		d.loadCancel()
	}
	if d.searchCancel != nil {
		d.searchCancel()
		d.searchCancel = nil
	}
	if d.matchCancel != nil {
		d.matchCancel()
		d.matchCancel = nil
	}
	previousDone := d.loadDone
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	d.loadCancel = cancel
	d.loadGeneration++
	generation := d.loadGeneration
	done := make(chan struct{})
	d.loadDone = done
	d.sessions = nil
	d.baseSessions = nil
	d.cursor = 0
	d.viewTop = 0
	d.renaming = false
	d.rename = ""
	d.root = root
	d.cwd = cwd
	d.allScope = len(allScope) > 0 && allScope[0]
	d.paths = nil
	d.query = ""
	d.searching = false
	d.searchPending = false
	d.searchReady = false
	d.searchSegments = nil
	d.searchMatches = nil
	d.searchErr = ""
	d.searchEvents = nil
	d.loading = true
	d.loadingDone = 0
	d.loadingTotal = 0
	d.loadingStartedAt = time.Now()
	d.loadSlots = nil
	d.loadReadyThrough = 0
	d.active = true

	if ctxErr := ctx.Err(); ctxErr != nil {
		cancel()
		d.loadCancel = nil
		d.loading = false
		d.active = false
		d.loadDone = previousDone
		events := make(chan sessionLoadEvent)
		close(events)
		close(done)
		return events
	}

	events := make(chan sessionLoadEvent, 1)
	send := func(event sessionLoadEvent) bool {
		select {
		case events <- event:
			return true
		case <-ctx.Done():
			return false
		}
	}
	describeSession := d.describeSession
	if describeSession == nil {
		describeSession = core.DescribeSessionContext
	}
	go func() {
		defer close(done)
		defer close(events)
		if previousDone != nil {
			<-previousDone
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		paths := core.ListSessionPathsContext(ctx, root, cwd, len(allScope) > 0 && allScope[0])
		if !send(sessionLoadEvent{
			kind:       sessionLoadStarted,
			generation: generation,
			total:      len(paths),
			paths:      paths,
		}) {
			return
		}

		workerCount := len(paths)
		if workerCount > maxSessionLoadWorkers {
			workerCount = maxSessionLoadWorkers
		}
		jobs := make(chan sessionLoadJob)
		var wg sync.WaitGroup
		wg.Add(workerCount)
		for worker := 0; worker < workerCount; worker++ {
			go func() {
				defer wg.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case job, ok := <-jobs:
						if !ok {
							return
						}
						summary := describeSession(ctx, job.path)
						select {
						case <-ctx.Done():
							return
						default:
						}
						if !send(sessionLoadEvent{
							kind:       sessionLoadEntry,
							generation: generation,
							index:      job.index,
							summary:    summary,
						}) {
							return
						}
					}
				}
			}()
		}
	dispatch:
		for index, path := range paths {
			select {
			case jobs <- sessionLoadJob{index: index, path: path}:
			case <-ctx.Done():
				break dispatch
			}
		}
		close(jobs)
		wg.Wait()
		if ctx.Err() != nil {
			return
		}
		send(sessionLoadEvent{kind: sessionLoadFinished, generation: generation})
	}()
	return events
}

// ApplyLoad incorporates one result from Open. It must be called by the UI
// goroutine so Render and HandleKey never race with background file reads.
func (d *sessionDialog) ApplyLoad(event sessionLoadEvent) {
	if !d.active || event.generation != d.loadGeneration {
		return
	}
	switch event.kind {
	case sessionLoadStarted:
		d.loadingTotal = event.total
		d.loadingDone = 0
		d.paths = append([]string(nil), event.paths...)
		if d.searchPending {
			d.searchPending = false
			d.startSearchReader()
		}
		d.loadSlots = make([]sessionLoadSlot, event.total)
		d.loadReadyThrough = 0
		d.sessions = nil
		d.baseSessions = nil
	case sessionLoadEntry:
		if event.index < 0 || event.index >= len(d.loadSlots) || d.loadSlots[event.index].loaded {
			return
		}
		d.loadSlots[event.index] = sessionLoadSlot{loaded: true, summary: event.summary}
		d.loadingDone++
		previousReady := d.loadReadyThrough
		for d.loadReadyThrough < len(d.loadSlots) && d.loadSlots[d.loadReadyThrough].loaded {
			d.loadReadyThrough++
		}
		d.appendLoadedSessions(previousReady, d.loadReadyThrough)
	case sessionLoadFinished:
		d.finishLoad(true)
	}
}

// ApplyLoadClosed finalizes a load whose producer stopped without delivering a
// terminal event, such as when its parent context is canceled.
func (d *sessionDialog) ApplyLoadClosed() {
	if !d.active || !d.loading {
		return
	}
	d.finishLoad(false)
	d.active = false
}

func (d *sessionDialog) finishLoad(complete bool) {
	if complete {
		d.rebuildLoadedSessions(len(d.loadSlots))
	} else {
		d.sessions = nil
	}
	d.loading = false
	d.loadSlots = nil
	d.loadReadyThrough = 0
	if d.loadCancel != nil {
		d.loadCancel()
		d.loadCancel = nil
	}
}

// appendLoadedSessions exposes the newly contiguous portion of the path
// list. Paths are already newest-first, so this keeps the first visible entry
// stable while workers finish out of order without rescanning old slots.
func (d *sessionDialog) appendLoadedSessions(start, end int) {
	limit := len(d.loadSlots)
	if start < 0 {
		start = 0
	} else if start > limit {
		start = limit
	}
	if end < 0 {
		end = 0
	} else if end > limit {
		end = limit
	}
	if start >= end {
		return
	}
	for _, slot := range d.loadSlots[start:end] {
		if !slot.loaded || slot.summary.HideFromSessions || slot.summary.MessageCount == 0 {
			continue
		}
		d.baseSessions = append(d.baseSessions, slot.summary)
		if d.query == "" {
			d.sessions = append(d.sessions, slot.summary)
		}
	}
}

func (d *sessionDialog) rebuildLoadedSessions(limit int) {
	if limit > len(d.loadSlots) {
		limit = len(d.loadSlots)
	}
	filtered := make([]core.SessionSummary, 0, limit)
	for _, slot := range d.loadSlots[:limit] {
		if !slot.loaded || slot.summary.HideFromSessions || slot.summary.MessageCount == 0 {
			continue
		}
		filtered = append(filtered, slot.summary)
	}
	d.baseSessions = filtered
	if d.query == "" {
		d.sessions = append([]core.SessionSummary(nil), filtered...)
	} else {
		d.applySearchFilter()
	}
	if d.cursor >= len(d.sessions) {
		d.cursor = len(d.sessions) - 1
	}
	if d.cursor < 0 {
		d.cursor = 0
	}
}

// CursorPos returns the row/col for the terminal cursor when in
// rename mode. Returns -1, -1 otherwise.
func (d *sessionDialog) CursorPos() (row, col int) {
	if !d.renaming {
		return -1, -1
	}
	// Row: frameHeader + padDialogFrame blank + rename hint = row 3 (0-indexed).
	// Col: "  ▌ " prefix + text length.
	return 3, 4 + len([]rune(d.rename))
}

// Close hides the dialog and cancels any in-flight session reads.
func (d *sessionDialog) Close() {
	if d.loadCancel != nil {
		d.loadCancel()
		d.loadCancel = nil
	}
	if d.searchCancel != nil {
		d.searchCancel()
		d.searchCancel = nil
	}
	if d.matchCancel != nil {
		d.matchCancel()
		d.matchCancel = nil
	}
	d.loading = false
	d.searching = false
	d.loadSlots = nil
	d.loadReadyThrough = 0
	d.searchEvents = nil
	d.searchSegments = nil
	d.searchMatches = nil
	d.active = false
}

// StartSearch starts one background corpus read for this picker opening. Query
// edits reuse the retained corpus and never reopen a session file.
func (d *sessionDialog) StartSearch(parent context.Context) <-chan sessionSearchEvent {
	if d.searchEvents != nil {
		return d.searchEvents
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	d.searchContext = ctx
	d.searchCancel = cancel
	d.searching = true
	d.searchGeneration++
	events := make(chan sessionSearchEvent, 1)
	d.searchEvents = events
	if len(d.paths) == 0 && d.loading {
		d.searchPending = true
		return events
	}
	d.startSearchReader()
	return events
}

func (d *sessionDialog) startSearchReader() {
	if d.searchEvents == nil || d.searchCancel == nil {
		return
	}
	ctx := d.searchContext
	paths := append([]string(nil), d.paths...)
	generation := d.searchGeneration
	events := d.searchEvents
	go func() {
		segments := make([]core.SessionSearchSegment, 0)
		skipped := 0
		for _, path := range paths {
			if ctx.Err() != nil {
				return
			}
			read, err := core.ReadSessionSearchSegments(ctx, path)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				skipped++
				continue
			}
			segments = append(segments, read...)
		}
		select {
		case events <- sessionSearchEvent{kind: sessionSearchCorpusReady, generation: generation, segments: segments, skipped: skipped}:
		case <-ctx.Done():
		}
	}()
}

// ApplySearch incorporates corpus and match results on the UI goroutine.
func (d *sessionDialog) ApplySearch(event sessionSearchEvent) {
	if !d.active || event.generation != d.searchGeneration {
		return
	}
	switch event.kind {
	case sessionSearchCorpusReady:
		d.searching = false
		d.searchReady = true
		if event.skipped > 0 {
			d.searchErr = fmt.Sprintf("search skipped %d unreadable session(s)", event.skipped)
		}
		d.searchSegments = event.segments
		d.scheduleSearchMatch()
	case sessionSearchMatchesReady:
		if event.query != d.query {
			return
		}
		d.searchMatches = event.matches
		d.applySearchFilter()
	}
}

func (d *sessionDialog) scheduleSearchMatch() {
	if !d.searchReady || d.query == "" || d.searchEvents == nil || d.searchCancel == nil {
		return
	}
	if d.matchCancel != nil {
		d.matchCancel()
	}
	query := d.query
	segments := d.searchSegments
	generation := d.searchGeneration
	events := d.searchEvents
	ctx, cancel := context.WithCancel(d.searchContext)
	d.matchCancel = cancel
	go func() {
		matches := matchSessionSearchSegments(ctx, query, segments)
		if ctx.Err() != nil {
			return
		}
		select {
		case events <- sessionSearchEvent{kind: sessionSearchMatchesReady, generation: generation, query: query, matches: matches}:
		case <-ctx.Done():
		}
	}()
}

func matchSessionSearchSegments(ctx context.Context, query string, segments []core.SessionSearchSegment) map[string]sessionSearchMatch {
	query = core.NormalizeSessionSearchText(query)
	if query == "" {
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	texts := make([]string, len(segments))
	for index, segment := range segments {
		if ctx.Err() != nil {
			return nil
		}
		texts[index] = segment.Normalized
		if texts[index] == "" {
			texts[index] = core.NormalizeSessionSearchText(segment.Text)
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	ranked := fuzzy.Find(query, texts)
	matches := make(map[string]sessionSearchMatch)
	for _, match := range ranked {
		if ctx.Err() != nil || match.Index < 0 || match.Index >= len(segments) {
			return matches
		}
		segment := segments[match.Index]
		current := matches[segment.Path]
		current.count++
		if current.excerpt == "" || match.Score > current.score {
			current.excerpt = texts[match.Index]
			current.indexes = append([]int(nil), match.MatchedIndexes...)
			current.score = match.Score
		}
		matches[segment.Path] = current
	}
	return matches
}

func (d *sessionDialog) applySearchFilter() {
	if d.query == "" {
		d.sessions = append(d.sessions[:0], d.baseSessions...)
	} else {
		filtered := make([]core.SessionSummary, 0, len(d.baseSessions))
		for _, summary := range d.baseSessions {
			if _, ok := d.searchMatches[summary.Path]; ok {
				filtered = append(filtered, summary)
			}
		}
		d.sessions = filtered
	}
	if d.cursor >= len(d.sessions) {
		d.cursor = max(0, len(d.sessions)-1)
	}
}

// Active reports whether the dialog is visible and consumes input.
func (d *sessionDialog) Active() bool { return d != nil && d.active }

// Loading reports whether the dialog still has session entries in flight.
func (d *sessionDialog) Loading() bool { return d != nil && d.active && d.loading }

// Render returns the dialog lines.
func (d *sessionDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	var lines []string
	title := "sessions · this directory"
	if d.allScope {
		title = "sessions · all"
	}
	lines = append(lines, frameHeader(th, title, width))
	if d.loading {
		lines = append(lines, th.FGColor(th.Muted, d.loadingMessage(th)))
		if len(d.sessions) == 0 {
			lines = append(lines, th.FGColor(th.Muted, "session entries appear as they load"))
			lines = append(lines, frameRule(th, width))
			return lines
		}
	} else if len(d.sessions) == 0 {
		message := "no previous sessions for this directory"
		if d.allScope {
			message = "no previous sessions in this namespace"
		}
		if d.query != "" && d.searchReady {
			message = "no matching sessions"
		}
		lines = append(lines, th.FGColor(th.Muted, message))
		lines = append(lines, th.FGColor(th.Muted, "tab changes scope · esc closes"))
		lines = append(lines, frameRule(th, width))
		return lines
	}
	if d.renaming {
		lines = append(lines, th.FGColor(th.Muted, "rename session (enter save, esc cancel):"))
		text := d.rename
		if text == "" {
			text = th.FGColor(th.Muted, "session name")
		} else {
			text = th.FGColor(th.FG, text)
		}
		lines = append(lines, "  "+th.FGColor(th.Accent, "▌ ")+text)
		lines = append(lines, frameRule(th, width))
		return lines
	}
	hint := "↑/↓ pick · enter resume · / search · tab scope · r rename · esc cancel"
	if d.searchEvents != nil {
		hint = "search: " + d.query + "  (esc clear · tab scope)"
		if d.searching {
			hint += " · indexing"
		}
	}
	if d.loading {
		hint += " · loading"
	}
	lines = append(lines, th.FGColor(th.Muted, hint))
	if d.searchErr != "" {
		lines = append(lines, th.FGColor(th.Warning, d.searchErr))
	}

	// Viewport: windowed slice of d.sessions around d.cursor so a
	// list taller than the terminal still scrolls. Caller sets
	// MaxRows to the number of rows available for session entries
	// (i.e. excluding the header, hint, chrome). When it's zero or
	// bigger than the list, we draw everything.
	total := len(d.sessions)
	window := d.MaxRows
	if window <= 0 || window >= total {
		window = total
	}
	d.viewTop = clampViewTop(d.viewTop, d.cursor, window, total)
	viewBot := d.viewTop + window
	if viewBot > total {
		viewBot = total
	}

	// Top indicator: how many rows are above the viewport.
	if d.viewTop > 0 {
		hidden := d.viewTop
		lines = append(lines, th.FGColor(th.Muted, fmt.Sprintf("  ↑ %d more above", hidden)))
	}
	for i := d.viewTop; i < viewBot; i++ {
		s := d.sessions[i]
		match := d.searchMatches[s.Path]
		plain := "  " + formatSessionSearchRowPlain(s, width-2, match.count)
		if i == d.cursor {
			lines = append(lines, th.PadHighlight(plain, width))
		} else {
			lines = append(lines, th.FGColor(th.Muted, plain))
		}
	}
	// Bottom indicator: how many rows are below the viewport.
	if viewBot < total {
		hidden := total - viewBot
		lines = append(lines, th.FGColor(th.Muted, fmt.Sprintf("  ↓ %d more below", hidden)))
	}
	if d.allScope && d.cursor >= 0 && d.cursor < len(d.sessions) && d.sessions[d.cursor].CWD != "" {
		cwd := sanitizeSessionTreeText(friendlyPath(d.sessions[d.cursor].CWD))
		lines = append(lines, th.FGColor(th.Muted, "  cwd: "+cwd))
	}
	if d.query != "" && d.cursor >= 0 && d.cursor < len(d.sessions) {
		if match := d.searchMatches[d.sessions[d.cursor].Path]; match.excerpt != "" {
			lines = append(lines, "  "+highlightSessionSearchExcerpt(th, match.excerpt, match.indexes, width-2))
		}
	}
	lines = append(lines, frameRule(th, width))
	return lines
}

// clampViewTop returns a viewTop that keeps cursor visible in a
// window of the given size over a list of `total` rows. Leaves one
// row of padding above/below where possible so moving the cursor
// doesn't land right on the top/bottom edge — easier to see what
// direction you're moving.
func clampViewTop(viewTop, cursor, window, total int) int {
	if window <= 0 || total <= 0 {
		return 0
	}
	if window >= total {
		return 0
	}
	pad := 2
	if window < 6 {
		pad = 0
	}
	if cursor < viewTop+pad {
		viewTop = cursor - pad
	}
	if cursor >= viewTop+window-pad {
		viewTop = cursor - window + pad + 1
	}
	if viewTop < 0 {
		viewTop = 0
	}
	if viewTop+window > total {
		viewTop = total - window
	}
	return viewTop
}

// formatSessionRowPlain returns the session row body without any ANSI
// styling so the caller can wrap it in either a plain mute color or a
// full-row selection highlight. The returned string is guaranteed to
// fit within maxWidth visible characters so the terminal never soft-
// wraps it into the next row.
func formatSessionSearchRowPlain(s core.SessionSummary, maxWidth, matchCount int) string {
	row := formatSessionRowPlain(s, maxWidth)
	if matchCount == 0 {
		return row
	}
	count := fmt.Sprintf("  %d matches", matchCount)
	if len([]rune(row))+len([]rune(count)) > maxWidth {
		return row
	}
	return row + count
}

func highlightSessionSearchExcerpt(th tui.Theme, excerpt string, indexes []int, maxWidth int) string {
	limit := 240
	if maxWidth > 0 && maxWidth < limit {
		limit = maxWidth
	}
	excerpt, indexes = windowSessionSearchExcerpt(excerpt, indexes, limit)
	if len(indexes) == 0 {
		return th.FGColor(th.Muted, excerpt)
	}
	matched := make(map[int]bool, len(indexes))
	for _, index := range indexes {
		matched[index] = true
	}
	var out strings.Builder
	for index := 0; index < len(excerpt); {
		r, size := utf8.DecodeRuneInString(excerpt[index:])
		part := string(r)
		if matched[index] {
			out.WriteString(th.FGColor(th.Accent, part))
		} else {
			out.WriteString(th.FGColor(th.Muted, part))
		}
		index += size
	}
	return out.String()
}

func windowSessionSearchExcerpt(text string, indexes []int, limit int) (string, []int) {
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text, indexes
	}
	matchRune := 0
	if len(indexes) > 0 {
		for runeIndex, byteIndex := range runeByteOffsets(text) {
			if byteIndex <= indexes[0] {
				matchRune = runeIndex
			} else {
				break
			}
		}
	}
	room := limit - 3
	if room < 1 {
		return string(runes[:limit]), nil
	}
	start := max(0, matchRune-room/2)
	if start+room > len(runes) {
		start = len(runes) - room
	}
	end := min(len(runes), start+room)
	prefix := ""
	if start > 0 {
		prefix = "..."
	}
	suffix := ""
	if end < len(runes) {
		suffix = "..."
	}
	base := len(string(runes[:start]))
	stop := len(string(runes[:end]))
	rebased := make([]int, 0, len(indexes))
	for _, index := range indexes {
		if index >= base && index < stop {
			rebased = append(rebased, len(prefix)+index-base)
		}
	}
	return prefix + string(runes[start:end]) + suffix, rebased
}

func runeByteOffsets(text string) []int {
	offsets := make([]int, 0, len(text))
	for index := range text {
		offsets = append(offsets, index)
	}
	return offsets
}

func formatSessionRowPlain(s core.SessionSummary, maxWidth int) string {
	when := formatRelative(s.Started)
	summary := strings.TrimSpace(s.Title)
	if summary == "" {
		summary = strings.TrimSpace(s.FirstUserText)
	}
	if summary == "" {
		summary = "(empty)"
	}
	provider := sanitizeSessionTreeText(s.Provider)
	model := sanitizeSessionTreeText(s.Model)
	summary = sanitizeSessionTreeText(summary)
	left := fmt.Sprintf("%-14s  %s/%s  %d msgs  $%.4f  ",
		when, provider, model, s.MessageCount, s.TotalCost)
	room := maxWidth - len([]rune(left))
	if room < 4 {
		room = 4
	}
	runes := []rune(summary)
	if len(runes) > room {
		summary = string(runes[:room-3]) + "..."
	}
	row := left + summary
	// Hard clamp: ensure the full row never exceeds maxWidth.
	rowRunes := []rune(row)
	if len(rowRunes) > maxWidth {
		if maxWidth <= 3 {
			row = strings.Repeat(".", maxWidth)
		} else {
			row = string(rowRunes[:maxWidth-3]) + "..."
		}
	}
	return row
}

func (d *sessionDialog) loadingMessage(th tui.Theme) string {
	frames := th.SpinnerFrames
	if len(frames) == 0 {
		frames = []string{"⠋", "⠙", "⠚", "⠞", "⠖", "⠦", "⠴", "⠲", "⠳", "⠓"}
	}
	interval := th.SpinnerIntervalMS
	if interval <= 0 {
		interval = 80
	}
	idx := 0
	if !d.loadingStartedAt.IsZero() {
		elapsed := time.Since(d.loadingStartedAt)
		if elapsed < 0 {
			elapsed = 0
		}
		idx = int(elapsed/(time.Duration(interval)*time.Millisecond)) % len(frames)
	}
	progress := "Loading sessions"
	if d.loadingTotal > 0 {
		progress = fmt.Sprintf("Loading sessions (%d/%d)", d.loadingDone, d.loadingTotal)
	}
	return th.FGColor(th.Spinner, frames[idx]) + " " + th.FGColor(th.Muted, progress+" (esc cancel)")
}

func formatRelative(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	default:
		return t.Local().Format("2006-01-02")
	}
}

// HandleKey advances the dialog and returns an action to apply, if any.
func (d *sessionDialog) HandleKey(k tui.Key) sessionDialogAction {
	// Rename mode: type the new name.
	if d.renaming {
		switch k.Kind {
		case tui.KeyEnter:
			title := core.NormalizeSessionTitle(d.rename)
			path := ""
			renamed := false
			var renameErr error
			if title != "" && d.cursor >= 0 && d.cursor < len(d.sessions) {
				path = d.sessions[d.cursor].Path
				if err := core.RenameSession(path, title); err != nil {
					renameErr = err
				} else {
					d.sessions[d.cursor].Title = title
					for index := range d.baseSessions {
						if d.baseSessions[index].Path == path {
							d.baseSessions[index].Title = title
							break
						}
					}
					renamed = true
				}
			}
			d.renaming = false
			d.rename = ""
			return sessionDialogAction{Renamed: renamed, Path: path, RenameTitle: title, Err: renameErr}
		case tui.KeyEsc:
			d.renaming = false
			d.rename = ""
			return sessionDialogAction{}
		case tui.KeyBackspace:
			if len(d.rename) > 0 {
				r := []rune(d.rename)
				d.rename = string(r[:len(r)-1])
			}
			return sessionDialogAction{}
		case tui.KeyPaste:
			d.rename += k.Paste
			return sessionDialogAction{}
		case tui.KeyRune:
			if k.Rune != 0 {
				d.rename += string(k.Rune)
			}
			return sessionDialogAction{}
		}
		return sessionDialogAction{}
	}

	if k.Kind == tui.KeyTab {
		return sessionDialogAction{ToggleScope: true}
	}
	if k.Kind == tui.KeyRune && k.Rune == '/' && d.query == "" {
		return sessionDialogAction{StartSearch: true}
	}
	if d.query != "" || d.searchEvents != nil {
		switch k.Kind {
		case tui.KeyEsc:
			if d.query == "" {
				d.Close()
				return sessionDialogAction{Close: true}
			}
			d.query = ""
			d.searchMatches = nil
			d.applySearchFilter()
			return sessionDialogAction{}
		case tui.KeyBackspace:
			runes := []rune(d.query)
			if len(runes) > 0 {
				d.query = string(runes[:len(runes)-1])
			}
			d.applySearchFilter()
			d.scheduleSearchMatch()
			return sessionDialogAction{}
		case tui.KeyPaste:
			d.query += k.Paste
		case tui.KeyRune:
			if k.Rune != 0 {
				d.query += string(k.Rune)
			}
		default:
			// Navigation and selection operate on the current filtered rows.
			goto navigate
		}
		d.query = sanitizeSessionTreeText(d.query)
		if len([]rune(d.query)) > 256 {
			d.query = string([]rune(d.query)[:256])
		}
		d.applySearchFilter()
		d.scheduleSearchMatch()
		return sessionDialogAction{}
	}

	if d.loading && len(d.sessions) == 0 {
		if k.Kind == tui.KeyEsc {
			d.Close()
			return sessionDialogAction{Close: true}
		}
		return sessionDialogAction{}
	}

navigate:
	page := d.MaxRows
	if page <= 0 {
		page = 10
	}
	if page > 1 {
		page--
	}
	switch k.Kind {
	case tui.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
	case tui.KeyDown:
		if d.cursor < len(d.sessions)-1 {
			d.cursor++
		}
	case tui.KeyPageUp:
		d.cursor -= page
		if d.cursor < 0 {
			d.cursor = 0
		}
	case tui.KeyPageDown:
		d.cursor += page
		if d.cursor >= len(d.sessions) {
			d.cursor = len(d.sessions) - 1
			if d.cursor < 0 {
				d.cursor = 0
			}
		}
	case tui.KeyHome:
		d.cursor = 0
	case tui.KeyEnd:
		if len(d.sessions) > 0 {
			d.cursor = len(d.sessions) - 1
		}
	case tui.KeyEsc:
		d.Close()
		return sessionDialogAction{Close: true}
	case tui.KeyEnter:
		if len(d.sessions) == 0 {
			d.Close()
			return sessionDialogAction{Close: true}
		}
		s := d.sessions[d.cursor]
		d.Close()
		return sessionDialogAction{Select: true, Path: s.Path}
	case tui.KeyRune:
		if k.Rune == 'r' && !d.loading && len(d.sessions) > 0 {
			s := d.sessions[d.cursor]
			d.renaming = true
			if s.Title != "" {
				d.rename = s.Title
			} else {
				d.rename = ""
			}
			return sessionDialogAction{}
		}
	}
	return sessionDialogAction{}
}
