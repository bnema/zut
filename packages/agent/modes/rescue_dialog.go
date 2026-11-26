package modes

import (
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// rescueDialog offers a quick model swap when the active turn fails
// with a recoverable provider error (auth/rate/temporary). It looks
// and behaves like modelDialog: vertical list of available models,
// up/down to select, enter to retry, esc to cancel. The candidate
// list is built dynamically from the providers the user is currently
// logged in to — no static fallback config.
type rescueDialog struct {
	active   bool
	all      []provider.PickerModel // candidates, picker order
	texts    []pickerModelText      // search text of all, same index
	view     []provider.PickerModel // filtered view (typed query)
	cursor   int
	viewTop  int
	queryRow int    // row of the query line in the last Render
	listRows int    // list height of the last Render
	current  string // currently active model id (excluded from view)
	query    pickerQuery
	failedAt string // failed provider/model pair, e.g. "kimi/kimi-for-coding"
	reason   string // complete diagnostic, including provider response
	prompt   string // the user prompt that should be retried on Select
	details  bool
	scroll   int
	// Port mode: pendingID identifies the session request and onAnswer
	// receives "provider/model" (or nil when cancelled).
	pendingID string
	onAnswer  func([]tools.QuestionAnswer)
}

type rescueDialogAction struct {
	Select   bool
	Provider string
	Model    string
	Prompt   string
	Close    bool
}

func newRescueDialog() *rescueDialog { return &rescueDialog{} }

// Open shows the dialog. current is the currently active model id so
// it can be excluded from the candidate list. loggedInProviders is
// the set of provider names with usable credentials right now.
func (d *rescueDialog) Open(current string, loggedInProviders []string, failedProvider, failedModel, reason, prompt string) {
	d.active = true
	d.current = current
	d.query.Reset()
	d.details = false
	d.scroll = 0
	d.reason = reason
	d.prompt = prompt
	d.failedAt = strings.TrimSpace(failedProvider + "/" + failedModel)

	all := provider.Active()
	provSet := map[string]bool{}
	for _, p := range loggedInProviders {
		provSet[p] = true
	}
	var filtered []provider.Model
	for _, m := range all {
		if !provSet[m.Provider] {
			continue
		}
		// Drop the exact failed pair so users can't retry on the
		// model that just failed.
		if m.Provider == failedProvider && m.ID == failedModel {
			continue
		}
		// Drop the currently-active model id (if it differs from
		// the failed one for some reason). The picker is meant to
		// help the user move *off* the broken pair.
		if m.ID == current {
			continue
		}
		filtered = append(filtered, m)
	}
	d.setCandidates(provider.PickerModels(filtered))
}

// OpenPending shows a Port rescue request using the session's candidates.
func (d *rescueDialog) OpenPending(id string, opts session.RescueOptions, onAnswer func([]tools.QuestionAnswer)) {
	d.Close()
	d.active = true
	d.current, d.prompt, d.failedAt = "", "", ""
	d.query.Reset()
	d.details, d.scroll = false, 0
	d.reason = opts.Error
	d.pendingID, d.onAnswer = id, onAnswer
	var candidates []provider.PickerModel
	lookup := provider.ModelLookup()
	for _, c := range opts.Candidates {
		p, id, ok := strings.Cut(c, "/")
		if !ok || p == "" || id == "" {
			continue
		}
		// Catalog metadata feeds the row name and the detail line; the
		// session's candidate order is kept.
		m, found := lookup(p, id)
		if !found {
			m = provider.Model{Provider: p, ID: id}
		}
		candidates = append(candidates, provider.PickerModel{Model: m})
	}
	d.setCandidates(candidates)
}

func (d *rescueDialog) setCandidates(models []provider.PickerModel) {
	d.all = models
	d.texts = pickerModelTexts(models)
	d.refilter()
}

// ClosePending closes the dialog without answering when it shows id.
func (d *rescueDialog) ClosePending(id string) {
	if d.active && d.pendingID == id {
		d.pendingID, d.onAnswer = "", nil
		d.active = false
	}
}

// Close dismisses the dialog; a Port request is answered as declined.
func (d *rescueDialog) Close() {
	d.answer(nil)
	d.active = false
}

func (d *rescueDialog) answer(answers []tools.QuestionAnswer) {
	if fn := d.onAnswer; fn != nil {
		d.pendingID, d.onAnswer = "", nil
		fn(answers)
	}
}

func (d *rescueDialog) Active() bool { return d != nil && d.active }

func (d *rescueDialog) refilter() {
	d.view = rankPickerModels(d.all, d.texts, d.query.String(), nil)
	d.cursor, d.viewTop = 0, 0
}

// CursorPos returns the row/col of the query caret in the lines Render
// returned, or -1, -1 in the error view, which has no query line.
func (d *rescueDialog) CursorPos() (row, col int) {
	if !d.Active() || d.details {
		return -1, -1
	}
	return d.queryRow, d.query.Caret()
}

// Render mirrors modelDialog so a rescue prompt feels identical to
// every other picker in the TUI.
func (d *rescueDialog) Render(th tui.Theme, width, height int) []string {
	if !d.Active() {
		return nil
	}
	var lines []string
	header := "rescue turn"
	if d.failedAt != "" && d.failedAt != "/" {
		header = "rescue turn — " + d.failedAt + " failed"
	}
	if d.details {
		header = "provider error"
	}
	lines = append(lines, frameHeader(th, header, width))

	// The picker keeps a bounded preview; the details view retains the entire
	// diagnostic and owns its viewport rather than letting the pane clip it.
	errorRows := wrapDialogTextRows(sanitizeSessionTreeText(d.reason), width)
	if d.details {
		hint := wrapDialogTextRows("↑/↓ or pgup/pgdn: scroll; tab/esc: back", width)
		hint = hint[:min(len(hint), max(0, height-1))]
		visible := max(1, height-len(hint))
		d.scroll = min(max(0, d.scroll), max(0, len(errorRows)-visible))
		for _, row := range hint {
			lines = append(lines, th.FGColor(th.Muted, row))
		}
		for _, row := range errorRows[d.scroll:min(len(errorRows), d.scroll+visible)] {
			lines = append(lines, th.FGColor(th.Warning, row))
		}
		return append(lines, frameRule(th, width))
	}
	// The spacer keeps the pane frame from inserting one (which would
	// shift the caret row). The list gets up to rescueListRows rows; on a
	// short pane the footer, then the detail (with its rule), then error
	// preview rows give way first.
	lines = append(lines, strings.Repeat(" ", width))
	errLines := 0
	if d.reason != "" {
		errLines = min(3, len(errorRows))
	}
	footer, detail := true, true
	rows := min(rescueListRows, max(len(d.view), 1))
	if height > 0 {
		want := min(rows, modelPickerMinRows)
		chrome := func() int {
			n := 1 + errLines + 3 // spacer, error, hint, query, rule
			if detail {
				n += 2
			}
			if footer {
				n++
			}
			return n
		}
	shrink:
		for height-chrome() < want {
			switch {
			case footer:
				footer = false
			case detail:
				detail = false
			case errLines > 0:
				errLines--
			default:
				break shrink // nothing left to drop: one list row
			}
		}
		rows = min(rows, max(height-chrome(), 1))
	}
	d.listRows = rows
	for _, row := range errorRows[:errLines] {
		lines = append(lines, th.FGColor(th.Warning, row))
	}

	hint := "retry this turn with another model - type to filter"
	lines = append(lines, th.FGColor(th.Muted, pickerFit(" "+hint, width)))
	d.queryRow = len(lines)
	lines = append(lines, d.query.Render(th, width))
	lines = append(lines, frameRule(th, width))

	if len(d.view) == 0 {
		msg := " no models match " + fmt.Sprintf("%q", d.query.String())
		if len(d.all) == 0 {
			msg = " no other models available — log in to another provider with /login"
		}
		lines = append(lines, th.FGColor(th.Muted, pickerFit(msg, width)))
	} else {
		d.viewTop = clampViewTop(d.viewTop, d.cursor, rows, len(d.view))
		end := min(len(d.view), d.viewTop+rows)
		pickerRows := make([]pickerRow, 0, end-d.viewTop)
		for _, m := range d.view[d.viewTop:end] {
			pickerRows = append(pickerRows, modelPickerRow(m, true, false))
		}
		cols := pickerListColumns(pickerRows)
		for n, row := range pickerRows {
			lines = append(lines, renderPickerRow(th, row, d.viewTop+n == d.cursor, cols, width))
		}
	}
	if detail {
		lines = append(lines, frameRule(th, width))
		text := ""
		if d.cursor < len(d.view) {
			text = " " + modelDetail(d.view[d.cursor])
		}
		lines = append(lines, th.FGColor(th.FG, pickerFit(text, width)))
	}
	if footer {
		help := " ↑↓ pick · enter retry · tab full error · esc cancel"
		if len(d.view) == 0 {
			help = " tab full error · esc cancel"
		}
		lines = append(lines, th.FGColor(th.Muted, pickerFit(help, width)))
	}
	return lines
}

// rescueListRows caps the rescue list height.
const rescueListRows = 12

func (d *rescueDialog) HandleKey(k tui.Key) rescueDialogAction {
	if k.Kind == tui.KeyTab {
		d.details = !d.details
		d.scroll = 0
		return rescueDialogAction{}
	}
	if d.details {
		switch k.Kind {
		case tui.KeyEsc:
			d.details = false
		case tui.KeyUp, tui.KeyMouseWheelUp:
			d.scroll = max(0, d.scroll-1)
		case tui.KeyDown, tui.KeyMouseWheelDown:
			d.scroll++
		case tui.KeyPageUp:
			d.scroll = max(0, d.scroll-8)
		case tui.KeyPageDown:
			d.scroll += 8
		case tui.KeyHome:
			d.scroll = 0
		}
		return rescueDialogAction{}
	}
	switch k.Kind {
	case tui.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
	case tui.KeyDown:
		if d.cursor < len(d.view)-1 {
			d.cursor++
		}
	case tui.KeyPaste:
		if d.query.InsertLine(k.Paste) {
			d.refilter()
		}
	case tui.KeyEsc:
		d.Close()
		return rescueDialogAction{Close: true}
	case tui.KeyEnter:
		if len(d.view) == 0 {
			d.Close()
			return rescueDialogAction{Close: true}
		}
		m := d.view[d.cursor]
		if d.onAnswer != nil {
			d.answer([]tools.QuestionAnswer{{Selected: []string{m.Provider + "/" + m.ID}}})
			d.active = false
			return rescueDialogAction{Close: true}
		}
		prompt := d.prompt
		d.Close()
		return rescueDialogAction{Select: true, Provider: m.Provider, Model: m.ID, Prompt: prompt}
	default:
		if !k.Alt && d.query.Edit(k) {
			d.refilter()
		}
	}
	return rescueDialogAction{}
}

// classifyRescueError inspects an agent error and decides whether
// it's a recoverable provider failure that the user should be offered
// a rescue picker for, plus a category and the complete diagnostic. Returns
// (false, "") for errors we should NOT auto-rescue (bad request,
// context length, transcript serialization issues, etc.) so the
// regular red status banner still surfaces them.
func classifyRescueError(err error) (bool, string) {
	if err == nil {
		return false, ""
	}
	msg := err.Error()
	low := strings.ToLower(msg)

	// Don't trigger on context overflow; that path already has its own
	// auto-compact handling.
	if provider.IsContextOverflowError(err) {
		return false, ""
	}

	// Network failures.
	if strings.Contains(low, "timeout") ||
		strings.Contains(low, "deadline exceeded") ||
		strings.Contains(low, "connection refused") ||
		strings.Contains(low, "connection reset") ||
		strings.Contains(low, "no such host") ||
		strings.Contains(low, "tls handshake") ||
		strings.Contains(low, "eof") {
		return true, "network failure: " + msg
	}

	switch {
	case containsAny(low, "http 401", " 401:", "invalid_authentication", "token expired", "api key appears to be invalid"):
		return true, "authentication failed: " + msg
	case containsAny(low, "http 403", " 403:", "permission denied", "forbidden"):
		return true, "permission denied: " + msg
	case containsAny(low, "http 429", " 429:", "rate limit", "rate_limit", "too many requests", "quota"):
		return true, "rate limited: " + msg
	case containsAny(low, "http 500", "http 502", "http 503", "http 504", " 500:", " 502:", " 503:", " 504:", "upstream connect error", "service unavailable", "internal server error", "bad gateway", "gateway timeout"):
		return true, "provider unavailable: " + msg
	}

	// Anything else (400 bad request, validation errors, etc.) is
	// usually not fixed by switching models; let it surface as-is.
	return false, ""
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
