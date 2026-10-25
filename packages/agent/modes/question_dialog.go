package modes

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/tui"
)

type questionResponse struct {
	answers []tools.QuestionAnswer
	err     error
}
type questionState struct {
	cursor            int
	checked           map[int]bool
	text              string
	editing, answered bool
	answer            tools.QuestionAnswer
}
type questionRequest struct {
	questions []tools.Question
	states    []questionState
	index     int
	hint      string
	resp      chan questionResponse
}
type questionDialog struct {
	mu      sync.Mutex
	pending []*questionRequest
}

func newQuestionDialog() *questionDialog { return &questionDialog{} }
func (d *questionDialog) Enqueue(qs []tools.Question) *questionRequest {
	r := &questionRequest{questions: qs, states: make([]questionState, len(qs)), resp: make(chan questionResponse, 1)}
	for n, q := range qs {
		r.states[n] = questionState{checked: map[int]bool{}, editing: len(q.Options) == 0}
	}
	d.mu.Lock()
	d.pending = append(d.pending, r)
	d.mu.Unlock()
	return r
}
func (d *questionDialog) Active() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pending) > 0
}
func (d *questionDialog) Remove(r *questionRequest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for n, p := range d.pending {
		if p == r {
			d.pending = append(d.pending[:n], d.pending[n+1:]...)
			return
		}
	}
}
func (d *questionDialog) HandleKey(k tui.Key) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.pending) == 0 {
		return
	}
	r := d.pending[0]
	s := &r.states[r.index]
	q := r.questions[r.index]
	finish := func(err error) {
		answers := make([]tools.QuestionAnswer, len(r.states))
		for n, st := range r.states {
			answers[n] = st.answer
		}
		d.pending = d.pending[1:]
		r.resp <- questionResponse{answers, err}
	}
	advance := func() {
		s.answered = true
		r.hint = ""
		if r.index == len(r.questions)-1 {
			finish(nil)
		} else {
			r.index++
		}
	}
	if k.Kind == tui.KeyCtrlC {
		finish(tools.ErrQuestionsDeclined)
		return
	}
	if k.Kind == tui.KeyShiftTab || (!s.editing && k.Kind == tui.KeyLeft) {
		if r.index > 0 {
			r.index--
			r.hint = ""
		}
		return
	}
	if !s.editing && (k.Kind == tui.KeyTab || k.Kind == tui.KeyRight) {
		if s.answered && r.index < len(r.questions)-1 {
			r.index++
		}
		return
	}
	if s.editing {
		switch k.Kind {
		case tui.KeyEsc:
			if len(q.Options) == 0 {
				finish(tools.ErrQuestionsDeclined)
			} else {
				s.editing = false
			}
		case tui.KeyEnter:
			if strings.TrimSpace(s.text) == "" {
				r.hint = "Enter an answer."
			} else {
				s.answer = tools.QuestionAnswer{Custom: strings.TrimSpace(s.text)}
				advance()
			}
		case tui.KeyBackspace:
			runes := []rune(s.text)
			if len(runes) > 0 {
				s.text = string(runes[:len(runes)-1])
				s.answered = false
			}
		case tui.KeyRune:
			s.text += strings.NewReplacer("\r", " ", "\n", " ").Replace(string(k.Rune))
			s.answered = false
		case tui.KeyPaste:
			s.text += strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(k.Paste)
			if k.Paste != "" {
				s.answered = false
			}
		}
		return
	}
	if k.Kind == tui.KeyEsc {
		finish(tools.ErrQuestionsDeclined)
		return
	}
	if k.Kind == tui.KeyUp || (k.Kind == tui.KeyRune && k.Rune == 'k') {
		s.cursor = max(0, s.cursor-1)
		return
	}
	if k.Kind == tui.KeyDown || (k.Kind == tui.KeyRune && k.Rune == 'j') {
		s.cursor = min(len(q.Options), s.cursor+1)
		return
	}
	if k.Kind == tui.KeyRune && k.Rune >= '1' && k.Rune <= '9' {
		n := int(k.Rune - '1')
		if n < len(q.Options) {
			s.cursor = n
			if q.MultiSelect {
				s.checked[n] = !s.checked[n]
				s.answered = false
				return
			}
			k.Kind = tui.KeyEnter
		}
	}
	if k.Kind == tui.KeyRune && k.Rune == ' ' && q.MultiSelect && s.cursor < len(q.Options) {
		s.checked[s.cursor] = !s.checked[s.cursor]
		s.answered = false
		return
	}
	if k.Kind != tui.KeyEnter {
		return
	}
	if s.cursor == len(q.Options) {
		s.editing = true
		return
	}
	a := tools.QuestionAnswer{}
	if q.MultiSelect {
		for n, o := range q.Options {
			if s.checked[n] {
				a.Selected = append(a.Selected, o.Label)
			}
		}
		if len(a.Selected) == 0 {
			r.hint = "Select at least one option (Space)."
			return
		}
	} else {
		a.Selected = []string{q.Options[s.cursor].Label}
	}
	s.answer = a
	advance()
}
func (d *questionDialog) Render(th tui.Theme, width int) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.pending) == 0 {
		return nil
	}
	width = max(1, width)
	r := d.pending[0]
	q := r.questions[r.index]
	s := r.states[r.index]
	title := fmt.Sprintf("Question %d/%d", r.index+1, len(r.questions))
	if q.Header != "" {
		title += " · " + q.Header
	}
	lines := []string{frameHeader(th, title, width)}
	// Like wrapDialogTextRows, split logical lines before wrapping, but keep
	// this dialog's unindented layout. Style physical rows after wrapping.
	addRows := func(text string, focused bool) {
		for _, line := range strings.Split(text, "\n") {
			for _, row := range tui.WrapANSILine(line, width) {
				if focused {
					row = th.PadHighlight(stripANSIBytes(row), width)
				}
				lines = append(lines, row)
			}
		}
	}
	add := func(text string) { addRows(text, false) }
	add(q.Question)
	lines = append(lines, "")
	footer := th.FGColor(th.Muted, "↑/↓ j/k move · 1–9 pick · Space toggle · Enter confirm · ←/Shift+Tab back · Tab next · Esc decline")
	if s.editing {
		addRows("> "+s.text+"▏", true)
		footer = th.FGColor(th.Muted, "Enter submit · Esc back/cancel · Shift+Tab previous")
	} else {
		for n := 0; n <= len(q.Options); n++ {
			label := "Other…"
			if n < len(q.Options) {
				label = fmt.Sprintf("%d. %s", n+1, q.Options[n].Label)
				if q.MultiSelect {
					mark := "[ ] "
					if s.checked[n] {
						mark = "[x] "
					}
					label = mark + label
				}
				if q.Options[n].Description != "" {
					label += " — " + th.FGColor(th.Muted, q.Options[n].Description)
				}
			}
			prefix := "  "
			if n == s.cursor {
				prefix = "› "
			}
			addRows(prefix+label, n == s.cursor)
		}
	}
	if r.hint != "" {
		add(th.FGColor(th.Muted, r.hint))
	}
	lines = append(lines, "")
	add(footer)
	lines = append(lines, frameRule(th, width))
	// Frame helpers may retain their minimum decoration width. Wrap these too.
	var out []string
	for _, line := range lines {
		for _, logical := range strings.Split(line, "\n") {
			out = append(out, tui.WrapANSILine(logical, width)...)
		}
	}
	return out
}

// AskQuestions blocks only the calling agent goroutine, never the UI loop.
func (i *Interactive) AskQuestions(ctx context.Context, qs []tools.Question) ([]tools.QuestionAnswer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := i.questionDialog.Enqueue(qs)
	i.alertNeedsInput(alertAwaitingAnswers)
	i.invalidate()
	defer func() { i.questionDialog.Remove(r); i.invalidate() }()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case response := <-r.resp:
		return response.answers, response.err
	}
}
