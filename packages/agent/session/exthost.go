package session

import (
	"context"
	"errors"
	"github.com/bnema/zut/packages/agent/extproto"
)

type extState struct{}

func (s *extState) snapshotInto(out *Snapshot) {}

// ReloadExtensions is reserved for the exthost cluster implementation.
func (c *Controller) ReloadExtensions(ctx context.Context) error { return errors.New("unimplemented") }

// InvokeExtensionCommand is reserved for the exthost cluster implementation.
func (c *Controller) InvokeExtensionCommand(ctx context.Context, name, args string) error {
	return errors.New("unimplemented")
}

// ExtensionPanelInput is reserved for the exthost cluster implementation.
func (c *Controller) ExtensionPanelInput(ctx context.Context, ext, panelID string, key extproto.PanelKeyFromHost) error {
	return errors.New("unimplemented")
}
