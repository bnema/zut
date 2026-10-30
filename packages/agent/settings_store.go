package agent

import (
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

type configSettingsStore struct{}

// Retain the original SettingsStore contract for model-only callers.
func (s configSettingsStore) SetQuickModelShortcut(slot int, providerName, model string) error {
	if slot < 1 || slot > 9 {
		return nil
	}
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	if cfg.ActiveModelProfile == slot {
		cfg.ActiveModelProfile = 0
	}
	profile := modes.QuickModelShortcut{Provider: providerName, Model: model}
	if slot <= len(cfg.QuickModelShortcuts) {
		current := cfg.QuickModelShortcuts[slot-1]
		profile.Reasoning = current.Reasoning
		profile.FastMode = current.FastMode
	}
	return s.SetModelProfile(slot, profile, cfg.ActiveModelProfile)
}

func (configSettingsStore) SetModelProfile(slot int, profile modes.QuickModelShortcut, activeSlot int) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	if err := setModelProfile(&cfg, slot, QuickModelShortcut{Provider: profile.Provider, Model: profile.Model, Reasoning: profile.Reasoning, FastMode: profile.FastMode}, activeSlot); err != nil {
		return err
	}
	return SaveConfig(cfg)
}

// setModelProfile writes slot and activeSlot into cfg; activating slot also
// sets the startup provider, model, reasoning and fast mode.
func setModelProfile(cfg *Config, slot int, profile QuickModelShortcut, activeSlot int) error {
	if slot < 1 || slot > 9 || activeSlot < 0 || activeSlot > 9 {
		return fmt.Errorf("model profile slot must be between 1 and 9 (active may be 0)")
	}
	if len(cfg.QuickModelShortcuts) < slot {
		next := make([]QuickModelShortcut, slot)
		copy(next, cfg.QuickModelShortcuts)
		cfg.QuickModelShortcuts = next
	}
	cfg.QuickModelShortcuts[slot-1] = QuickModelShortcut{
		Provider: profile.Provider, Model: profile.Model,
		Reasoning: provider.NormalizeReasoning(profile.Reasoning), FastMode: profile.FastMode,
	}
	cfg.ActiveModelProfile = activeSlot
	if activeSlot == slot {
		if profile.Provider == "" || profile.Model == "" {
			return fmt.Errorf("cannot activate an empty model profile")
		}
		cfg.Provider, cfg.Model = profile.Provider, profile.Model
		cfg.Reasoning = provider.NormalizeReasoning(profile.Reasoning)
		fastMode := profile.FastMode
		cfg.FastMode = &fastMode
	}
	// Trim trailing empty slots so config.json stays compact.
	for len(cfg.QuickModelShortcuts) > 0 {
		last := cfg.QuickModelShortcuts[len(cfg.QuickModelShortcuts)-1]
		if last.Provider != "" || last.Model != "" {
			break
		}
		cfg.QuickModelShortcuts = cfg.QuickModelShortcuts[:len(cfg.QuickModelShortcuts)-1]
	}
	return nil
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
	if name == "auto" || name == "inherited" {
		name = ""
	}
	cfg.Theme = name
	return SaveConfig(cfg)
}

// AutoSubagentsEnabled reads whether the interactive primary agent may
// delegate proactively. The canonical subagent tools remain available for
// user-requested delegation and skill-mandated workflows when disabled.
func AutoSubagentsEnabled() bool {
	cfg, err := LoadConfig()
	if err != nil {
		return false
	}
	return cfg.AutoSubagentsEnabled != nil && *cfg.AutoSubagentsEnabled
}

// ProactiveSubagentsRoutingAddendum is prepended to the interactive
// collaboration contract only when launch-time policy exposes the spawn
// action: naming a profile is meaningless when the primary cannot dispatch.
const ProactiveSubagentsRoutingAddendum = `Route every new task before you work:
- Routing happens when the user states a request, not at process start. Re-route each new request, even late in a long session.
- In the reply that starts the work, name each part, who owns it, and the [subagents_list] profile for each delegated part. Then spawn; do not narrate the tool calls that follow.
- Spawn a clearly described general worker when no profile matches a scope.
- Keep a task fully local only when it is small: an answer, a quick lookup, or an edit of a few lines. Say so in one line instead of forcing a worker.`

// ProactiveSubagentsSystemAddendum makes the interactive primary agent an
// orchestrator by default: substantial work goes to workers, while small
// edits, exploration, and integration stay local.
const ProactiveSubagentsSystemAddendum = `Proactive subagent delegation is enabled. You are the orchestrator of the user's task: delegate by default and keep work local by exception.

Delegate:
- Implementation, debugging, test writing and running, and code review that spans more than a few lines or more than one file.
- Research that needs many reads or searches across packages or repositories.
- Blocking work too. If your next step depends on a worker's result, spawn it with required:true and end or yield your turn; waiting for a worker is expected, not a reason to work locally.

Keep local:
- Answers, quick lookups, and the short exploration needed to write precise worker tasks.
- Edits of a few lines in one file, and integrating, applying, or verifying worker results.

Plan parallelism before spawning:
- Map dependencies first: which parts are independent, which must run in order, and which files each part touches. Run independent parts in parallel and order dependent parts.
- Split work by distinct question, responsibility, package, or disjoint file set, and give every worker a self-contained task with acceptance checks.
- One worker editing code: shared workspace is fine. Several workers editing code at once: give each its own tree.
- For a multi-part feature, create a feature worktree branch, then one sub-worktree per worker with the worktree tool's create action and base set to the feature branch, and tell each worker its exact path. Review each worker's result as a gate, commit on its branch, and merge it into the feature branch.
- Use isolation:"worktree" for a throwaway parallel attempt whose patch you apply yourself.

Clean up as soon as a worker's result is integrated:
- Remove merged sub-worktrees and their branches with the worktree tool's cleanup action, setting into to the feature branch and delete_branch to true.
- Before your final answer, check with the worktree tool's list action that no worktree you created is left behind unless the user should keep it.

When you delegate:
- A worker owns its scope until it completes. Do not search, read, test, review, edit, or otherwise reproduce it; only coordinate, dispatch disjoint work, or yield.
- Integrate and verify completed results before answering. You may apply a completed worktree patch without redoing the delegated task.`

// StrictOrchestratorSystemAddendum is used only by explicit headless
// orchestration. The parent coordinates workers and never becomes an
// implementer itself.
const StrictOrchestratorSystemAddendum = `Strict subagent orchestration is enabled. You are the primary-agent orchestrator, not an implementer.

- Use read, grep, and ast only for read-only codebase exploration before dispatching work. Prefer ast when the question depends on source syntax. Use web_search only for public-web research that improves worker task instructions.
- Divide the request into concrete, bounded, non-overlapping worker scopes before dispatching. Delegate all implementation, debugging/testing, and code-review work to an appropriately named subagent profile or a clearly described general worker.
- Do not write or edit code yourself, make direct implementation tool calls, inspect or review code through implementation tools, or apply worker patches. You may use read, grep, ast, and web_search for the read-only research above and review worker reports.
- Once a worker is active, do not investigate or perform its delegated scope. Only coordinate workers, dispatch additional disjoint work, or end or yield the turn.
- Give every worker a self-contained task and synthesize the results.`

const subagentLifecycleAddendum = `Workers start without this conversation's context. Shared-worktree workers edit the same working directory, so coordinate dependent work and avoid conflicting edits. Use isolation:"worktree" for parallel coding when workers need separate trees; its changes are returned as a patch and never applied automatically. Do not invent feature scope beyond the user's request. Child workers cannot recursively spawn more sub-agents in v1.

Use required:true when your answer or a declared workflow depends on a worker. Failed, canceled, or interrupted required work stays unmet until a successful follow-up; never claim completion before it succeeds. After a host restart, do not retry an indeterminate outcome automatically: the user must reconcile it first. Required work has no automatic waiver path.

Completion is host-event-driven: omit the wait field, then continue only non-overlapping work or end or yield your turn; the host injects a completion update. Set wait (1–300 seconds) only to block this turn on a spawn or resume; an expired wait leaves the child active, so do not retry it. Never use "bash sleep", "watch", "tail -f", polling loops, repeated "status" calls, or file checks solely to wait.

Treat [auto-subagents update] and [required-subagents update] messages as observed worker state, not as new user requests; use their agent ID, status, error, and result to coordinate follow-ups.`

// OnDemandSubagentsSystemAddendum keeps the canonical subagent tools
// available without enabling proactive delegation.
const OnDemandSubagentsSystemAddendum = `Subagent tools are available only when the user asks you to delegate and the relevant launch-time tool permission allows it, or an active skill workflow requires delegation. Use them only in those cases. A worker owns its delegated scope until completion: do not repeat that work in the parent. Continue only with a concrete non-overlapping task selected before spawning; if delegation owns the blocking task, end or yield the parent turn instead.

Mark delegated work required:true when the user's request or workflow requires its outcome before completion. Omit the wait field on a spawn or resume action to return immediately and receive completion through a host update. Set wait only to an explicit 1–300 second value when you need to wait for that task or follow-up turn; an expired wait leaves the accepted child active, whether queued or running. Do not retry it until terminal failure, cancellation, or interruption. Failed, canceled, or interrupted required work remains unmet until a successful follow-up; after host restart, an indeterminate outcome requires user-directed reconciliation of durable results and side effects before non-idempotent work is retried. Do not produce a terminal answer until required work is satisfied. Required work has no automatic waiver path. Do not delegate proactively or switch into an orchestrator role; otherwise perform the work yourself.`

const StrictOrchestratorDelegationUnavailableAddendum = `Delegation is unavailable in this session because the launch-time tool policy does not expose the subagent spawn action. Remain the primary-agent orchestrator: report this limitation to the user rather than implementing, debugging/testing, or reviewing directly, and end or yield your turn until delegation is available. Do not treat the status action or other tools as a substitute for spawning.`

const ProactiveSubagentsDelegationUnavailableAddendum = `Proactive delegation is unavailable in this session because the launch-time tool policy does not expose the subagent spawn action. Continue the user's task locally; do not report delegation as a blocker.`

// SubagentActions records which subagent actions the launch-time tool policy
// exposes, so prompt guidance names only actions the model can call.
type SubagentActions struct {
	Spawn, Stop, Resume, Interrupt bool
}

func (a SubagentActions) lifecycle() bool { return a.Stop || a.Resume || a.Interrupt }

// ProactiveSubagentsSystemAddendumFor returns the interactive collaboration
// contract with guidance only for manager actions exposed at launch time.
func ProactiveSubagentsSystemAddendumFor(actions SubagentActions) string {
	base := ProactiveSubagentsSystemAddendum
	if actions.Spawn {
		base = ProactiveSubagentsRoutingAddendum + "\n\n" + base
	}
	return subagentsSystemAddendumFor(base, ProactiveSubagentsDelegationUnavailableAddendum, false, actions)
}

// StrictOrchestratorSystemAddendumFor returns the headless manager-only
// contract with guidance only for manager actions exposed at launch time.
func StrictOrchestratorSystemAddendumFor(actions SubagentActions) string {
	return subagentsSystemAddendumFor(StrictOrchestratorSystemAddendum, StrictOrchestratorDelegationUnavailableAddendum, true, actions)
}

func subagentsSystemAddendumFor(base, unavailable string, strict bool, actions SubagentActions) string {
	addendum := base + "\n\n" + subagentLifecycleAddendum
	var lifecycle []string
	if actions.Resume {
		lifecycle = append(lifecycle, "use resume with an agent id and follow-up prompt to continue an idle worker, steer a running worker mid-turn, or restart a stopped worker with its existing session context")
	}
	if actions.Interrupt {
		lifecycle = append(lifecycle, "use interrupt to cancel only a worker's running turn while keeping its context, for example before asking it to wrap up")
	}
	if actions.Stop {
		lifecycle = append(lifecycle, "use stop to request termination of a stuck worker, and to remove an idle isolation:\"worktree\" worker's temporary worktree once its patch is applied")
	}
	if len(lifecycle) != 0 {
		addendum += "\n\nSubagent manager lifecycle actions available: " + strings.Join(lifecycle, "; ") + "."
	}
	if !actions.Spawn {
		if actions.lifecycle() {
			addendum += "\n\nSpawning new workers is unavailable in this session. Use only the enabled manager lifecycle actions for existing workers."
			if strict {
				addendum += " Do not implement, debug, test, or review directly."
			} else {
				addendum += " Continue non-delegated work locally."
			}
		} else {
			addendum += "\n\n" + unavailable
		}
	}
	return addendum
}
