package modes

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"unicode"

	"github.com/mattn/go-runewidth"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// sessionTreeBoundary describes what a tree row points at. Message rows are
// normal transcript rows. Empty and detached rows are real, selectable
// boundaries even though they do not correspond to a message.
type sessionTreeBoundary uint8

const (
	sessionTreeMessageBoundary sessionTreeBoundary = iota
	sessionTreeEmptyBoundary
	sessionTreeDetachedBoundary
)

func (b sessionTreeBoundary) String() string {
	switch b {
	case sessionTreeEmptyBoundary:
		return "empty"
	case sessionTreeDetachedBoundary:
		return "detached"
	default:
		return "message"
	}
}

// sessionTreeTarget is the stable description of a row in the dialog.
// EffectiveIndex and SelectionBoundary refer to the row's provider-valid
// segment. Ordinary rows use SourcePath's effective transcript; historical
// rows use the pre-compaction segment identified by HistorySegment. Keeping
// both values is important for a user row: selecting it branches before that
// row so the complete draft can be put back in the editor.
type sessionTreeTarget struct {
	SourcePath        string
	EffectiveIndex    int
	SelectionBoundary int
	Role              provider.Role
	UserDraft         string
	Boundary          sessionTreeBoundary
	Historical        bool
	HistorySegment    int
}

func (t sessionTreeTarget) IsBoundary() bool {
	return t.Boundary != sessionTreeMessageBoundary
}

func (t sessionTreeTarget) IsEmptyBoundary() bool {
	return t.Boundary == sessionTreeEmptyBoundary
}

func (t sessionTreeTarget) IsDetachedBoundary() bool {
	return t.Boundary == sessionTreeDetachedBoundary
}

// sessionTreeDialog renders a compact outline of the current session family.
// Branches are shown inline at the point where they forked, using indentation
// rather than file-level rows. Selecting a node checks out that point into a
// new branch.
type sessionTreeDialog struct {
	active bool
	items  []sessionTreeItem
	cursor int

	loading        bool
	loadingDone    int
	loadGeneration uint64
	loadCancel     context.CancelFunc
	loadDone       chan struct{}
	loadFamily     func(context.Context, string, string, string) ([]sessionTreeItem, error)

	// MaxRows is the maximum number of transcript/boundary rows rendered in
	// one viewport. The interactive host may set it from terminal height. A
	// bounded default is deliberately used when it is left at zero so a large
	// family cannot consume the whole chat pane.
	MaxRows int
	viewTop int
}

type sessionTreeItem struct {
	label      string
	messageIdx int // compatibility alias for target.EffectiveIndex
	turnNo     int
	role       provider.Role
	prompt     string // compatibility alias for target.UserDraft
	path       string // compatibility alias for target.SourcePath
	depth      int
	current    bool
	target     sessionTreeTarget
}

// sessionTreeAction is returned by HandleKey. Target is the structured API
// for the integration layer. The scalar fields are retained for the existing
// interactive.go integration and mirror a message target where possible.
type sessionTreeAction struct {
	Select bool
	Target sessionTreeTarget

	// Legacy/package integration fields. New callers should use Target,
	// especially Target.SelectionBoundary for empty or detached rows.
	MessageIdx int
	TurnNo     int
	Role       provider.Role
	Prompt     string
	Path       string
	Boundary   sessionTreeBoundary
	Close      bool
}

type sessionTreeSnapshot struct {
	path     string
	messages []provider.Message
	history  []core.SessionHistorySegment
}

type sessionTreeLoadEventKind uint8

const (
	sessionTreeLoadStarted sessionTreeLoadEventKind = iota
	sessionTreeLoadEntry
	sessionTreeLoadFinished
	sessionTreeLoadFailed
)

type sessionTreeLoadEvent struct {
	kind       sessionTreeLoadEventKind
	generation uint64
	item       sessionTreeItem
	err        error
}

const defaultSessionTreeRows = 12

func newSessionTreeDialog() *sessionTreeDialog { return &sessionTreeDialog{} }

// OpenMessages opens a snapshot supplied by the running agent. This is the
// fallback used before a session has a persisted source path.
func (d *sessionTreeDialog) OpenMessages(msgs []provider.Message) bool {
	items := buildSessionTreeItems("", msgs, 0, false)
	if len(items) == 0 {
		return false
	}
	d.activate(items, len(items)-1)
	return true
}

// OpenSessionFamily preflights the complete family containing currentPath,
// then activates the dialog in one step. A missing current path is not
// recoverable by choosing an arbitrary forest root. Failed reads leave the
// prior dialog state untouched.
func (d *sessionTreeDialog) OpenSessionFamily(root, cwd, currentPath string) bool {
	items, err := loadSessionTreeFamily(context.Background(), root, cwd, currentPath)
	if err != nil || len(items) == 0 {
		return false
	}
	d.activate(items, indexCurrentTreeItem(items))
	return true
}

// OpenSessionFamilyAsync shows the tree immediately and streams rows from the
// newest visible content toward the oldest. The caller applies returned events
// on its UI goroutine, so disk reads never contend with input or rendering.
func (d *sessionTreeDialog) OpenSessionFamilyAsync(parent context.Context, root, cwd, currentPath string, beforeLoad func()) <-chan sessionTreeLoadEvent {
	if d.loadCancel != nil {
		d.loadCancel()
	}
	previousDone := d.loadDone
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	d.loadCancel = cancel
	d.loadGeneration++
	generation := d.loadGeneration
	d.loadDone = make(chan struct{})
	d.items = nil
	d.cursor = 0
	d.viewTop = 0
	d.loading = true
	d.loadingDone = 0
	d.active = true

	// Keep the producer ahead of a redraw so a large family does not require
	// one main-loop iteration for every emitted row.
	events := make(chan sessionTreeLoadEvent, 64)
	send := func(event sessionTreeLoadEvent) bool {
		select {
		case events <- event:
			return true
		case <-ctx.Done():
			return false
		}
	}
	loadFamily := d.loadFamily
	done := d.loadDone
	go func() {
		defer close(done)
		defer close(events)
		if previousDone != nil {
			select {
			case <-previousDone:
			case <-ctx.Done():
				return
			}
		}
		if beforeLoad != nil {
			beforeLoad()
		}
		if ctx.Err() != nil {
			return
		}
		if !send(sessionTreeLoadEvent{kind: sessionTreeLoadStarted, generation: generation}) {
			return
		}
		emit := func(item sessionTreeItem) bool {
			return send(sessionTreeLoadEvent{kind: sessionTreeLoadEntry, generation: generation, item: item})
		}
		var err error
		if loadFamily != nil {
			items, loadErr := loadFamily(ctx, root, cwd, currentPath)
			err = loadErr
			for index := len(items) - 1; err == nil && index >= 0; index-- {
				if !emit(items[index]) {
					return
				}
			}
		} else {
			err = streamSessionTreeFamily(ctx, root, cwd, currentPath, emit)
		}
		if err != nil {
			if ctx.Err() == nil {
				send(sessionTreeLoadEvent{kind: sessionTreeLoadFailed, generation: generation, err: err})
			}
			return
		}
		send(sessionTreeLoadEvent{kind: sessionTreeLoadFinished, generation: generation})
	}()
	return events
}

// ApplyLoad incorporates a background event on the UI goroutine. A non-nil
// result means the complete family could not be read and should be reported by
// the host after it closes the overlay.
func (d *sessionTreeDialog) ApplyLoad(event sessionTreeLoadEvent) error {
	if !d.active || event.generation != d.loadGeneration {
		return nil
	}
	switch event.kind {
	case sessionTreeLoadStarted:
	case sessionTreeLoadEntry:
		d.items = append(d.items, event.item)
		d.loadingDone++
	case sessionTreeLoadFinished:
		if cursor := indexCurrentTreeItem(d.items); cursor >= 0 {
			d.cursor = cursor
		}
		d.finishLoad()
	case sessionTreeLoadFailed:
		d.finishLoad()
		d.active = false
		return event.err
	}
	return nil
}

func (d *sessionTreeDialog) ApplyLoadClosed() {
	if d.active && d.loading {
		d.finishLoad()
		d.active = false
	}
}

func (d *sessionTreeDialog) finishLoad() {
	d.loading = false
	if d.loadCancel != nil {
		d.loadCancel()
		d.loadCancel = nil
	}
}

func loadSessionTreeFamily(ctx context.Context, root, cwd, currentPath string) ([]sessionTreeItem, error) {
	roots, err := core.BuildSessionTreeFamilyContext(ctx, root, cwd, currentPath)
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("session tree: no readable session family")
	}
	familyRoot := roots[0]
	snapshots, err := preflightSessionTreeFamilyContext(ctx, familyRoot)
	if err != nil {
		return nil, err
	}
	items := flattenSessionFamilySnapshot(familyRoot, currentPath, snapshots)
	if len(items) == 0 || !sessionTreeItemsMeaningful(items, familyRoot.Summary.Path) {
		return nil, fmt.Errorf("session tree: no messages in this family")
	}
	return items, nil
}

func streamSessionTreeFamily(ctx context.Context, root, cwd, currentPath string, emit func(sessionTreeItem) bool) error {
	roots, err := core.BuildSessionTreeFamilyContext(ctx, root, cwd, currentPath)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		return fmt.Errorf("session tree: no readable session family")
	}
	familyRoot := roots[0]
	current := findSessionTreeNode(familyRoot, currentPath)
	if current == nil {
		return fmt.Errorf("session tree: current session is missing from its family")
	}
	currentHistory, err := core.ReadSessionHistoryContext(ctx, current.Summary.Path)
	if err != nil {
		return fmt.Errorf("session tree: read current %q: %w", current.Summary.Path, err)
	}
	if currentHistory.Meta.ID != current.Meta.ID {
		return fmt.Errorf("session tree: current session id changed in %q", current.Summary.Path)
	}
	currentSnapshots := map[string]sessionTreeSnapshot{
		current.Summary.Path: {path: current.Summary.Path, messages: sessionHistoryMessages(currentHistory), history: currentHistory.Segments},
	}
	preview := flattenSessionNode(current, currentPath, currentSnapshots, 0, current.Meta.ForkPoint, false, false)
	seen := make(map[sessionTreeTarget]struct{}, len(preview))
	for index := len(preview) - 1; index >= 0; index-- {
		seen[preview[index].target] = struct{}{}
		if !emit(preview[index]) {
			return ctx.Err()
		}
	}

	snapshots, err := preflightSessionTreeFamilyContext(ctx, familyRoot)
	if err != nil {
		return err
	}
	items := flattenSessionFamilySnapshot(familyRoot, currentPath, snapshots)
	if len(items) == 0 || !sessionTreeItemsMeaningful(items, familyRoot.Summary.Path) {
		return fmt.Errorf("session tree: no messages in this family")
	}
	for index := len(items) - 1; index >= 0; index-- {
		if _, alreadyEmitted := seen[items[index].target]; alreadyEmitted {
			continue
		}
		if !emit(items[index]) {
			return ctx.Err()
		}
	}
	return nil
}

func findSessionTreeNode(node *core.TreeNode, path string) *core.TreeNode {
	if node == nil {
		return nil
	}
	if node.Summary.Path == path {
		return node
	}
	for _, child := range node.Children {
		if found := findSessionTreeNode(child, path); found != nil {
			return found
		}
	}
	return nil
}

func sessionHistoryMessages(history core.SessionHistory) []provider.Message {
	if len(history.Segments) == 0 {
		return nil
	}
	return history.Segments[len(history.Segments)-1].Messages
}

func sessionTreeItemsMeaningful(items []sessionTreeItem, rootPath string) bool {
	for _, item := range items {
		if !item.target.IsBoundary() || item.path != rootPath {
			return true
		}
	}
	return false
}

func (d *sessionTreeDialog) activate(items []sessionTreeItem, cursor int) {
	if d.loadCancel != nil {
		d.loadCancel()
		d.loadCancel = nil
	}
	d.loading = false
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= len(items) {
		cursor = len(items) - 1
	}
	d.items = items
	d.cursor = cursor
	d.viewTop = 0
	d.active = true
}

// Close hides the dialog and cancels any in-flight family read.
func (d *sessionTreeDialog) Close() {
	if d.loadCancel != nil {
		d.loadCancel()
		d.loadCancel = nil
	}
	d.loading = false
	d.active = false
}

// CursorPos returns -1 because this dialog has no inline editor.
func (d *sessionTreeDialog) CursorPos() (row, col int) { return -1, -1 }

// Active reports whether the dialog consumes input.
func (d *sessionTreeDialog) Active() bool { return d != nil && d.active }

func (d *sessionTreeDialog) pageRows() int {
	if d.MaxRows > 0 {
		return d.MaxRows
	}
	return defaultSessionTreeRows
}

func (d *sessionTreeDialog) followCursor() {
	d.viewTop = clampSessionTreeViewTop(d.viewTop, d.cursor, d.pageRows(), len(d.items))
}

// HandleKey advances the cursor or resolves the selection.
func (d *sessionTreeDialog) HandleKey(k tui.Key) sessionTreeAction {
	if d == nil {
		return sessionTreeAction{}
	}
	page := d.pageRows()
	if page < 1 {
		page = 1
	}
	switch k.Kind {
	case tui.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
		d.followCursor()
	case tui.KeyDown:
		if d.cursor < len(d.items)-1 {
			d.cursor++
		}
		d.followCursor()
	case tui.KeyPageUp:
		d.cursor -= page
		if d.cursor < 0 {
			d.cursor = 0
		}
		d.followCursor()
	case tui.KeyPageDown:
		d.cursor += page
		if d.cursor >= len(d.items) {
			d.cursor = len(d.items) - 1
		}
		if d.cursor < 0 {
			d.cursor = 0
		}
		d.followCursor()
	case tui.KeyHome:
		d.cursor = 0
		d.followCursor()
	case tui.KeyEnd:
		if len(d.items) > 0 {
			d.cursor = len(d.items) - 1
		}
		d.followCursor()
	case tui.KeyEsc:
		d.Close()
		return sessionTreeAction{Close: true}
	case tui.KeyEnter:
		if len(d.items) == 0 || d.cursor < 0 || d.cursor >= len(d.items) {
			d.Close()
			return sessionTreeAction{Close: true}
		}
		it := d.items[d.cursor]
		d.Close()
		return sessionTreeActionForTarget(it.target, it.turnNo)
	}
	return sessionTreeAction{}
}

func sessionTreeActionForTarget(target sessionTreeTarget, turnNo int) sessionTreeAction {
	act := sessionTreeAction{
		Select:   true,
		Target:   target,
		TurnNo:   turnNo,
		Boundary: target.Boundary,
		Path:     target.SourcePath,
		Role:     target.Role,
		Prompt:   target.UserDraft,
		// MessageIdx is the effective index for ordinary rows. For a
		// boundary, use the old integration's "last included row" form
		// so msgIdx+1 still equals the safe selection boundary.
		MessageIdx: target.EffectiveIndex,
	}
	if target.IsBoundary() {
		act.MessageIdx = target.SelectionBoundary - 1
		act.Role = provider.RoleAssistant
		act.Prompt = ""
	}
	return act
}

// Render returns the dialog lines. MaxRows controls the number of selectable
// rows, not the chrome; every returned row is still hard-clipped to width.
func (d *sessionTreeDialog) Render(th tui.Theme, width int) []string {
	if !d.Active() {
		return nil
	}
	renderWidth := width
	if renderWidth < 0 {
		renderWidth = 0
	}
	line := func(s string) string { return truncateSessionTreeANSI(s, renderWidth) }

	lines := []string{line(frameHeader(th, "session tree", renderWidth))}
	if d.loading {
		progress := fmt.Sprintf("loading session history... %d loaded", d.loadingDone)
		lines = append(lines, line(th.FGColor(th.Muted, progress)))
		if len(d.items) == 0 {
			lines = append(lines,
				line(th.FGColor(th.Muted, "recent messages appear first; esc cancels")),
				line(frameRule(th, renderWidth)),
			)
			return lines
		}
	}
	if len(d.items) == 0 {
		lines = append(lines,
			line(th.FGColor(th.Muted, "no messages in this session yet")),
			line(th.FGColor(th.Muted, "press esc to close")),
			line(frameRule(th, renderWidth)),
		)
		return lines
	}
	hint := "session history and branches (↑/↓, pgup/pgdn, home/end, enter checkout, esc cancel):"
	if d.loading {
		hint = "session history and branches (↑/↓, pgup/pgdn, home/end, enter checkout, esc cancel; more loading):"
	}
	lines = append(lines, line(th.FGColor(th.Muted, hint)))

	d.followCursor()
	start := d.viewTop
	end := start + d.pageRows()
	if end > len(d.items) {
		end = len(d.items)
	}
	if start > end {
		start = end
	}
	if start > 0 {
		lines = append(lines, line(th.FGColor(th.Muted, fmt.Sprintf("  ↑ %d more above", start))))
	}
	for i := start; i < end; i++ {
		it := d.items[i]
		plain := fitSessionTreeItemPlain(it, renderWidth)
		if i == d.cursor {
			lines = append(lines, line(th.PadHighlight(plain, renderWidth)))
		} else {
			lines = append(lines, line(colorSessionTreeLine(th, plain)))
		}
	}
	if end < len(d.items) {
		lines = append(lines, line(th.FGColor(th.Muted, fmt.Sprintf("  ↓ %d more below", len(d.items)-end))))
	}
	lines = append(lines, line(th.FGColor(th.Muted, fmt.Sprintf("%d/%d", d.cursor+1, len(d.items)))), line(frameRule(th, renderWidth)))
	return lines
}

func clampSessionTreeViewTop(viewTop, cursor, window, total int) int {
	if window <= 0 || total <= 0 || window >= total {
		return 0
	}
	if cursor < viewTop {
		viewTop = cursor
	}
	if cursor >= viewTop+window {
		viewTop = cursor - window + 1
	}
	if viewTop < 0 {
		viewTop = 0
	}
	if viewTop+window > total {
		viewTop = total - window
	}
	return viewTop
}

func fitSessionTreeItemPlain(it sessionTreeItem, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	prefix := "  " + strings.Repeat("  ", it.depth)
	suffix := ""
	if it.current {
		suffix = "  [current]"
	}
	prefixWidth := runewidth.StringWidth(prefix)
	suffixWidth := runewidth.StringWidth(suffix)
	if prefixWidth+suffixWidth >= maxWidth {
		// A terminal narrower than the structural chrome cannot show the
		// complete row; retain the current marker as the highest-value suffix.
		return fitSessionTreeLabel(suffix, maxWidth)
	}
	labelWidth := maxWidth - prefixWidth - suffixWidth
	return prefix + fitSessionTreeLabel(it.label, labelWidth) + suffix
}

// preflightSessionTreeFamilyContext reads every node and returns the shared
// effective snapshots used by all flattening and target construction for one
// open operation.
func preflightSessionTreeFamilyContext(ctx context.Context, root *core.TreeNode) (map[string]sessionTreeSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("session tree: nil family root")
	}
	snapshots := make(map[string]sessionTreeSnapshot)
	visiting := make(map[string]bool)
	var walk func(*core.TreeNode) error
	walk = func(node *core.TreeNode) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if node == nil {
			return nil
		}
		path := node.Summary.Path
		if visiting[path] {
			return fmt.Errorf("session tree: cycle at %q", path)
		}
		if _, ok := snapshots[path]; ok {
			return nil
		}
		visiting[path] = true
		history, err := core.ReadSessionHistoryContext(ctx, path)
		if err != nil {
			delete(visiting, path)
			return fmt.Errorf("session tree: read %q: %w", path, err)
		}
		if history.Meta.ID != node.Meta.ID {
			delete(visiting, path)
			return fmt.Errorf("session tree: session id changed in %q", path)
		}
		var messages []provider.Message
		if len(history.Segments) > 0 {
			messages = history.Segments[len(history.Segments)-1].Messages
		}
		snapshots[path] = sessionTreeSnapshot{path: path, messages: messages, history: history.Segments}
		for _, child := range node.Children {
			if err := walk(child); err != nil {
				delete(visiting, path)
				return err
			}
		}
		delete(visiting, path)
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	return snapshots, nil
}

func flattenSessionFamilySnapshot(root *core.TreeNode, currentPath string, snapshots map[string]sessionTreeSnapshot) []sessionTreeItem {
	return flattenSessionNode(root, currentPath, snapshots, 0, 0, true, false)
}

// flattenSessionNode emits the visible suffix for a node and inserts children
// at their effective fork boundary. Fork points are counts, so fork N appears
// after message N-1. A fork beyond the parent's effective snapshot is kept at
// the end and gets a detached boundary instead of silently disappearing.
func flattenSessionNode(node *core.TreeNode, currentPath string, snapshots map[string]sessionTreeSnapshot, depth, parentLen int, rootNode, detachedFromParent bool) []sessionTreeItem {
	if node == nil {
		return nil
	}
	snapshot, ok := snapshots[node.Summary.Path]
	if !ok {
		return nil
	}
	msgs := snapshot.messages
	start := 0
	var selectionBoundary int
	boundary := sessionTreeMessageBoundary
	rawFork := 0
	if !rootNode {
		rawFork = node.Meta.ForkPoint
		switch {
		case detachedFromParent || rawFork < 0 || rawFork > parentLen || rawFork > len(msgs):
			// Historical placement no longer maps to either snapshot. Keep the
			// branch visible at the parent's tail, but render its complete
			// current snapshot rather than slicing it at the stale fork point.
			boundary = sessionTreeDetachedBoundary
			start = 0
			selectionBoundary = len(msgs)
		default:
			start = rawFork
			selectionBoundary = rawFork
			if start >= len(msgs) {
				boundary = sessionTreeEmptyBoundary
			}
		}
	} else if len(msgs) == 0 {
		boundary = sessionTreeEmptyBoundary
		selectionBoundary = 0
	} else {
		selectionBoundary = len(msgs)
	}

	historyItems := buildSessionTreeHistoryItems(snapshot.path, snapshot.history, depth, snapshot.path == currentPath)
	allByIndex := make(map[int]sessionTreeItem, len(msgs))
	for _, item := range historyItems {
		if !item.target.Historical {
			allByIndex[item.target.EffectiveIndex] = item
		}
	}
	childrenAt := make(map[int][]*core.TreeNode)
	var stale []*core.TreeNode
	for _, child := range node.Children {
		fork := child.Meta.ForkPoint
		attach := fork
		if attach < start {
			attach = start
		}
		childSnapshot, childLoaded := snapshots[child.Summary.Path]
		// A branch with no post-fork messages still needs its explicit empty
		// boundary even when the copied historical prefix is no longer
		// comparable after compaction; there is no suffix whose placement could
		// be wrong. Non-empty suffixes require the prefix identity check.
		emptySuffix := childLoaded && fork >= 0 && fork == len(childSnapshot.messages)
		if fork < 0 || fork > len(msgs) || !childLoaded ||
			(!emptySuffix && !sessionTreePrefixMatches(msgs, childSnapshot.messages, fork)) {
			// Numeric fork points are only placement hints. A parent may have
			// been compacted and later grown back to the old length, so bounds
			// alone are insufficient: verify that the child's copied prefix is
			// still the parent's current prefix before placing the edge.
			stale = append(stale, child)
			continue
		}
		childrenAt[attach] = append(childrenAt[attach], child)
	}

	var out []sessionTreeItem
	for _, item := range historyItems {
		if item.target.Historical {
			out = append(out, item)
		}
	}
	if boundary != sessionTreeMessageBoundary {
		turn := treeTurnAtBoundary(msgs, selectionBoundary)
		boundaryCurrent := snapshot.path == currentPath && (len(msgs) == 0 || boundary == sessionTreeEmptyBoundary)
		out = append(out, makeSessionTreeBoundaryItem(snapshot.path, selectionBoundary, depth, boundaryCurrent, boundary, rawFork, len(msgs), turn))
	}

	emitChildren := func(children []*core.TreeNode, childDetached bool) {
		for _, child := range children {
			out = append(out, flattenSessionNode(child, currentPath, snapshots, depth+1, len(msgs), false, childDetached)...)
		}
	}

	// A fork at zero is shown before the first visible message. For a branch,
	// children that forked in the hidden copied prefix are normalized to its
	// first visible index above, so they remain discoverable too.
	emitChildren(childrenAt[start], false)
	for idx := start; idx < len(msgs); idx++ {
		if item, ok := allByIndex[idx]; ok {
			out = append(out, item)
		}
		emitChildren(childrenAt[idx+1], false)
	}
	// Stale children are attached after the current effective snapshot. Their
	// own row makes the missing historical boundary explicit.
	emitChildren(stale, true)
	return out
}

func makeSessionTreeBoundaryItem(path string, effectiveIndex, depth int, current bool, boundary sessionTreeBoundary, forkPoint, snapshotLen, turn int) sessionTreeItem {
	label := fmt.Sprintf("%s branch boundary", boundary.String())
	if boundary == sessionTreeDetachedBoundary {
		label = fmt.Sprintf("detached branch boundary (fork %d; snapshot %d)", forkPoint, snapshotLen)
	}
	target := sessionTreeTarget{
		SourcePath:        path,
		EffectiveIndex:    effectiveIndex,
		SelectionBoundary: effectiveIndex,
		Boundary:          boundary,
	}
	return sessionTreeItem{
		label:      label,
		messageIdx: effectiveIndex,
		turnNo:     turn,
		path:       path,
		depth:      depth,
		current:    current,
		target:     target,
	}
}

func sessionTreePrefixMatches(parent, child []provider.Message, fork int) bool {
	if fork < 0 || fork > len(parent) || fork > len(child) {
		return false
	}
	return reflect.DeepEqual(parent[:fork], child[:fork])
}

func treeTurnAtBoundary(msgs []provider.Message, boundary int) int {
	if boundary < 0 {
		boundary = 0
	}
	if boundary > len(msgs) {
		boundary = len(msgs)
	}
	turn := 0
	for _, msg := range msgs[:boundary] {
		if msg.Role == provider.RoleUser {
			turn++
		}
	}
	if turn == 0 {
		return 1
	}
	return turn
}

func buildSessionTreeItems(path string, msgs []provider.Message, depth int, currentPath bool) []sessionTreeItem {
	return buildSessionTreeSegmentItems(path, msgs, depth, currentPath, false, -1, nil)
}

func buildSessionTreeHistoryItems(path string, segments []core.SessionHistorySegment, depth int, currentPath bool) []sessionTreeItem {
	var out []sessionTreeItem
	for segmentIdx, segment := range segments {
		historical := segmentIdx < len(segments)-1
		skip := compactionTailIndices(segments, segmentIdx)
		out = append(out, buildSessionTreeSegmentItems(path, segment.Messages, depth, currentPath && !historical, historical, segmentIdx, skip)...)
	}
	return out
}

func buildSessionTreeSegmentItems(path string, msgs []provider.Message, depth int, currentPath, historical bool, historySegment int, skip map[int]bool) []sessionTreeItem {
	out := make([]sessionTreeItem, 0, len(msgs))
	turn := 0
	lastTurn := 0
	visible := make([]int, 0, len(msgs))
	for idx, msg := range msgs {
		if skip[idx] || !isForkableSessionTreeMessage(msg) {
			continue
		}
		visible = append(visible, idx)
	}
	for visibleIndex, idx := range visible {
		msg := msgs[idx]
		if msg.Role == provider.RoleUser {
			turn++
			lastTurn = turn
		} else if lastTurn == 0 {
			lastTurn = 1
		}
		roleLabel := sessionTreeRoleLabel(msg.Role)
		preview := sessionTreePreview(msg)
		boundary := idx + 1
		draft := ""
		if msg.Role == provider.RoleUser {
			boundary = idx
			draft = completeUserDraft(msg)
		}
		target := sessionTreeTarget{
			SourcePath:        path,
			EffectiveIndex:    idx,
			SelectionBoundary: boundary,
			Role:              msg.Role,
			UserDraft:         draft,
			Boundary:          sessionTreeMessageBoundary,
			Historical:        historical,
			HistorySegment:    historySegment,
		}
		out = append(out, sessionTreeItem{
			label:      fmt.Sprintf("%s: %s", roleLabel, preview),
			messageIdx: idx,
			turnNo:     lastTurn,
			role:       msg.Role,
			prompt:     draft,
			path:       path,
			depth:      depth,
			current:    currentPath && visibleIndex == len(visible)-1,
			target:     target,
		})
	}
	return out
}

func compactionTailIndices(segments []core.SessionHistorySegment, segmentIdx int) map[int]bool {
	if segmentIdx <= 0 || segmentIdx >= len(segments) || !segments[segmentIdx].Compacted {
		return nil
	}
	current := segments[segmentIdx].Messages
	previous := segments[segmentIdx-1].Messages
	if len(current) < 2 || len(previous) == 0 || !isCompactionSummaryMessage(current[0]) && current[0].Meta["compaction"] != "true" {
		return nil
	}
	maxTail := len(current) - 1
	if len(previous) < maxTail {
		maxTail = len(previous)
	}
	for tailLen := maxTail; tailLen > 0; tailLen-- {
		if !reflect.DeepEqual(previous[len(previous)-tailLen:], current[1:1+tailLen]) {
			continue
		}
		skip := make(map[int]bool, tailLen)
		for idx := 1; idx <= tailLen; idx++ {
			skip[idx] = true
		}
		return skip
	}
	return nil
}

func isForkableSessionTreeMessage(msg provider.Message) bool {
	if isHiddenTranscriptMessage(msg) || msg.Meta[shellEscapeMetaKey] == "true" || msg.Meta[autoCompactContinueMetaKey] == "true" || msg.Meta["compaction"] == "true" || isCompactionSummaryMessage(msg) {
		return false
	}
	switch msg.Role {
	case provider.RoleUser:
		return isForkableUserMessage(msg)
	case provider.RoleAssistant:
		if messageHasToolCall(msg) {
			return false
		}
		for _, content := range msg.Content {
			switch block := content.(type) {
			case provider.TextBlock:
				if strings.TrimSpace(block.Text) != "" {
					return true
				}
			case provider.ImageBlock:
				return true
			}
		}
	}
	return false
}

func isForkableUserMessage(msg provider.Message) bool {
	if msg.Role != provider.RoleUser || isHiddenTranscriptMessage(msg) || msg.Meta[shellEscapeMetaKey] == "true" || msg.Meta[autoCompactContinueMetaKey] == "true" || msg.Meta["compaction"] == "true" || isCompactionSummaryMessage(msg) {
		return false
	}
	return sessionTreePreview(msg) != "(empty)"
}

func isCompactionSummaryMessage(msg provider.Message) bool {
	if msg.Role != provider.RoleUser {
		return false
	}
	for _, content := range msg.Content {
		if block, ok := content.(provider.TextBlock); ok && strings.HasPrefix(strings.TrimSpace(block.Text), "## Context Summary (compacted)") {
			return true
		}
	}
	return false
}

func completeUserDraft(msg provider.Message) string {
	var text []string
	for _, c := range msg.Content {
		if tb, ok := c.(provider.TextBlock); ok {
			text = append(text, tb.Text)
		}
	}
	return strings.Join(text, "\n")
}

func indexCurrentTreeItem(items []sessionTreeItem) int {
	for i, it := range items {
		if it.current {
			return i
		}
	}
	if len(items) == 0 {
		return 0
	}
	return len(items) - 1
}

func sessionTreeRoleLabel(role provider.Role) string {
	switch role {
	case provider.RoleUser:
		return "you"
	case provider.RoleAssistant:
		return "zut"
	case provider.RoleTool:
		return "tool"
	default:
		return string(role)
	}
}

func sessionTreePreview(msg provider.Message) string {
	var parts []string
	for _, c := range msg.Content {
		switch b := c.(type) {
		case provider.TextBlock:
			if text := sanitizeSessionTreeText(b.Text); text != "" {
				parts = append(parts, text)
			}
		case provider.ImageBlock:
			parts = append(parts, "[image]")
		case provider.ToolCallBlock:
			parts = append(parts, "tool "+sanitizeSessionTreeText(b.Name))
		case provider.ToolResultBlock:
			if b.IsError {
				parts = append(parts, "tool result error")
			} else {
				parts = append(parts, "tool result")
			}
		case provider.ReasoningBlock:
			if b.Summary != "" {
				parts = append(parts, "reasoning")
			}
		}
	}
	if len(parts) == 0 {
		return "(empty)"
	}
	return strings.Join(parts, " ")
}

// sanitizeSessionTreeText keeps previews single-line and prevents transcript
// content from injecting terminal control sequences into the dialog frame.
func sanitizeSessionTreeText(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i := 0; i < len(runes); {
		if end, ok := sessionTreeCSIEnd(runes, i); ok {
			i = end
			continue
		}
		if runes[i] == '\x1b' && i+1 < len(runes) && runes[i+1] == ']' {
			// OSC sequences end at BEL or ST. Drop the whole sequence when
			// present.
			i += 2
			for i < len(runes) && runes[i] != '\a' {
				if runes[i] == '\x1b' && i+1 < len(runes) && runes[i+1] == '\\' {
					i += 2
					break
				}
				i++
			}
			if i < len(runes) && runes[i] == '\a' {
				i++
			}
			continue
		}
		if runes[i] == '\x1b' {
			// An isolated ESC (or a two-byte escape) is dropped by itself.
			i++
			continue
		}
		if unicode.IsControl(runes[i]) {
			b.WriteByte(' ')
		} else {
			b.WriteRune(runes[i])
		}
		i++
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func colorSessionTreeLine(th tui.Theme, line string) string {
	plain := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(plain, "you:"):
		return th.FGColor(th.FG, line)
	case strings.HasPrefix(plain, "zut:"):
		return th.FGColor(th.Muted, line)
	case strings.HasPrefix(plain, "tool:"):
		return th.FGColor(th.ToolOut, line)
	default:
		return th.FGColor(th.Muted, line)
	}
}

// fitSessionTreeLabel truncates by terminal cells, not Go runes. This is
// intentionally also used for the indentation and current marker together so
// neither can push a row past the terminal edge.
func fitSessionTreeLabel(label string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if runewidth.StringWidth(label) <= maxWidth {
		return label
	}
	if maxWidth <= 3 {
		return strings.Repeat(".", maxWidth)
	}
	return truncateSessionTreePlain(label, maxWidth-3) + "..."
}

func truncateSessionTreePlain(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := runewidth.RuneWidth(r)
		if w > 0 && used+w > maxWidth {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String()
}

// truncateSessionTreeANSI is the dialog-local equivalent of tui's renderer
// clipping helper. Keeping it here avoids changing tui for one dialog while
// still making headers, hints, colors, and highlighted rows width-safe.
func truncateSessionTreeANSI(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if sessionTreeANSIWidth(s) <= maxWidth {
		return s
	}
	runes := []rune(s)
	var b strings.Builder
	used := 0
	clipped := false
	for i := 0; i < len(runes); {
		if end, ok := sessionTreeCSIEnd(runes, i); ok {
			b.WriteString(string(runes[i:end]))
			i = end
			continue
		}
		w := runewidth.RuneWidth(runes[i])
		if w > 0 && used+w > maxWidth {
			clipped = true
			break
		}
		b.WriteRune(runes[i])
		used += w
		i++
	}
	if clipped && strings.Contains(s, "\x1b[") {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

func sessionTreeANSIWidth(s string) int {
	runes := []rune(s)
	width := 0
	for i := 0; i < len(runes); {
		if end, ok := sessionTreeCSIEnd(runes, i); ok {
			i = end
			continue
		}
		width += runewidth.RuneWidth(runes[i])
		i++
	}
	return width
}

func sessionTreeCSIEnd(runes []rune, start int) (int, bool) {
	if start+1 >= len(runes) || runes[start] != '\x1b' || runes[start+1] != '[' {
		return start, false
	}
	for i := start + 2; i < len(runes); i++ {
		if runes[i] >= 0x40 && runes[i] <= 0x7e {
			return i + 1, true
		}
	}
	return len(runes), true
}
