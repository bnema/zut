package tools

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/bnema/zut/packages/provider"
	"strings"
	"testing"
)

type questionAskerFunc func(context.Context, []Question) ([]QuestionAnswer, error)

func (f questionAskerFunc) AskQuestions(c context.Context, q []Question) ([]QuestionAnswer, error) {
	return f(c, q)
}
func TestAskUserQuestionValidation(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"questions":[]}`, `{"questions":[{}]}`, `{"questions":[{"question":"q","bogus":true}]}`, `{"questions":[{"question":"q","options":[{}]}]}`, `{"questions":[{"question":"q","header":"12345678901234567"}]}`, `{"questions":[{"question":"q","options":[{"label":"a"},{"label":"a"}]}]}`, `{"questions":[{"question":"q"}]} {}`} {
		tool := &AskUserQuestionTool{Asker: func() QuestionAsker { t.Fatal("invalid input reached host"); return nil }}
		r, e := tool.Execute(context.Background(), json.RawMessage(raw), nil)
		if e != nil || !r.IsError {
			t.Fatalf("%s: %+v %v", raw, r, e)
		}
	}
}
func TestAskUserQuestionResults(t *testing.T) {
	raw := json.RawMessage(`{"questions":[{"question":"Which?","options":[{"label":"A"},{"label":"B"}],"multi_select":true},{"question":"Why?"}]}`)
	tool := &AskUserQuestionTool{}
	r, e := tool.Execute(context.Background(), raw, nil)
	if e != nil || !r.IsError || !strings.Contains(r.Content[0].(provider.TextBlock).Text, "interactive terminal") {
		t.Fatalf("headless: %+v %v", r, e)
	}
	tool.Asker = func() QuestionAsker {
		return questionAskerFunc(func(context.Context, []Question) ([]QuestionAnswer, error) {
			return []QuestionAnswer{{Selected: []string{"A", "B"}}, {Custom: "Because"}}, nil
		})
	}
	r, e = tool.Execute(context.Background(), raw, nil)
	if e != nil || r.IsError || r.Content[0].(provider.TextBlock).Text != "Q: Which?\nA: A, B\n\nQ: Why?\nA: Because (custom)" {
		t.Fatalf("format: %+v %v", r, e)
	}
	tool.Asker = func() QuestionAsker {
		return questionAskerFunc(func(context.Context, []Question) ([]QuestionAnswer, error) { return nil, ErrQuestionsDeclined })
	}
	r, e = tool.Execute(context.Background(), raw, nil)
	if e != nil || r.IsError || !strings.Contains(r.Content[0].(provider.TextBlock).Text, "declined") {
		t.Fatalf("decline: %+v %v", r, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tool.Asker = func() QuestionAsker {
		return questionAskerFunc(func(c context.Context, _ []Question) ([]QuestionAnswer, error) {
			cancel()
			<-c.Done()
			return nil, c.Err()
		})
	}
	_, e = tool.Execute(ctx, raw, nil)
	if !errors.Is(e, context.Canceled) {
		t.Fatalf("cancel: %v", e)
	}
}
