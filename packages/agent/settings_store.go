package agent

import (
	"fmt"

	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

type configSettingsStore struct{}

func (configSettingsStore) SetQuickModelShortcut(slot int, providerName, model string) error {
	if slot < 1 || slot > 9 {
		return nil
	}
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	if len(cfg.QuickModelShortcuts) < slot {
		next := make([]QuickModelShortcut, slot)
		copy(next, cfg.QuickModelShortcuts)
		cfg.QuickModelShortcuts = next
	}
	cfg.QuickModelShortcuts[slot-1] = QuickModelShortcut{Provider: providerName, Model: model}
	// Trim trailing empty slots so config.json stays compact.
	for len(cfg.QuickModelShortcuts) > 0 {
		last := cfg.QuickModelShortcuts[len(cfg.QuickModelShortcuts)-1]
		if last.Provider != "" || last.Model != "" {
			break
		}
		cfg.QuickModelShortcuts = cfg.QuickModelShortcuts[:len(cfg.QuickModelShortcuts)-1]
	}
	return SaveConfig(cfg)
}

func (configSettingsStore) SetInlineImages(enabled bool) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.InlineImagesEnabled = &enabled
	return SaveConfig(cfg)
}

func (configSettingsStore) SetTerminalAlertsEnabled(enabled bool) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.TerminalAlertsEnabled = &enabled
	return SaveConfig(cfg)
}

func (configSettingsStore) SetTerminalTitleEnabled(enabled bool) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.TerminalTitleEnabled = &enabled
	return SaveConfig(cfg)
}

func (configSettingsStore) SetAutoSubagents(enabled bool) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.AutoSubagentsEnabled = &enabled
	return SaveConfig(cfg)
}

func (configSettingsStore) SetPonytailEnabled(enabled bool) error {
	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("load config for Ponytail setting: %w", err)
	}
	cfg.PonytailEnabled = &enabled
	if err := SaveConfig(cfg); err != nil {
		return fmt.Errorf("save Ponytail setting: %w", err)
	}
	return nil
}

func (configSettingsStore) SetWebSearchEnabled(enabled bool) error {
	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("load config for web search setting: %w", err)
	}
	cfg.WebSearchEnabled = &enabled
	if err := SaveConfig(cfg); err != nil {
		return fmt.Errorf("save web search setting: %w", err)
	}
	return nil
}

func (configSettingsStore) SetFastMode(enabled bool) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.FastMode = &enabled
	return SaveConfig(cfg)
}

func (configSettingsStore) SetLSPEnabled(enabled bool) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.LSPEnabled = &enabled
	return SaveConfig(cfg)
}

func (configSettingsStore) SetSubagentLSPEnabled(enabled bool) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.SubagentLSPEnabled = &enabled
	return SaveConfig(cfg)
}

func (configSettingsStore) SetAutoCompactThreshold(percent int) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.AutoCompactThreshold = &percent
	return SaveConfig(cfg)
}

func (configSettingsStore) SetJailByDefault(enabled bool) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.JailByDefault = &enabled
	return SaveConfig(cfg)
}

func (configSettingsStore) SetRecursiveFileSuggest(enabled bool) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.RecursiveFileSuggest = &enabled
	return SaveConfig(cfg)
}

func (configSettingsStore) SetRespectGitignore(enabled bool) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.RespectGitignore = &enabled
	return SaveConfig(cfg)
}

func (configSettingsStore) SetCompactMode(enabled bool) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.CompactMode = &enabled
	return SaveConfig(cfg)
}

func (configSettingsStore) SetShowInstructionsAtStartup(enabled bool) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.ShowInstructionsAtStartup = &enabled
	return SaveConfig(cfg)
}

func (configSettingsStore) SetTUIInputStyle(style string) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	style = tui.NormalizeInputStyle(style)
	if style == tui.InputStylePlain {
		cfg.TUIInputStyle = ""
	} else {
		cfg.TUIInputStyle = style
	}
	return SaveConfig(cfg)
}

func (configSettingsStore) SetTUIStatusPosition(position string) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	position = tui.NormalizeStatusPosition(position)
	if position == tui.StatusPositionAboveInput {
		cfg.TUIStatusPosition = ""
	} else {
		cfg.TUIStatusPosition = position
	}
	return SaveConfig(cfg)
}

func (configSettingsStore) SetTUIWorkingPosition(position string) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	position = tui.NormalizeWorkingPosition(position)
	if position == tui.WorkingPositionAboveInput {
		cfg.TUIWorkingPosition = ""
	} else {
		cfg.TUIWorkingPosition = position
	}
	return SaveConfig(cfg)
}

func (configSettingsStore) SetReasoning(level string) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	cfg.Reasoning = provider.NormalizeReasoning(level)
	return SaveConfig(cfg)
}

func (configSettingsStore) SetTheme(name string) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	if name == "auto" {
		name = ""
	}
	cfg.Theme = name
	return SaveConfig(cfg)
}

// AutoSubagentsEnabled reads whether the interactive primary agent runs in
// strict orchestrator mode. The canonical subagent tools remain available for
// user-requested delegation and skill-mandated workflows when disabled.
func AutoSubagentsEnabled() bool {
	cfg, err := LoadConfig()
	if err != nil {
		return false
	}
	return cfg.AutoSubagentsEnabled != nil && *cfg.AutoSubagentsEnabled
}

// AutoSubagentsSystemAddendum is appended to the system prompt when
// auto-subagents is enabled. It makes the interactive primary agent an
// orchestrator.
const AutoSubagentsSystemAddendum = `Auto-subagents are enabled. You are the primary-agent orchestrator, not an implementer.

- Use read and grep only for read-only codebase exploration before dispatching work. Use web_search only for public-web research that improves worker task instructions.
- Delegate all implementation, debugging/testing, and code-review work to an appropriately named subagent profile or a clearly described general worker.
- Do not write or edit code yourself, make direct implementation tool calls, inspect or review code through implementation tools, or apply worker patches. You may use read, grep, and web_search for the read-only research above and review worker reports.
- Give every worker a self-contained task and synthesize the results.

Workers start without this conversation's context. Shared-worktree workers edit the same working directory, so coordinate dependent work and avoid conflicting edits. Use isolation:"worktree" for parallel coding when workers need separate trees; its changes are returned as a patch for a worker to integrate, not for you to apply. Do not invent feature scope beyond the user's request. Child workers cannot recursively spawn more sub-agents in v1.

Use required:true whenever your answer or a declared workflow depends on that worker, especially for required validation or finalization. Required workers remain asynchronous unless you explicitly set subagent_spawn's wait field: without it, manager calls return immediately, the parent stays free to coordinate or perform independent work, and the host delivers terminal outcomes through completion updates. Failed, canceled, or interrupted required work remains unmet: use an enabled manager follow-up action to retry it and do not claim completion. An indeterminate outcome after host restart must not be retried automatically; the user must inspect durable results and side effects, then explicitly resume, restart, or remove the worker. Only the user can waive required work by explicitly removing the terminal worker with /subagents rm. Keep required:false only for independent work that may safely finish later.

Completion is host-event-driven unless you explicitly set subagent_spawn's wait field to a whole number of seconds (1–300) for that initial task. Omitting wait always returns immediately. An expired wait leaves the accepted child active, whether queued or running; do not retry it until terminal failure, cancellation, or interruption. Never use "bash sleep", "watch", "tail -f", polling loops, repeated "subagent_status", or dashboard, metadata, or file checks solely to wait. Those are not completion signals. Continue unrelated independent tasks; otherwise end or yield your turn until the host injects a completion update.

When workers finish, use the host update's agent ID, status, task, optional error, and final response or tail to coordinate any follow-up and summarize the outcome. Treat [auto-subagents update] and [required-subagents update] messages as observed worker state, not as new user requests.`

// OnDemandSubagentsSystemAddendum keeps the canonical subagent tools
// available without enabling proactive primary-agent orchestration.
const OnDemandSubagentsSystemAddendum = `Subagent tools are available only when the user asks you to delegate and the relevant launch-time tool permission allows it, or an active skill workflow requires delegation. Use them only in those cases. Mark delegated work required:true when the user's request or workflow requires its outcome before completion. Omit subagent_spawn's wait field to return immediately and receive completion through a host update. Set wait only to an explicit 1–300 second value when you need to wait for that initial task; an expired wait leaves the accepted child active, whether queued or running. Do not retry it until terminal failure, cancellation, or interruption. Failed, canceled, or interrupted required work remains unmet and must be retried; after host restart, an indeterminate outcome requires user-directed reconciliation of durable results and side effects before non-idempotent work is retried. Do not produce a terminal answer until required work is satisfied. Only the user may waive terminal required work by removing the worker with /subagents rm. Do not delegate proactively or switch into an orchestrator role; otherwise perform the work yourself.`

// AutoSubagentsDelegationUnavailableAddendum explains why the strict
// orchestrator contract still applies when launch-time policy withholds the
// spawn tool.
const AutoSubagentsDelegationUnavailableAddendum = `Delegation is unavailable in this session because the launch-time tool policy does not expose subagent_spawn. Remain the primary-agent orchestrator: report this limitation to the user rather than implementing, debugging/testing, or reviewing directly, and end or yield your turn until delegation is available. Do not treat subagent_status or other tools as a substitute for subagent_spawn.`

// AutoSubagentsSystemAddendumFor returns the strict contract with lifecycle
// guidance only for manager actions exposed by the launch-time tool policy.
// It adds explicit limitation guidance when the spawn tool is not available.
func AutoSubagentsSystemAddendumFor(spawnToolAllowed, stopToolAllowed, resumeToolAllowed bool) string {
	addendum := AutoSubagentsSystemAddendum
	switch {
	case stopToolAllowed && resumeToolAllowed:
		addendum += "\n\nManager lifecycle actions are available: use subagent_stop to request termination of a stuck worker, and subagent_resume with an agent id and follow-up prompt to continue an idle worker or restart a stopped worker with its existing session context."
	case stopToolAllowed:
		addendum += "\n\nA manager lifecycle action is available: use subagent_stop to request termination of a stuck worker."
	case resumeToolAllowed:
		addendum += "\n\nA manager lifecycle action is available: use subagent_resume with an agent id and follow-up prompt to continue an idle worker or restart a stopped worker with its existing session context."
	}
	if !spawnToolAllowed {
		if stopToolAllowed || resumeToolAllowed {
			addendum += "\n\nSpawning new workers is unavailable in this session. Use only the enabled manager lifecycle actions for existing workers. Do not implement, debug, test, or review directly."
		} else {
			addendum += "\n\n" + AutoSubagentsDelegationUnavailableAddendum
		}
	}
	return addendum
}
