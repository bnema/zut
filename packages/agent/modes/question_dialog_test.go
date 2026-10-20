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
