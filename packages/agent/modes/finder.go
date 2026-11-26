package modes

import (
	"context"
	"time"
	"unicode"

	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// finderSource supplies the rows of one finder tab. Implementations must
// honor ctx cancellation; the model never calls Search itself.
type finderSource interface {
	ID() string
	Title() string
	Search(ctx context.Context, query finderQuery) (finderResults, error)
}

// finderQuery is the source-agnostic search request built from the query line.
type finderQuery struct {
	Text string // raw query line; sources parse "@here" from it
	CWD  string
}

// finderResults is one complete answer from a source.
type finderResults struct {
	Items   []finderItem
	Partial bool
	Hint    string // shown instead of "no matches" when Items is empty
}

// finderItem is one selectable row. Key identifies it across result sets
// (for sessions, the session path).
type finderItem struct {
	Key    string
	Label  string
	Detail string
	Count  int
	Hits   []finderHit
	Live   bool
	LiveID string // daemon session id when Live
	Title  string // raw title, prefilled when renaming
	// LiveCWD is the daemon session's working directory when Live.
	LiveCWD string
}

// finderHit is one match inside an item; Ref is opaque to the model and
// interpreted by the source that produced it.
type finderHit struct {
	Ref       any
	Role      provider.Role
	Time      time.Time
	Compacted bool
	Text      string // snippet, filled lazily via ApplySnippets
}

type finderActionKind int

const (
	finderActionNone finderActionKind = iota
	finderActionSearch
	finderActionOpen
	finderActionRename
	finderActionDelete
	finderActionClose
)

// finderAction tells the caller what to do after a key. For Search, the
// caller runs Source.Search(Query) (debounced as it sees fit) and returns
// the answer through ApplyResults with Gen and TabID.
type finderAction struct {
	Kind   finderActionKind
	Source finderSource
	TabID  string
	Gen    uint64
	Query  finderQuery
	Item   finderItem
	Hit    int    // index into Item.Hits, -1 when the item has no hits
	Title  string // new title for Rename
}

// finderMode is what keys currently drive.
type finderMode int

const (
	finderModeList finderMode = iota
	finderModeRename
	finderModeConfirmDelete
)

// finderModel holds finder state and key handling; it does no rendering
// and no I/O.
type finderModel struct {
	active  bool
	tabs    []finderSource
	tab     int
	cwd     string
	query   []rune
	qcursor int
	qscroll int // first query rune shown when the query overflows

	items   []finderItem
	partial bool
	hint    string
	cursor  int
	hit     int
	viewTop int

	gen uint64
	// cards caches previews of items without hits, by Key. A card whose gen
	// is older than cardsGen still renders but is refetched.
	cards    map[string]finderCardEntry
	cardsGen uint64
	// results counts installed result sets; snippet fetches are tied to
	// it, not to gen, since a keystroke bumps gen before results change.
	results uint64
	loading bool
	// loadingSince drives the spinner animation of the in-flight search.
	loadingSince time.Time
	err          string

	// indexing is set while the index updates; indexDone/indexTotal count
	// scanned session files.
	indexing              bool
	indexDone, indexTotal int

	// live holds daemon sessions (port mode); raw is the last undecorated
	// result list so live data arriving later can be re-applied.
	live []finderItem
	raw  []finderItem
	// liveLoaded is set once the daemon live list arrived (port mode);
	// rename and delete wait for it.
	liveLoaded bool

	// deleted and renamed overlay results until an index update that
	// started after the action finishes (see overlaySeq).
	deleted    map[string]bool
	renamed    map[string]string
	overlaySeq uint64

	// mode is list, rename (rename edits the selected item's title; keys
	// in finder_actions.go), or delete confirmation.
	mode      finderMode
	rename    []rune
	renameKey string
	// renameCaret is the caret column computed by the last Render.
	renameCaret int
}

func newFinderModel(cwd string, tabs ...finderSource) *finderModel {
	return &finderModel{cwd: cwd, tabs: tabs}
}

// Open activates the finder on the given tab with an empty query and
// returns the initial search request.
func (m *finderModel) Open(tab int) finderAction {
	m.active = true
	m.tab = 0
	if tab >= 0 && tab < len(m.tabs) {
		m.tab = tab
	}
	m.query, m.qcursor, m.qscroll = nil, 0, 0
	m.items, m.partial, m.hint = nil, false, ""
	m.cursor, m.hit, m.viewTop = 0, 0, 0
	m.err = ""
	m.mode, m.rename, m.renameKey = finderModeList, nil, ""
	return m.search()
}

type finderCardEntry struct {
	card finderCard
	gen  uint64
}

// CardsGeneration is the id of the current card set; Refresh bumps it.
func (m *finderModel) CardsGeneration() uint64 { return m.cardsGen }

// SetCard stores the preview card of the item with key, fetched for card
// generation gen, and reports whether it was stored.
func (m *finderModel) SetCard(gen uint64, key string, card finderCard) bool {
	if !m.Active() || gen != m.cardsGen {
		return false
	}
	if m.cards == nil {
		m.cards = map[string]finderCardEntry{}
	}
	m.cards[key] = finderCardEntry{card: card, gen: gen}
	return true
}

// hasFreshCard reports whether key has a card of the current generation.
func (m *finderModel) hasFreshCard(key string) bool {
	e, ok := m.cards[key]
	return ok && e.gen == m.cardsGen
}

// Close deactivates the finder; in-flight results become stale.
func (m *finderModel) Close() {
	m.active = false
	m.loading = false
	m.mode, m.rename, m.renameKey = finderModeList, nil, ""
	m.gen++
}

func (m *finderModel) Active() bool  { return m != nil && m.active }
func (m *finderModel) Query() string { return string(m.query) }

// Generation is the id of the latest search request.
func (m *finderModel) Generation() uint64 { return m.gen }

// ResultsVersion is the id of the installed result set.
func (m *finderModel) ResultsVersion() uint64 { return m.results }

func (m *finderModel) source() finderSource {
	if m.tab < 0 || m.tab >= len(m.tabs) {
		return nil
	}
	return m.tabs[m.tab]
}

func (m *finderModel) selected() (finderItem, bool) {
	if m.cursor < 0 || m.cursor >= len(m.items) {
		return finderItem{}, false
	}
	return m.items[m.cursor], true
}

// search bumps the generation and returns a search request for the
// current tab and query.
func (m *finderModel) search() finderAction {
	m.gen++
	m.err = ""
	src := m.source()
	if src == nil {
		m.loading = false
		return finderAction{}
	}
	m.loading = true
	m.loadingSince = time.Now()
	text := string(m.query)
	return finderAction{
		Kind:   finderActionSearch,
		Source: src,
		TabID:  src.ID(),
		Gen:    m.gen,
		Query:  finderQuery{Text: text, CWD: m.cwd},
	}
}

// ApplyResults installs results for (gen, tabID) unless they are stale.
// The cursor stays on the same Key when it is still present.
func (m *finderModel) ApplyResults(gen uint64, tabID string, res finderResults, err error) bool {
	src := m.source()
	if !m.active || gen != m.gen || src == nil || src.ID() != tabID {
		return false
	}
	m.loading = false
	if err != nil {
		m.err = err.Error()
		return true
	}
	prevKey := ""
	if item, ok := m.selected(); ok {
		prevKey = item.Key
	}
	prevHit := m.hit
	// Copy: rename overlays edit rows in place and must not touch the
	// source's slice.
	m.raw, m.partial, m.hint = m.applyOverlay(append([]finderItem(nil), res.Items...)), res.Partial, res.Hint
	m.items = m.decorateLive(m.raw)
	m.results++
	m.cursor, m.hit = 0, 0
	if prevKey != "" {
		for i, item := range m.items {
			if item.Key == prevKey {
				m.cursor = i
				if prevHit < len(item.Hits) {
					m.hit = prevHit
				}
				break
			}
		}
	}
	if m.mode == finderModeConfirmDelete {
		m.mode = finderModeList
	}
	return true
}

// SetIndexing records index update progress for the header.
func (m *finderModel) SetIndexing(active bool, done, total int) {
	m.indexing, m.indexDone, m.indexTotal = active, done, total
}

// Refresh re-runs the current search, for example after the index changed.
func (m *finderModel) Refresh() finderAction {
	if !m.Active() {
		return finderAction{}
	}
	m.cardsGen++ // the index may have changed: refetch, keep showing old cards
	return m.search()
}

// ApplySnippets sets hit snippet texts (keyed by hit index) on the item
// with the given key, for the result set ResultsVersion returned. It reports whether
// anything was applied.
func (m *finderModel) ApplySnippets(version uint64, key string, texts map[int]string) bool {
	if !m.active || version != m.results {
		return false
	}
	for i := range m.items {
		if m.items[i].Key != key {
			continue
		}
		applied := false
		hits := append([]finderHit(nil), m.items[i].Hits...)
		for idx, text := range texts {
			if idx >= 0 && idx < len(hits) {
				hits[idx].Text = text
				applied = true
			}
		}
		m.items[i].Hits = hits
		return applied
	}
	return false
}

// HandleKey advances the finder and returns the action to apply.
func (m *finderModel) HandleKey(k tui.Key) finderAction {
	if m.mode == finderModeRename {
		return m.handleRenameKey(k)
	}
	if m.mode == finderModeConfirmDelete {
		m.mode = finderModeList
		if k.Kind == tui.KeyRune && (k.Rune == 'y' || k.Rune == 'Y') && !k.Ctrl && !k.Alt {
			if item, ok := m.selected(); ok {
				return finderAction{Kind: finderActionDelete, Item: item, Hit: m.hitIndex(item)}
			}
		}
		return finderAction{}
	}
	switch k.Kind {
	case tui.KeyEsc, tui.KeyCtrlC:
		m.Close()
		return finderAction{Kind: finderActionClose}
	case tui.KeyEnter:
		if item, ok := m.selected(); ok {
			return finderAction{Kind: finderActionOpen, Item: item, Hit: m.hitIndex(item)}
		}
	case tui.KeyCtrlR:
		m.startRename()
	case tui.KeyCtrlD:
		if _, ok := m.selected(); ok {
			m.mode = finderModeConfirmDelete
		}
	case tui.KeyUp:
		m.moveCursor(-1)
	case tui.KeyDown:
		m.moveCursor(1)
	case tui.KeyCtrlN:
		m.moveHit(1)
	case tui.KeyCtrlP:
		m.moveHit(-1)
	case tui.KeyTab:
		return m.switchTab(1)
	case tui.KeyShiftTab:
		return m.switchTab(-1)
	default:
		if m.editQuery(k) {
			return m.search()
		}
	}
	return finderAction{}
}

func (m *finderModel) hitIndex(item finderItem) int {
	if len(item.Hits) == 0 {
		return -1
	}
	return m.hit
}

func (m *finderModel) moveCursor(delta int) {
	if len(m.items) == 0 {
		return
	}
	m.cursor = min(max(m.cursor+delta, 0), len(m.items)-1)
	m.hit = 0
}

func (m *finderModel) moveHit(delta int) {
	item, ok := m.selected()
	if !ok || len(item.Hits) == 0 {
		return
	}
	m.hit = min(max(m.hit+delta, 0), len(item.Hits)-1)
}

func (m *finderModel) switchTab(delta int) finderAction {
	if len(m.tabs) < 2 {
		return finderAction{}
	}
	m.tab = (m.tab + delta + len(m.tabs)) % len(m.tabs)
	m.items, m.partial, m.hint = nil, false, ""
	m.cursor, m.hit, m.viewTop = 0, 0, 0
	return m.search()
}

// editQuery applies single-line editing keys; it reports whether the
// query text changed.
func (m *finderModel) editQuery(k tui.Key) bool {
	switch k.Kind {
	case tui.KeyRune:
		if k.Ctrl || !unicode.IsPrint(k.Rune) {
			return false
		}
		return m.insert([]rune{k.Rune})
	case tui.KeyPaste:
		return m.insert(finderPasteRunes(k.Paste))
	case tui.KeyBackspace:
		if m.qcursor == 0 {
			return false
		}
		m.query = append(m.query[:m.qcursor-1], m.query[m.qcursor:]...)
		m.qcursor--
		return true
	case tui.KeyCtrlW:
		end := m.qcursor
		start := end
		for start > 0 && unicode.IsSpace(m.query[start-1]) {
			start--
		}
		for start > 0 && !unicode.IsSpace(m.query[start-1]) {
			start--
		}
		if start == end {
			return false
		}
		m.query = append(m.query[:start], m.query[end:]...)
		m.qcursor = start
		return true
	case tui.KeyCtrlU:
		if m.qcursor == 0 {
			return false
		}
		m.query = append([]rune(nil), m.query[m.qcursor:]...)
		m.qcursor = 0
		return true
	case tui.KeyLeft:
		m.qcursor = max(m.qcursor-1, 0)
	case tui.KeyRight:
		m.qcursor = min(m.qcursor+1, len(m.query))
	case tui.KeyHome, tui.KeyCtrlA:
		m.qcursor = 0
	case tui.KeyEnd, tui.KeyCtrlE:
		m.qcursor = len(m.query)
	}
	return false
}

func (m *finderModel) insert(rs []rune) bool {
	if len(rs) == 0 {
		return false
	}
	next := make([]rune, 0, len(m.query)+len(rs))
	next = append(next, m.query[:m.qcursor]...)
	next = append(next, rs...)
	next = append(next, m.query[m.qcursor:]...)
	m.query = next
	m.qcursor += len(rs)
	return true
}

// finderPasteRunes keeps the printable runes of a paste for a single-line
// field; other whitespace (newlines, tabs) becomes a space.
func finderPasteRunes(paste string) []rune {
	var rs []rune
	for _, r := range paste {
		if unicode.IsPrint(r) {
			rs = append(rs, r)
		} else if unicode.IsSpace(r) {
			rs = append(rs, ' ')
		}
	}
	return rs
}
