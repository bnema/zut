package session

import (
	"context"
	"errors"
)

type interState struct{}

func (s *interState) snapshotInto(out *Snapshot) {}

// SessionsQuery is reserved for the intersession cluster implementation.
func (c *Controller) SessionsQuery(ctx context.Context) (Tree, error) {
	return Tree{}, errors.New("unimplemented")
}
func (c *Controller) deliverInterSession(from Sender, text string) error {
	return errors.New("unimplemented")
}
