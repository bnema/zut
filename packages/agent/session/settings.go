package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

type settingsState struct {
	title        string
	titleStarted bool
	replaceErr   error
}

func (s *settingsState) snapshotInto(out *Snapshot) { out.Info.Title = s.title }

// settingsCall serializes settings changes; no runtime reconfiguration races a turn.
func (c *Controller) settingsCall(ctx context.Context, fn func() error) error {
	var result error
	err := c.call(ctx, func() {
		if c.turnSeam.isBusy() || c.compact.busy {
			result = ErrBusy
			return
		}
		result = fn()
		if result == nil {
			c.publish(EvInfo, InfoEvent{Info: c.Info()})
		}
	})
	if err != nil {
		return err
	}
	return result
}

// replaceAgent preserves the transcript and usage before handing ownership to TURN.
// The actor-only seam cannot return errors, so callers inspect replaceErr.
func (c *Controller) replaceAgent(ag *core.Agent) {
	c.settings.replaceErr = nil
	if ag == nil {
		c.settings.replaceErr = errors.New("agent builder returned nil")
		return
	}
	if old := c.turnSeam.currentAgent(); old != nil {
		ag.RestoreMessages(old.Messages())
		ag.SeedCost(old.Cost())
		ag.SeedLastTurnUsage(old.LastTurnUsage())
	}
	c.settings.replaceErr = c.turnSeam.setAgent(ag)
}
func (c *Controller) swapModel(p, m string) error {
	if c.deps.AgentBuilder == nil {
		return errors.New("agent builder unavailable")
	}
	ag, actualP, actualM, err := c.deps.AgentBuilder.BuildAgentFor(p, m)
	if err != nil {
		return err
	}
	c.replaceAgent(ag)
	if c.settings.replaceErr != nil {
		return c.settings.replaceErr
	}
	c.deps.Provider, c.deps.Model = actualP, actualM
	if c.deps.SessionStore != nil {
		c.deps.SessionStore.PersistModel(actualP, actualM)
	}
	return nil
}

// SetModel builds a replacement without discarding the transcript.
func (c *Controller) SetModel(ctx context.Context, p, m string) error {
	return c.settingsCall(ctx, func() error { return c.swapModel(p, m) })
}

// SetReasoning persists and updates the idle agent's reasoning level.
func (c *Controller) SetReasoning(ctx context.Context, level string) error {
	return c.settingsCall(ctx, func() error {
		if c.deps.SettingsStore != nil {
			if err := c.deps.SettingsStore.SetReasoning(level); err != nil {
				return err
			}
		}
		c.deps.Reasoning = level
		if ag := c.turnSeam.currentAgent(); ag != nil {
			ag.Reasoning = level
		}
		return nil
	})
}

// SetFastMode validates the provider before changing runtime state.
func (c *Controller) SetFastMode(ctx context.Context, enabled bool) error {
	return c.ApplySettings(ctx, SettingsPatch{FastMode: &enabled})
}

// ApplyProfile applies a one-based configured model shortcut.
func (c *Controller) ApplyProfile(ctx context.Context, n int) error {
	return c.settingsCall(ctx, func() error {
		if n < 1 || n > len(c.deps.QuickModelShortcuts) {
			return fmt.Errorf("unknown model profile %d", n)
		}
		p := c.deps.QuickModelShortcuts[n-1]
		if err := provider.ValidateFastMode(p.Provider, p.FastMode); err != nil {
			return err
		}
		if err := c.swapModel(p.Provider, p.Model); err != nil {
			return err
		}
		c.deps.Reasoning = p.Reasoning
		setSetting(&c.deps.FastMode, &p.FastMode)
		if ag := c.turnSeam.currentAgent(); ag != nil {
			ag.Reasoning = p.Reasoning
			ag.SetFastMode(p.FastMode)
		}
		return nil
	})
}

// setSetting retains assembly's shared setting pointers, allocating only when absent.
func setSetting[T any](dst **T, src *T) {
	if src == nil {
		return
	}
	if *dst == nil {
		*dst = new(T)
	}
	**dst = *src
}

// ApplySettings changes only domain settings; orchestration remains launch-only.
func (c *Controller) ApplySettings(ctx context.Context, p SettingsPatch) error {
	return c.settingsCall(ctx, func() error {
		if p.AutoCompactThreshold != nil && (*p.AutoCompactThreshold < 0 || *p.AutoCompactThreshold > 100) {
			return errors.New("auto compact threshold must be 0..100")
		}
		if p.FastMode != nil {
			if err := provider.ValidateFastMode(c.deps.Provider, *p.FastMode); err != nil {
				return err
			}
		}
		if c.deps.SettingsStore != nil {
			if p.AutoSubagentsEnabled != nil {
				if err := c.deps.SettingsStore.SetAutoSubagents(*p.AutoSubagentsEnabled); err != nil {
					return err
				}
			}
			if p.JailByDefault != nil {
				if err := c.deps.SettingsStore.SetJailByDefault(*p.JailByDefault); err != nil {
					return err
				}
			}
		}
		setSetting(&c.deps.AutoSubagentsEnabled, p.AutoSubagentsEnabled)
		setSetting(&c.deps.WebSearchEnabled, p.WebSearchEnabled)
		setSetting(&c.deps.LSPEnabled, p.LSPEnabled)
		setSetting(&c.deps.SubagentLSPEnabled, p.SubagentLSPEnabled)
		setSetting(&c.deps.AutoCompactThreshold, p.AutoCompactThreshold)
		setSetting(&c.deps.JailByDefault, p.JailByDefault)
		setSetting(&c.deps.FastMode, p.FastMode)
		setSetting(&c.deps.GoalMaxTokenBudget, p.GoalMaxTokenBudget)
		if p.FastMode != nil {
			if ag := c.turnSeam.currentAgent(); ag != nil {
				ag.SetFastMode(*p.FastMode)
			}
		}
		if c.deps.ToolRefresher != nil {
			if p.WebSearchEnabled != nil {
				c.deps.ToolRefresher.SetWebSearchAvailable(*p.WebSearchEnabled)
			}
			return c.deps.ToolRefresher.RefreshTools()
		}
		return nil
	})
}

// ChangeCWD resolves relative paths against the session, never the process cwd.
func (c *Controller) ChangeCWD(ctx context.Context, path string) error {
	return c.settingsCall(ctx, func() error {
		if !filepath.IsAbs(path) {
			path = filepath.Join(c.deps.CWD, path)
		}
		path = filepath.Clean(path)
		if !filepath.IsAbs(path) {
			return errors.New("session cwd must be absolute")
		}
		if c.deps.SessionStore == nil {
			return errors.New("session store unavailable")
		}
		if err := c.deps.SessionStore.ChangeCWD(path); err != nil {
			return err
		}
		c.deps.CWD = path
		if c.deps.ToolRefresher != nil {
			return c.deps.ToolRefresher.RefreshTools()
		}
		return nil
	})
}

// SetJail updates session confinement without changing process state.
func (c *Controller) SetJail(ctx context.Context, b bool) error {
	return c.ApplySettings(ctx, SettingsPatch{JailByDefault: &b})
}

// SetTitle persists a normalized title before publishing it.
func (c *Controller) SetTitle(ctx context.Context, title string) error {
	return c.settingsCall(ctx, func() error { return c.persistTitle(title) })
}
func (c *Controller) persistTitle(title string) error {
	title = core.NormalizeSessionTitle(strings.TrimSpace(title))
	if c.deps.SessionStore != nil {
		if err := c.deps.SessionStore.PersistTitle(title); err != nil {
			return err
		}
	}
	c.settings.title = title
	c.publish(EvTitle, TitleEvent{Title: title})
	return nil
}

// ToggleSkillPin uses the session cwd captured by assembly.
func (c *Controller) ToggleSkillPin(ctx context.Context, name string, global bool) error {
	return c.settingsCall(ctx, func() error {
		if c.deps.SkillStore == nil {
			return errors.New("skill store unavailable")
		}
		return c.deps.SkillStore.ToggleSkillPin(c.deps.CWD, name, global)
	})
}

// ReloadCredentials rebuilds through assembly, which owns credential resolution.
func (c *Controller) ReloadCredentials(ctx context.Context) error {
	return c.SetModel(ctx, c.Info().Provider, c.Info().Model)
}
