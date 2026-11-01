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
	// profiles caches the store's slots; activeProfile is per session (0 none).
	profiles      []QuickModelShortcut
	activeProfile int
	title         string
	titleStarted  bool
	replaceErr    error
	// toolsDirty defers a RefreshToolsWhenIdle request until no turn or
	// compaction runs.
	toolsDirty bool
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
		// Ops may fail after partially applying state (store writes, tool
		// refresh); Info always reflects what actually changed.
		c.updateInfo()
		c.publish(EvInfo, InfoEvent{Info: c.Info()})
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
	return c.settingsCall(ctx, func() error {
		if err := c.swapModel(p, m); err != nil {
			return err
		}
		return c.autosaveModel()
	})
}

// SetReasoning persists and updates the idle agent's reasoning level; with an
// active profile it is saved into that profile instead of plain settings.
func (c *Controller) SetReasoning(ctx context.Context, level string) error {
	return c.settingsCall(ctx, func() error {
		if n := c.settings.activeProfile; n > 0 {
			level = clampReasoning(c.deps.Provider, c.deps.Model, level)
			p := c.liveProfile()
			p.Reasoning = level
			if err := c.saveProfile(n, p, false); err != nil {
				return err
			}
		} else if c.deps.SettingsStore != nil {
			if err := c.deps.SettingsStore.SetReasoning(level); err != nil {
				return err
			}
		}
		c.setLiveReasoning(level)
		return nil
	})
}

// SetFastMode validates the provider before changing runtime state.
func (c *Controller) SetFastMode(ctx context.Context, enabled bool) error {
	return c.ApplySettings(ctx, SettingsPatch{FastMode: &enabled})
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
		if p.AutoCompactThreshold != nil && !validCompactThreshold(*p.AutoCompactThreshold) {
			return fmt.Errorf("auto compact threshold %d: must be one of 0 (off), 70, 80, 85, 90, 95", *p.AutoCompactThreshold)
		}
		if p.FastMode != nil {
			if err := provider.ValidateFastMode(c.deps.Provider, *p.FastMode); err != nil {
				return err
			}
		}
		// Each persisted field updates memory right after its own write, so a
		// later failure leaves memory matching the store.
		if p.FastMode != nil {
			if n := c.settings.activeProfile; n > 0 {
				prof := c.liveProfile()
				prof.FastMode = *p.FastMode
				if err := c.saveProfile(n, prof, false); err != nil {
					return err
				}
			} else if c.deps.SettingsStore != nil {
				if err := c.deps.SettingsStore.SetFastMode(*p.FastMode); err != nil {
					return err
				}
			}
		}
		if p.AutoSubagentsEnabled != nil && c.deps.SettingsStore != nil {
			if err := c.deps.SettingsStore.SetAutoSubagents(*p.AutoSubagentsEnabled); err != nil {
				return err
			}
		}
		setSetting(&c.deps.AutoSubagentsEnabled, p.AutoSubagentsEnabled)
		if p.JailByDefault != nil && c.deps.SettingsStore != nil {
			if err := c.deps.SettingsStore.SetJailByDefault(*p.JailByDefault); err != nil {
				return err
			}
		}
		setSetting(&c.deps.JailByDefault, p.JailByDefault)
		if p.WebSearchEnabled != nil && c.deps.SettingsStore != nil {
			if err := c.deps.SettingsStore.SetWebSearchEnabled(*p.WebSearchEnabled); err != nil {
				return err
			}
		}
		setSetting(&c.deps.WebSearchEnabled, p.WebSearchEnabled)
		setSetting(&c.deps.LSPEnabled, p.LSPEnabled)
		setSetting(&c.deps.SubagentLSPEnabled, p.SubagentLSPEnabled)
		setSetting(&c.deps.AutoCompactThreshold, p.AutoCompactThreshold)
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
			return refreshApplied(c.deps.ToolRefresher.RefreshTools())
		}
		return nil
	})
}

// RequestToolRefresh asks the session to rebuild its tool registry and
// system prompt, e.g. after a background MCP server became ready or changed
// its tools. It never blocks: the request is handed to the actor from a
// goroutine bound to the session lifetime. Requests made while one is still
// queued coalesce into it; a request made after the queued one reached the
// actor (even mid-rebuild) queues another, so no update is lost. The rebuild
// runs when the session is idle, otherwise right after the running turn or
// compaction ends, so a turn never sees its tools change mid-flight.
func (c *Controller) RequestToolRefresh() {
	if !c.refreshQueued.CompareAndSwap(false, true) {
		return
	}
	go func() {
		err := c.post(c.actorBase, func() {
			c.refreshQueued.Store(false)
			c.settings.toolsDirty = true
			c.flushToolRefresh()
		})
		if err != nil {
			c.refreshQueued.Store(false)
			if !errors.Is(err, ErrClosed) {
				c.logger().Debug("tool refresh request dropped", "err", err)
			}
		}
	}()
}

// flushToolRefresh runs a deferred tool refresh when idle (actor only).
func (c *Controller) flushToolRefresh() {
	if !c.settings.toolsDirty || c.isBusy() || c.compact.busy {
		return
	}
	c.settings.toolsDirty = false
	if c.deps.ToolRefresher == nil {
		return
	}
	if err := c.deps.ToolRefresher.RefreshTools(); err != nil {
		c.logger().Warn("deferred tool refresh failed", "err", err)
	}
	c.updateInfo()
	c.publish(EvInfo, InfoEvent{Info: c.Info()})
}

// validCompactThreshold matches the levels maybeAutoCompact honours.
func validCompactThreshold(v int) bool {
	switch v {
	case 0, 70, 80, 85, 90, 95:
		return true
	}
	return false
}

// refreshApplied reports a refresh failure after the setting already changed.
func refreshApplied(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("setting applied, but refresh tools: %w", err)
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
			return refreshApplied(c.deps.ToolRefresher.RefreshTools())
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
	c.updateInfo()
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

// ReloadCredentials rebuilds through assembly, which owns credential
// resolution. The swap detaches the active profile instead of autosaving.
func (c *Controller) ReloadCredentials(ctx context.Context) error {
	info := c.Info()
	return c.settingsCall(ctx, func() error {
		if err := c.swapModel(info.Provider, info.Model); err != nil {
			return err
		}
		c.settings.activeProfile = 0
		return nil
	})
}
