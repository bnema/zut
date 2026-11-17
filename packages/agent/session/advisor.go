package session

import (
	"context"
	"errors"
	"strings"

	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// advisorEffective reports how the live agent reaches its advisor. It takes
// the already built agent: Info must never trigger the lazy agent build, so
// the value stays empty until the first turn or model change builds one.
// Native counts only while status still matches the agent's decided native
// advisor, mirroring the per-request guard that drops a stale one.
func advisorEffective(ag *core.Agent, status AdvisorStatus) string {
	if ag == nil {
		return ""
	}
	if adv := ag.ServerAdvisor(); adv != nil {
		if nativeMatchesStatus(*adv, status) {
			return AdvisorEffectiveNative
		}
		return ""
	}
	if ag.HasTool(tools.AdvisorToolName) {
		return AdvisorEffectiveClient
	}
	return ""
}

// nativeMatchesStatus reports whether status still selects adv: enabled,
// auto mode, same max_tokens, and a spec naming adv's model (optionally
// with a ":reasoning" suffix, which native mode ignores).
func nativeMatchesStatus(adv provider.NativeAdvisor, status AdvisorStatus) bool {
	if !status.Enabled || status.Mode != "auto" || status.MaxTokens != adv.MaxTokens {
		return false
	}
	_, model, ok := strings.Cut(status.Model, "/")
	return ok && (model == adv.Model || strings.HasPrefix(model, adv.Model+":"))
}

// initAdvisor caches the persisted advisor status for Info. A read failure
// leaves the status absent rather than failing session startup.
func (c *Controller) initAdvisor() {
	if c.deps.Advisor == nil {
		return
	}
	if status, err := c.deps.Advisor.AdvisorStatus(); err == nil {
		c.settings.advisor = &status
	}
}

// syncAdvisorStatus re-reads the persisted advisor status (config is shared
// by every session, so another session's /advisor may have changed it). When
// it differs from the cached one, the cache is updated and a tool refresh is
// marked so the advisor tool, prompt guidance and native mode follow it.
// Actor only.
func (c *Controller) syncAdvisorStatus() {
	if c.deps.Advisor == nil {
		return
	}
	status, err := c.deps.Advisor.AdvisorStatus()
	if err != nil {
		return
	}
	if cur := c.settings.advisor; cur != nil && *cur == status {
		return
	}
	changed := c.settings.advisor != nil
	c.settings.advisor = &status
	if changed {
		c.settings.toolsDirty = true
	}
}

// SetAdvisor persists an advisor change, then rebuilds tools and the system
// prompt so the advisor tool and its guidance follow the enabled state.
func (c *Controller) SetAdvisor(ctx context.Context, change AdvisorChange) (AdvisorStatus, error) {
	var status AdvisorStatus
	err := c.settingsCall(ctx, func() error {
		if c.deps.Advisor == nil {
			return errors.New("advisor settings unavailable")
		}
		next, err := c.deps.Advisor.SetAdvisor(change)
		if err != nil {
			return err
		}
		status = next
		c.settings.advisor = &next
		if c.deps.ToolRefresher != nil {
			return refreshApplied(c.deps.ToolRefresher.RefreshTools())
		}
		return nil
	})
	if err == nil {
		if info := c.Info(); info.Advisor != nil {
			status.Effective = info.Advisor.Effective
		}
	}
	return status, err
}
