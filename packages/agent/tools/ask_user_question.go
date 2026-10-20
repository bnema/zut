package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const AskUserQuestionToolName = "ask_user_question"

type Question struct {
	Header      string           `json:"header"`
	Question    string           `json:"question"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multi_select"`
}
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}
type QuestionAnswer struct {
	Selected []string
	Custom   string
}

var ErrQuestionsDeclined = errors.New("user declined to answer")

type QuestionAsker interface {
	AskQuestions(context.Context, []Question) ([]QuestionAnswer, error)
}
type AskUserQuestionTool struct{ Asker func() QuestionAsker }

func (*AskUserQuestionTool) Name() string      { return AskUserQuestionToolName }
func (*AskUserQuestionTool) Interactive() bool { return true }
func (*AskUserQuestionTool) Description() string {
	return "Ask the user up to 10 structured questions in an interactive terminal. Offer up to 9 options per question, or omit options for free text. The user can always provide a custom answer or decline."
}
func (*AskUserQuestionTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","minItems":1,"maxItems":10,"items":{"type":"object","properties":{"header":{"type":"string","maxLength":16},"question":{"type":"string","minLength":1},"options":{"type":"array","maxItems":9,"items":{"type":"object","properties":{"label":{"type":"string","minLength":1},"description":{"type":"string"}},"required":["label"],"additionalProperties":false}},"multi_select":{"type":"boolean"}},"required":["question"],"additionalProperties":false}}},"required":["questions"],"additionalProperties":false}`)
}
func (t *AskUserQuestionTool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	fail := func(s string) (core.ToolResult, error) {
		return core.ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: s}}}, nil
	}
	// Null is not a valid value for any field in this schema. encoding/json
	// otherwise silently accepts null for strings and booleans.
	var shape any
	if err := json.Unmarshal(raw, &shape); err == nil && questionContainsNull(shape) {
		return fail("Invalid questions: null fields are not allowed")
	}
	var args struct {
		Questions []Question `json:"questions"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&args); err != nil {
		return fail("Invalid questions: " + err.Error())
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return fail("Invalid questions: expected one JSON object")
	}
	if len(args.Questions) < 1 || len(args.Questions) > 10 {
		return fail("questions must contain 1 to 10 questions")
	}
	for _, q := range args.Questions {
		if strings.TrimSpace(q.Question) == "" || utf8.RuneCountInString(q.Header) > 16 || len(q.Options) > 9 {
			return fail("Each question needs text, a header of at most 16 characters, and at most 9 options")
		}
		seen := map[string]bool{}
		for _, o := range q.Options {
			label := strings.TrimSpace(o.Label)
			if label == "" || seen[label] {
				return fail("Option labels must be non-empty and unique")
			}
			seen[label] = true
		}
	}
	if err := ctx.Err(); err != nil {
		return core.ToolResult{}, err
	}
	var asker QuestionAsker
	if t.Asker != nil {
		asker = t.Asker()
	}
	if asker == nil {
		return fail("ask_user_question needs an interactive terminal session")
	}
	answers, err := asker.AskQuestions(ctx, args.Questions)
	if ctx.Err() != nil {
		return core.ToolResult{}, ctx.Err()
	}
	if errors.Is(err, ErrQuestionsDeclined) {
		return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "The user declined to answer the questions."}}}, nil
	}
	if err != nil {
		return fail(err.Error())
	}
	if len(answers) != len(args.Questions) {
		return fail("Interactive host returned an incomplete answer set")
	}
	var b strings.Builder
	for n, q := range args.Questions {
		a := answers[n]
		text := strings.Join(a.Selected, ", ")
		if a.Custom != "" {
			text = a.Custom + " (custom)"
		}
		fmt.Fprintf(&b, "Q: %s\nA: %s\n\n", q.Question, text)
	}
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: strings.TrimSpace(b.String())}}}, nil
}

func questionContainsNull(v any) bool {
	if v == nil {
		return true
	}
	switch x := v.(type) {
	case map[string]any:
		for _, child := range x {
			if questionContainsNull(child) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if questionContainsNull(child) {
				return true
			}
		}
	}
	return false
}
