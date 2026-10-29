package session

import (
	"context"
	"errors"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
)

type confirmState struct{}

func (s *confirmState) snapshotInto(out *Snapshot) {}

// AnswerConfirmation is reserved for the confirm cluster implementation.
func (c *Controller) AnswerConfirmation(ctx context.Context, reqID string, decision core.ConfirmDecision) error {
	return errors.New("unimplemented")
}

// AnswerQuestion is reserved for the confirm cluster implementation.
func (c *Controller) AnswerQuestion(ctx context.Context, reqID string, answers []tools.QuestionAnswer) error {
	return errors.New("unimplemented")
}

// ResolveDeferred is reserved for the confirm cluster implementation.
func (c *Controller) ResolveDeferred(ctx context.Context, id, action string) error {
	return errors.New("unimplemented")
}
func (c *Controller) confirmer() core.ToolCallConfirmer  { return nil }
func (c *Controller) questionAsker() tools.QuestionAsker { return nil }
func (c *Controller) denyAllPending(reason string)       {}
