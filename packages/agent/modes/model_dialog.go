package modes

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mattn/go-runewidth"

	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// modelDialog is the model picker. It lists the models of the providers
// the user can reach, in provider.PickerModels order, with one tab per
// provider plus "all". Without a query the "all" tab groups models under
// provider headers (the active model's provider first) and hides dated
// snapshots; typing ranks every model by fuzzy score (see
// rankPickerModels). The line under the list details the highlighted
// model.
type modelDialog struct {
	active        bool
	all           []provider.PickerModel // reachable models, picker order
	texts         []pickerModelText      // search text of all, same index
	tabs          []string               // "" (all), then provider ids
	tab           int
	view          []provider.PickerModel // selectable rows, display order
	cursor        int
	viewTop       int    // first display line of the list
	listRows      int    // list height of the last Render, for page keys
	queryRow      int    // row of the query line in the last Render
	current       string // current model id
	currentProv   string // its provider; "" matches the id under any provider
	reasoning     string // current reasoning level, shown for discoverability
	showReasoning bool   // whether this picker can route to /reasoning
	query         pickerQuery
}

// modelDialogAction is returned by HandleKey.
type modelDialogAction struct {
	Select           bool
	Provider         string
	Model            string
	ReasoningChanged bool
	Reasoning        string
	Close            bool
}

const (
	modelPickerAllTab     = "all"
	modelPickerMinRows    = 3
	modelPickerFooterHelp = "tab provider · ↑↓ pick · enter select · esc close"
)

func newModelDialog() *modelDialog {
	return &modelDialog{}
}

// reachablePickerModels returns, in picker order, the models of the
// providers the user can actually reach: a provider is shown only when it
// has a resolvable credential (api key, oauth subscription, or the
// always-available ollama). An empty loggedInProviders list therefore
// yields an empty picker rather than dumping the entire ~900-model
// catalog. Callers build it before taking the interactive mutex: ordering
// the catalog is the costly part of opening the picker.
func reachablePickerModels(loggedInProviders []string) []provider.PickerModel {
	provSet := map[string]bool{}
	for _, p := range loggedInProviders {
		provSet[p] = true
	}
	var filtered []provider.Model
	for _, m := range provider.Active() {
		if provSet[m.Provider] {
			filtered = append(filtered, m)
		}
	}
	return provider.PickerModels(filtered)
}

// Open shows the dialog over models (see reachablePickerModels).
// currentProvider/current is the active model so it can be pre-selected
// and marked.
func (d *modelDialog) Open(currentProvider, current string, models []provider.PickerModel, reasoning ...string) {
	d.active = true
	d.reasoning = ""
	d.showReasoning = len(reasoning) > 0
	if d.showReasoning {
		d.reasoning = provider.NormalizeReasoning(reasoning[0])
	}
	d.setModels(models, currentProvider, current)
}

// setModels lists models (picker order) with current pre-selected, on the
// "all" tab with an empty query.
func (d *modelDialog) setModels(models []provider.PickerModel, currentProvider, current string) {
	d.current, d.currentProv = current, currentProvider
	d.all = d.currentProviderFirst(append([]provider.PickerModel(nil), models...))
	d.texts = pickerModelTexts(d.all)
	d.tabs = []string{""}
	for _, m := range d.all {
		if d.tabs[len(d.tabs)-1] != m.Provider {
			d.tabs = append(d.tabs, m.Provider)
		}
	}
	d.tab = 0
	d.query.Reset()
	d.refilter()
}

// isCurrent reports whether m is the active model.
func (d *modelDialog) isCurrent(m provider.PickerModel) bool {
	return d.current != "" && m.ID == d.current && (d.currentProv == "" || m.Provider == d.currentProv)
}

// currentProviderFirst moves the provider of the current model ahead of
// the others, keeping picker order otherwise.
func (d *modelDialog) currentProviderFirst(models []provider.PickerModel) []provider.PickerModel {
	first := ""
	for _, m := range models {
		if d.isCurrent(m) {
			first = m.Provider
			break
		}
	}
	if first == "" {
		return models
	}
	sort.SliceStable(models, func(i, j int) bool {
		return models[i].Provider == first && models[j].Provider != first
	})
	return models
}

// Close hides the dialog.
func (d *modelDialog) Close() { d.active = false }

// Active reports whether the dialog is visible and consumes input.
func (d *modelDialog) Active() bool { return d != nil && d.active }

// grouped reports whether the list shows provider headers.
func (d *modelDialog) grouped() bool {
	return d.tab == 0 && d.query.String() == "" && len(d.tabs) > 2
}

// refilter rebuilds view from all according to the tab and query. The
// cursor goes to the best match while searching, else to the current
// model (if visible) or the first row.
func (d *modelDialog) refilter() {
	models := d.all
	texts := d.texts
	if d.tab > 0 {
		models = make([]provider.PickerModel, 0, len(d.all))
		texts = make([]pickerModelText, 0, len(d.all))
		for i, m := range d.all {
			if m.Provider == d.tabs[d.tab] {
				models = append(models, m)
				texts = append(texts, d.texts[i])
			}
		}
	}
	d.view = rankPickerModels(models, texts, d.query.String(), d.isCurrent)
	d.cursor, d.viewTop = 0, 0
	if d.query.String() != "" {
		return
	}
	for i, m := range d.view {
		if d.isCurrent(m) {
			d.cursor = i
			break
		}
	}
}

// pickerModelText is the search text of a model: "provider name id" and
// its normalizeModelQuery form.
type pickerModelText struct{ hay, norm string }

func pickerModelTexts(models []provider.PickerModel) []pickerModelText {
	out := make([]pickerModelText, len(models))
	for i, m := range models {
		hay := m.Provider + " " + m.DisplayName + " " + m.ID
		out[i] = pickerModelText{hay: hay, norm: normalizeModelQuery(hay)}
	}
	return out
}

// rankPickerModels filters models (texts[i] is the search text of
// models[i]) for a picker query. An empty query
// keeps the given order and drops dated snapshots of listed aliases,
// except the one keep accepts (the current model; keep may be nil).
// Otherwise every query word must fuzzy-match "provider name id"; models
// containing a word literally (ignoring punctuation and case, so "opus46"
// finds claude-opus-4-6) rank first, then by fuzzy score, ties keeping
// the given order.
func rankPickerModels(models []provider.PickerModel, texts []pickerModelText, query string, keep func(provider.PickerModel) bool) []provider.PickerModel {
	words := strings.Fields(query)
	if len(words) == 0 {
		out := make([]provider.PickerModel, 0, len(models))
		for _, m := range models {
			if m.SnapshotOf == "" || (keep != nil && keep(m)) {
				out = append(out, m)
			}
		}
		return out
	}
	haystacks := make([]string, len(texts))
	for i, t := range texts {
		haystacks[i] = t.hay
	}
	const literalBonus = 1000
	total := make([]int, len(models))
	matchAll := make([]bool, len(models))
	for i := range matchAll {
		matchAll[i] = true
	}
	for _, word := range words {
		scores, matched := pickerFuzzyScores(word, haystacks)
		needle := normalizeModelQuery(word)
		for i := range models {
			if !matched[i] {
				matchAll[i] = false
				continue
			}
			total[i] += scores[i]
			if needle != "" && strings.Contains(texts[i].norm, needle) {
				total[i] += literalBonus
			}
		}
	}
	idx := make([]int, 0, len(models))
	for i := range models {
		if matchAll[i] {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return total[idx[a]] > total[idx[b]] })
	out := make([]provider.PickerModel, len(idx))
	for n, i := range idx {
		out[n] = models[i]
	}
	return out
}

// normalizeModelQuery lowercases and strips punctuation so substring
// matching works on both the query and haystacks. "opus46" and
// "opus-4-6" both become "opus46".
func normalizeModelQuery(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			sb.WriteRune(r + ('a' - 'A'))
		}
	}
	return sb.String()
}

// modelPickerLine is one display line of the list: a provider header
// (model < 0) or the model view[model].
type modelPickerLine struct {
	header string
	count  int
	model  int
}

func (d *modelDialog) lines() []modelPickerLine {
	out := make([]modelPickerLine, 0, len(d.view)+len(d.tabs))
	grouped := d.grouped()
	for i, m := range d.view {
		if grouped && (i == 0 || d.view[i-1].Provider != m.Provider) {
			count := 0
			for _, n := range d.view[i:] {
				if n.Provider != m.Provider {
					break
				}
				count++
			}
			out = append(out, modelPickerLine{header: m.Provider, count: count, model: -1})
		}
		out = append(out, modelPickerLine{model: i})
	}
	return out
}

// Render draws the picker in at most height body rows below the frame
// header: tabs, query, reasoning (when routed), the list, the highlighted
// model's details and the key help.
func (d *modelDialog) Render(th tui.Theme, width, height int) []string {
	if !d.Active() || width < 4 {
		return nil
	}
	muted := func(s string) string { return th.FGColor(th.Muted, s) }
	names := make([]string, len(d.tabs))
	for i, p := range d.tabs {
		names[i] = p
		if p == "" {
			names[i] = modelPickerAllTab
		}
	}
	status := fmt.Sprintf("%d models", len(d.view))
	if len(d.view) == 1 {
		status = "1 model"
	}
	lines := d.lines()
	// Chrome around the list: the spacer under the header always stays
	// (the pane frame would add it back), the footer and then the detail
	// (with its rule) go when the pane is too short for a few list rows.
	top := 4 // spacer, tabs, query, rule
	if d.showReasoning {
		top++
	}
	detail, footer := true, true
	rows := len(lines)
	if height > 0 {
		want := min(len(lines), modelPickerMinRows)
		if height-top-3 < want {
			footer = false
		}
		if height-top-2 < want {
			detail = false
		}
		chrome := top
		if footer {
			chrome++
		}
		if detail {
			chrome += 2
		}
		rows = min(rows, height-chrome)
	}
	rows = max(rows, 1)
	d.listRows = rows

	out := []string{frameHeader(th, "model", width), strings.Repeat(" ", width)}
	out = append(out, renderPickerTabs(th, names, d.tab, status, th.Muted, width))
	d.queryRow = len(out)
	out = append(out, d.query.Render(th, width))
	if d.showReasoning {
		out = append(out, d.renderReasoning(th, width))
	}
	out = append(out, muted(strings.Repeat("─", width)))

	cursorLine := 0
	for n, l := range lines {
		if l.model == d.cursor {
			cursorLine = n
			break
		}
	}
	d.viewTop = clampViewTop(d.viewTop, cursorLine, rows, len(lines))
	// Keep the header of the cursor's group on screen when it fits.
	if rows > 1 && d.viewTop == cursorLine && cursorLine > 0 && lines[cursorLine-1].model < 0 {
		d.viewTop--
	}
	out = append(out, d.renderList(th, lines, width, rows)...)
	if detail {
		out = append(out, muted(strings.Repeat("─", width)))
		out = append(out, d.renderDetail(th, width))
	}
	if footer {
		out = append(out, muted(pickerFit(" "+d.footerHelp(), width)))
	}
	return out
}

func (d *modelDialog) footerHelp() string {
	help := modelPickerFooterHelp
	if d.showReasoning {
		help = strings.Replace(help, "enter select", "←→ reasoning · enter select", 1)
	}
	return help
}

func (d *modelDialog) renderReasoning(th tui.Theme, width int) string {
	reasoning, adjustable := d.cursorReasoning()
	if reasoning == "" {
		reasoning = "off"
	}
	help := "  unavailable for highlighted model"
	if adjustable {
		help = "  ←/→ or H/L to change"
	}
	prefix := " ✦ reasoning: "
	used := runewidth.StringWidth(prefix + reasoning)
	if used > width {
		return th.FGColor(th.Muted, pickerFit(prefix+reasoning, width))
	}
	line := th.FGColor(th.Muted, prefix) + th.FGColor(reasoningLevelColor(th, reasoning), reasoning)
	return line + th.FGColor(th.Muted, pickerFit(help, width-used))
}

func (d *modelDialog) renderList(th tui.Theme, lines []modelPickerLine, width, rows int) []string {
	out := make([]string, 0, rows)
	if len(lines) == 0 {
		msg := " no models match " + strconv.Quote(d.query.String())
		if len(d.all) == 0 {
			msg = " no credentials found - run /login to add an api key or subscription"
		}
		out = append(out, th.FGColor(th.Muted, pickerFit(msg, width)))
	}
	end := min(len(lines), d.viewTop+rows)
	visible := make([]pickerRow, 0, end-d.viewTop)
	for _, l := range lines[d.viewTop:end] {
		if l.model >= 0 {
			visible = append(visible, d.row(d.view[l.model]))
		}
	}
	cols := pickerListColumns(visible)
	next := 0 // index into visible
	for _, l := range lines[d.viewTop:end] {
		if l.model < 0 {
			label := pickerFit(" "+provider.ProviderLabel(l.header), width)
			label = strings.TrimRight(label, " ")
			count := fmt.Sprintf(" · %d", l.count)
			out = append(out, th.FGColor(th.Accent, label)+th.FGColor(th.Muted, pickerFit(count, width-runewidth.StringWidth(label))))
			continue
		}
		out = append(out, renderPickerRow(th, visible[next], l.model == d.cursor, cols, width))
		next++
	}
	for len(out) < rows {
		out = append(out, strings.Repeat(" ", width))
	}
	return out
}

// row is the list row of m, with the provider as group when the list
// mixes providers without headers.
func (d *modelDialog) row(m provider.PickerModel) pickerRow {
	return modelPickerRow(m, d.tab == 0 && !d.grouped() && len(d.tabs) > 2, d.isCurrent(m))
}

// modelPickerRow is the list row of a model: its name (prefixed by
// "provider · " when withProvider), a note for speculative, snapshot and
// live entries, its id, and ● when current.
func modelPickerRow(m provider.PickerModel, withProvider, current bool) pickerRow {
	name := m.DisplayName
	if name == "" {
		name = m.ID
	}
	if withProvider {
		name = m.Provider + " · " + name
	}
	row := pickerRow{Label: name, Note: modelNote(m), Detail: m.ID}
	if current {
		row.Mark = "●"
	}
	return row
}

func modelNote(m provider.PickerModel) string {
	switch {
	case m.Speculative:
		return "speculative"
	case m.SnapshotOf != "":
		return "snapshot"
	case m.Source == "live":
		return "live"
	}
	return ""
}

// renderDetail renders the highlighted model's metadata, e.g.
// "1M ctx · 128k out · $5 in / $25 out per 1M · cache $0.5 · reasoning low→max".
func (d *modelDialog) renderDetail(th tui.Theme, width int) string {
	if d.cursor < 0 || d.cursor >= len(d.view) {
		return strings.Repeat(" ", width)
	}
	return th.FGColor(th.FG, pickerFit(" "+modelDetail(d.view[d.cursor]), width))
}

func modelDetail(m provider.PickerModel) string {
	var parts []string
	if m.ContextWindow > 0 {
		parts = append(parts, modelTokens(m.ContextWindow)+" ctx")
	}
	if m.MaxOutput > 0 {
		parts = append(parts, modelTokens(m.MaxOutput)+" out")
	}
	if m.PriceInput > 0 || m.PriceOutput > 0 {
		parts = append(parts, "$"+modelPrice(m.PriceInput)+" in / $"+modelPrice(m.PriceOutput)+" out per 1M")
		if m.PriceCacheRead > 0 {
			parts = append(parts, "cache $"+modelPrice(m.PriceCacheRead))
		}
	}
	levels := provider.AvailableReasoningLevels(m.Model)
	if len(levels) > 1 {
		first, last := levels[0], levels[len(levels)-1]
		if first == "" {
			first = "off"
		}
		parts = append(parts, "reasoning "+first+"→"+last)
	} else {
		parts = append(parts, "no reasoning")
	}
	switch {
	case m.Speculative:
		parts = append(parts, "not live on the API yet")
	case m.SnapshotOf != "":
		parts = append(parts, "snapshot of "+m.SnapshotOf)
	}
	return strings.Join(parts, " · ")
}

// modelTokens renders a token count as "200k", "128k", "1M" or "1.5M":
// decimal round numbers first (128000 -> 128k), then binary ones
// (131072 -> 128k, 1048576 -> 1M).
func modelTokens(n int) string {
	switch {
	case n >= 1_000_000 && n%1_000_000 == 0:
		return strconv.Itoa(n/1_000_000) + "M"
	case n >= 1_000_000 && n%(1<<20) == 0:
		return strconv.Itoa(n>>20) + "M"
	case n >= 1_000_000:
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64), ".0") + "M"
	case n >= 1000 && n%1000 == 0:
		return strconv.Itoa(n/1000) + "k"
	case n >= 1000 && n%1024 == 0:
		return strconv.Itoa(n/1024) + "k"
	case n >= 1000:
		return strconv.Itoa((n+500)/1000) + "k"
	}
	return strconv.Itoa(n)
}

func modelPrice(p float64) string { return strconv.FormatFloat(p, 'f', -1, 64) }

// reasoningLevelColor makes the active reasoning depth legible at a glance:
// low levels use the theme accent, medium/high use the warning color, and the
// most expensive levels use the explicit thinking-max color.
func reasoningLevelColor(th tui.Theme, level string) tui.TerminalColor {
	switch provider.NormalizeReasoning(level) {
	case "minimum", "low":
		return th.Accent
	case "medium", "high":
		return th.Warning
	case "xhigh", "max":
		return th.ThinkingMax
	default:
		return th.Muted
	}
}

// cursorReasoning returns the effective level for the model under the cursor
// and whether that model allows it to be adjusted.
func (d *modelDialog) cursorReasoning() (string, bool) {
	if len(d.view) == 0 || d.cursor < 0 || d.cursor >= len(d.view) {
		return d.reasoning, false
	}
	model := d.view[d.cursor].Model
	levels := provider.AvailableReasoningLevels(model)
	return provider.ClampReasoningForModel(model, d.reasoning), len(levels) > 1
}

// stepReasoning moves through the levels supported by the model under the
// cursor. It stops at each end rather than wrapping, matching arrow-key
// expectations and preventing an accidental jump from max to off.
func (d *modelDialog) stepReasoning(direction int) modelDialogAction {
	if len(d.view) == 0 || d.cursor < 0 || d.cursor >= len(d.view) {
		return modelDialogAction{}
	}
	model := d.view[d.cursor].Model
	levels := provider.AvailableReasoningLevels(model)
	current := provider.ClampReasoningForModel(model, d.reasoning)
	idx := 0
	for i, level := range levels {
		if level == current {
			idx = i
			break
		}
	}
	next := idx + direction
	if next < 0 || next >= len(levels) {
		return modelDialogAction{}
	}
	d.reasoning = levels[next]
	return modelDialogAction{ReasoningChanged: true, Reasoning: d.reasoning}
}

// CursorPos returns the row/col of the query caret in the lines Render
// returned, or -1, -1 when the dialog is closed.
func (d *modelDialog) CursorPos() (row, col int) {
	if !d.Active() {
		return -1, -1
	}
	return d.queryRow, d.query.Caret()
}

func (d *modelDialog) switchTab(delta int) {
	if len(d.tabs) < 2 {
		return
	}
	d.tab = (d.tab + delta + len(d.tabs)) % len(d.tabs)
	d.refilter()
}

func (d *modelDialog) moveCursor(delta int) {
	if len(d.view) > 0 {
		d.cursor = min(max(d.cursor+delta, 0), len(d.view)-1)
	}
}

// HandleKey advances the dialog and returns an action to apply, if any.
func (d *modelDialog) HandleKey(k tui.Key) modelDialogAction {
	page := max(d.listRows-1, 1)
	switch k.Kind {
	case tui.KeyLeft, tui.KeyRight:
		if d.showReasoning {
			if k.Kind == tui.KeyLeft {
				return d.stepReasoning(-1)
			}
			return d.stepReasoning(1)
		}
	case tui.KeyUp:
		d.moveCursor(-1)
		return modelDialogAction{}
	case tui.KeyDown:
		d.moveCursor(1)
		return modelDialogAction{}
	case tui.KeyPageUp:
		d.moveCursor(-page)
		return modelDialogAction{}
	case tui.KeyPageDown:
		d.moveCursor(page)
		return modelDialogAction{}
	case tui.KeyTab:
		d.switchTab(1)
		return modelDialogAction{}
	case tui.KeyShiftTab:
		d.switchTab(-1)
		return modelDialogAction{}
	case tui.KeyRune:
		if k.Alt || k.Ctrl {
			return modelDialogAction{}
		}
		if d.showReasoning {
			switch k.Rune {
			case 'H':
				return d.stepReasoning(-1)
			case 'L':
				return d.stepReasoning(1)
			}
		}
	case tui.KeyPaste:
		if d.query.InsertLine(k.Paste) {
			d.refilter()
		}
		return modelDialogAction{}
	case tui.KeyEsc:
		d.Close()
		return modelDialogAction{Close: true}
	case tui.KeyEnter:
		if len(d.view) == 0 {
			d.Close()
			return modelDialogAction{Close: true}
		}
		m := d.view[d.cursor]
		d.Close()
		return modelDialogAction{Select: true, Provider: m.Provider, Model: m.ID}
	}
	if d.query.Edit(k) {
		d.refilter()
	}
	return modelDialogAction{}
}
