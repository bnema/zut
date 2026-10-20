package modes

import (
	"context"
	"errors"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/tui"
	"github.com/mattn/go-runewidth"
	"strings"
	"testing"
)

func TestQuestionDialogChain(t *testing.T) {
	d := newQuestionDialog()
	opts := []tools.QuestionOption{{Label: "A"}, {Label: "B"}}
	r := d.Enqueue([]tools.Question{{Question: "Which?", Options: opts}, {Question: "Many?", Options: opts, MultiSelect: true}, {Question: "Explain?"}})
	key := func(k tui.KeyKind) { d.HandleKey(tui.Key{Kind: k}) }
	runeKey := func(c rune) { d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: c}) }
	if !strings.Contains(strings.Join(d.Render(tui.Theme{}, 80), "\n"), "Question 1/3") {
		t.Fatal("missing counter")
	}
	runeKey('2')
	if r.index != 1 || r.states[0].answer.Selected[0] != "B" {
		t.Fatal("single pick")
	}
	key(tui.KeyEnter)
	if r.hint == "" || r.index != 1 {
		t.Fatal("empty multi accepted")
	}
	runeKey(' ')
	key(tui.KeyDown)
	runeKey(' ')
	key(tui.KeyEnter)
	if r.index != 2 || len(r.states[1].answer.Selected) != 2 {
		t.Fatal("multi pick")
	}
	runeKey('界')
	key(tui.KeyShiftTab)
	if !r.states[1].checked[0] || !r.states[1].checked[1] {
		t.Fatal("checks lost")
	}
	key(tui.KeyLeft)
	if r.states[0].cursor != 1 {
		t.Fatal("cursor lost")
	}
	key(tui.KeyTab)
	key(tui.KeyTab)
	if r.states[2].text != "界" {
		t.Fatal("text lost")
	}
	key(tui.KeyEnter)
	response := <-r.resp
	if response.err != nil || response.answers[2].Custom != "界" || d.Active() {
		t.Fatalf("finish %+v", response)
	}
}
func TestQuestionDialogOtherAndCancel(t *testing.T) {
	d := newQuestionDialog()
	r := d.Enqueue([]tools.Question{{Question: "Q", Options: []tools.QuestionOption{{Label: "A"}}}})
	d.HandleKey(tui.Key{Kind: tui.KeyDown})
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: 'x'})
	d.HandleKey(tui.Key{Kind: tui.KeyEsc})
	if r.states[0].editing || r.states[0].text != "x" {
		t.Fatal("other escape lost text")
	}
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if (<-r.resp).answers[0].Custom != "x" {
		t.Fatal("other answer")
	}
	r = d.Enqueue([]tools.Question{{Question: "free"}})
	d.HandleKey(tui.Key{Kind: tui.KeyEsc})
	if (<-r.resp).err != tools.ErrQuestionsDeclined {
		t.Fatal("free cancel")
	}
	r = d.Enqueue([]tools.Question{{Question: "Q", Options: []tools.QuestionOption{{Label: "A"}}}})
	d.HandleKey(tui.Key{Kind: tui.KeyEsc})
	if (<-r.resp).err != tools.ErrQuestionsDeclined {
		t.Fatal("cancel")
	}
}
func TestQuestionDialogNarrowAndQueue(t *testing.T) {
	d := newQuestionDialog()
	r := d.Enqueue([]tools.Question{{Header: "Tag", Question: "Unicode 界界界 and a long question", Options: []tools.QuestionOption{{Label: "界 option", Description: "long description"}}}})
	other := d.Enqueue([]tools.Question{{Question: "second"}})
	for _, width := range []int{2, 8, 16, 30} {
		for _, line := range d.Render(tui.Theme{}, width) {
			if w := runewidth.StringWidth(stripANSIBytes(line)); w > width {
				t.Fatalf("width %d: %d %q", width, w, line)
			}
		}
	}
	d.Remove(other)
	if !d.Active() {
		t.Fatal("removed active by mistake")
	}
	d.Remove(r)
	if d.Active() {
		t.Fatal("request not removed")
	}
}

func TestAskQuestionsContextCancellationDismisses(t *testing.T) {
	i := &Interactive{questionDialog: newQuestionDialog(), dirty: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := i.AskQuestions(ctx, []tools.Question{{Question: "Q"}}); done <- err }()
	<-i.dirty // enqueue and alert completed, without a timer or polling
	if !i.questionDialog.Active() {
		t.Fatal("not enqueued")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if i.questionDialog.Active() {
		t.Fatal("cancel left dialog active")
	}
}

func TestQuestionInputFocusAfterConfirmation(t *testing.T) {
	i := newConfirmationInputInteractive()
	r := i.questionDialog.Enqueue([]tools.Question{{Question: "Q", Options: []tools.QuestionOption{{Label: "A"}}}})
	confirm := enqueueConfirmation(i)
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	if !(<-confirm).Allow || !i.questionDialog.Active() {
		t.Fatal("confirmation did not own focus")
	}
	select {
	case <-r.resp:
		t.Fatal("question answered during confirmation")
	default:
	}
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	if (<-r.resp).answers[0].Selected[0] != "A" {
		t.Fatal("question did not regain focus")
	}
	if !i.ed.IsEmpty() {
		t.Fatal("question input leaked into editor")
	}
}

func TestQuestionDialogSpacingInFloatingPane(t *testing.T) {
	d := newQuestionDialog()
	d.Enqueue([]tools.Question{{Header: "H", Question: "Which?", Options: []tools.QuestionOption{{Label: "A"}}}})
	_, body, _ := floatingDialogBody(d.Render(tui.Theme{}, 200))
	plain := make([]string, len(body))
	for n, line := range body {
		plain[n] = strings.TrimSpace(stripANSIBytes(line))
	}
	if plain[0] != "Which?" || plain[1] != "" || plain[len(plain)-2] != "" || !strings.Contains(plain[len(plain)-1], "Esc decline") {
		t.Fatalf("unexpected spacing: %q", plain)
	}
}

func TestQuestionDialogFocusRows(t *testing.T) {
	d := newQuestionDialog()
	d.Enqueue([]tools.Question{{Question: "A long question that wraps over several rows", Options: []tools.QuestionOption{{Label: "A long option that wraps"}, {Label: "Last"}}}})
	d.HandleKey(tui.Key{Kind: tui.KeyDown})
	assertFocus := func(want string) {
		t.Helper()
		_, body, _ := floatingDialogBody(d.Render(tui.Dark, 20))
		for _, row := range body {
			if strings.Contains(row, tui.Dark.SelectionStyle()) {
				if !strings.Contains(stripANSIBytes(row), want) {
					t.Fatalf("focus row = %q, want %q", row, want)
				}
				return
			}
		}
		t.Fatal("no detected focus row")
	}
	assertFocus("2. Last")
	d.HandleKey(tui.Key{Kind: tui.KeyDown})
	assertFocus("Other…")
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	d.HandleKey(tui.Key{Kind: tui.KeyPaste, Paste: "answer"})
	assertFocus("> answer▏")
}

func TestQuestionDialogChangedSelectionRequiresConfirmation(t *testing.T) {
	for _, toggle := range []rune{' ', '1'} {
		t.Run(string(toggle), func(t *testing.T) {
			d := newQuestionDialog()
			r := d.Enqueue([]tools.Question{{Question: "Pick", MultiSelect: true, Options: []tools.QuestionOption{{Label: "A"}, {Label: "B"}}}, {Question: "Next"}})
			key := func(k tui.KeyKind) { d.HandleKey(tui.Key{Kind: k}) }
			pick := func(c rune) { d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: c}) }
			pick(toggle)
			key(tui.KeyEnter)
			key(tui.KeyShiftTab)
			pick(toggle)
			key(tui.KeyDown)
			if toggle == '1' {
				pick('2')
			} else {
				pick(' ')
			}
			for _, k := range []tui.KeyKind{tui.KeyTab, tui.KeyRight} {
				key(k)
				if r.index != 0 {
					t.Fatal("advanced with stale answer")
				}
			}
			key(tui.KeyEnter)
			if r.index != 1 || strings.Join(r.states[0].answer.Selected, ",") != "B" {
				t.Fatalf("did not recommit B: %+v", r.states[0])
			}
		})
	}
}

func TestQuestionDialogEditedTextRequiresConfirmation(t *testing.T) {
	for _, edit := range []tui.Key{{Kind: tui.KeyBackspace}, {Kind: tui.KeyRune, Rune: 'x'}, {Kind: tui.KeyPaste, Paste: "new\ntext"}} {
		d := newQuestionDialog()
		r := d.Enqueue([]tools.Question{{Question: "Pick", Options: []tools.QuestionOption{{Label: "A"}}}, {Question: "Next"}})
		d.HandleKey(tui.Key{Kind: tui.KeyDown})
		d.HandleKey(tui.Key{Kind: tui.KeyEnter})
		d.HandleKey(tui.Key{Kind: tui.KeyPaste, Paste: "old"})
		d.HandleKey(tui.Key{Kind: tui.KeyEnter})
		d.HandleKey(tui.Key{Kind: tui.KeyShiftTab})
		d.HandleKey(edit)
		d.HandleKey(tui.Key{Kind: tui.KeyEsc})
		d.HandleKey(tui.Key{Kind: tui.KeyTab})
		if r.index != 0 || r.states[0].answered {
			t.Fatalf("stale text accepted for %+v", edit)
		}
		d.HandleKey(tui.Key{Kind: tui.KeyEnter})
		d.HandleKey(tui.Key{Kind: tui.KeyEnter})
		if r.index != 1 || r.states[0].answer.Custom != r.states[0].text {
			t.Fatal("text not recommitted")
		}
	}
}

func TestQuestionDialogMultilineRows(t *testing.T) {
	d := newQuestionDialog()
	r := d.Enqueue([]tools.Question{{Header: "H\nheader", Question: "Q\nquestion", Options: []tools.QuestionOption{{Label: "A\nlabel", Description: "D\ndescription"}}}})
	check := func() {
		t.Helper()
		for _, row := range d.Render(tui.Dark, 12) {
			if strings.Contains(row, "\n") {
				t.Fatalf("embedded newline: %q", row)
			}
		}
	}
	check()
	d.HandleKey(tui.Key{Kind: tui.KeyDown})
	d.HandleKey(tui.Key{Kind: tui.KeyEnter})
	d.HandleKey(tui.Key{Kind: tui.KeyPaste, Paste: "one\r\ntwo\nthree\rfour"})
	if r.states[0].text != "one two three four" {
		t.Fatalf("paste = %q", r.states[0].text)
	}
	d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: '\n'})
	check()
	// Rendering also tolerates multiline state independently of input normalization.
	r.states[0].text = "typed\ntext"
	check()
}

func TestQuestionDialogClipboardAndConfirmFocus(t *testing.T) {
	i := newConfirmationInputInteractive()
	r := i.questionDialog.Enqueue([]tools.Question{{Question: "Free text"}})
	if !i.confirmChildActive() {
		t.Fatal("question not recognized as clipboard child")
	}
	key, ok, err := resolveClipboardText(tui.Key{Kind: tui.KeyPasteClipboard}, func() (string, bool, error) { return "one\ntwo", true, nil })
	if err != nil || !ok || key.Kind != tui.KeyPaste {
		t.Fatalf("resolve: %+v %v", key, err)
	}
	i.handleKey(context.Background(), key)
	if r.states[0].text != "one two" || !i.ed.IsEmpty() {
		t.Fatal("paste not delivered to question input")
	}
	confirm := enqueueConfirmation(i)
	i.confirmDialog.Blur()
	i.restoreConfirmFocus()
	if i.confirmDialog.Focused() {
		t.Fatal("question stole focus restoration")
	}
	i.confirmDialog.Focus()
	i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	if !(<-confirm).Allow {
		t.Fatal("confirmation focus broken")
	}
}

func TestQuestionReadyAlertWhileBusy(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		term := &alertTestTerminal{}
		i := &Interactive{cfg: InteractiveConfig{Terminal: term, TerminalAlertsEnabled: &enabled}, busy: true, questionDialog: newQuestionDialog(), dirty: make(chan struct{}, 1)}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := i.AskQuestions(ctx, []tools.Question{{Question: "Q"}}); done <- err }()
		<-i.dirty // Alert completes before invalidation; no redraw or timer needed.
		want := ""
		if enabled {
			want = "\a"
		}
		if got := term.String(); got != want {
			t.Errorf("busy alert = %q, want %q", got, want)
		}
		if i.pendingAlert != nil {
			t.Error("question alert deferred")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
}
