package session

import (
	"context"
	"errors"
	"github.com/bnema/zut/packages/core"
)

type settingsState struct{}

func (s *settingsState) snapshotInto(out *Snapshot) {}

// SetModel is reserved for the settings cluster implementation.
func (c *Controller) SetModel(ctx context.Context, providerName, model string) error {
	return errors.New("unimplemented")
}

// SetReasoning is reserved for the settings cluster implementation.
func (c *Controller) SetReasoning(ctx context.Context, level string) error {
	return errors.New("unimplemented")
}

// SetFastMode is reserved for the settings cluster implementation.
func (c *Controller) SetFastMode(ctx context.Context, enabled bool) error {
	return errors.New("unimplemented")
}

// ApplyProfile is reserved for the settings cluster implementation.
func (c *Controller) ApplyProfile(ctx context.Context, n int) error {
	return errors.New("unimplemented")
}

// ApplySettings is reserved for the settings cluster implementation.
func (c *Controller) ApplySettings(ctx context.Context, patch SettingsPatch) error {
	return errors.New("unimplemented")
}

// ChangeCWD is reserved for the settings cluster implementation.
func (c *Controller) ChangeCWD(ctx context.Context, path string) error {
	return errors.New("unimplemented")
}

// SetJail is reserved for the settings cluster implementation.
func (c *Controller) SetJail(ctx context.Context, enabled bool) error {
	return errors.New("unimplemented")
}

// SetTitle is reserved for the settings cluster implementation.
func (c *Controller) SetTitle(ctx context.Context, title string) error {
	return errors.New("unimplemented")
}

// ToggleSkillPin is reserved for the settings cluster implementation.
func (c *Controller) ToggleSkillPin(ctx context.Context, name string, global bool) error {
	return errors.New("unimplemented")
}

// ReloadCredentials is reserved for the settings cluster implementation.
func (c *Controller) ReloadCredentials(ctx context.Context) error { return errors.New("unimplemented") }
func (c *Controller) replaceAgent(ag *core.Agent)                 {}
