package modes

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	toolspkg "github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

func effectiveImageProtocol(override *bool) tui.ImageProtocol {
	detected := tui.DetectImageProtocol()
	if override == nil {
		return detected
	}
	if !*override {
		return tui.ImageProtocolNone
	}
	return detected
}
func imageProtocolName(p tui.ImageProtocol) string {
	switch p {
	case tui.ImageProtocolKitty:
		return "kitty/ghostty"
	case tui.ImageProtocolITerm2:
		return "iTerm2"
	default:
		return "none"
	}
}
func onOff(v bool) string {
	if v {
		return "enabled"
	}
	return "disabled"
}
func (i *Interactive) applyInputCursorColor() {
	if i == nil || i.cfg.Terminal == nil {
		return
	}
	if i.cfg.EffectiveThemeName == "auto" && i.cfg.Theme.FG == tui.TerminalDefault() {
		_, _ = i.cfg.Terminal.Write([]byte(tui.ResetCursorColor() + tui.CursorShapeBlock()))
		return
	}
	color := tui.Color256(15)
	if i.cursorDimmed {
		color = i.cfg.Theme.DimColor(color, modalBackdropDimPercent)
	}
	_, _ = i.cfg.Terminal.Write([]byte(tui.CursorColor(color) + tui.CursorShapeBlock()))
}
func (i *Interactive) setInputCursorDimmed(dimmed bool) {
	if i == nil || i.cursorDimmed == dimmed {
		return
	}
	i.cursorDimmed = dimmed
	i.applyInputCursorColor()
}
func reasoningSettingOptions(levels []string) []settingsOption {
	descriptions := map[string]string{
		"":        "no reasoning",
		"minimum": "very brief (~1k tokens)",
		"low":     "light (~2k tokens)",
		"medium":  "moderate (~8k tokens)",
		"high":    "deep (~16k tokens)",
		"xhigh":   "extra-high effort",
		"max":     "unconstrained effort",
	}
	options := make([]settingsOption, 0, len(levels))
	for _, level := range levels {
		label := level
		if label == "" {
			label = "off"
		}
		options = append(options, settingsOption{value: level, label: label, desc: descriptions[level]})
	}
	return options
}
func (i *Interactive) reasoningSettingItem() settingsItem {
	levels := []string{"", "minimum", "low", "medium", "high", "xhigh", "max"}
	reasoning := provider.NormalizeReasoning(i.cfg.Reasoning)
	desc := "reasoning depth for reasoning-capable models"
	hint := ""
	if m, err := provider.FindModel(i.cfg.Provider, i.cfg.Model); err == nil {
		levels = provider.AvailableReasoningLevels(m)
		reasoning = provider.ClampReasoningForModel(m, reasoning)
		if !m.Reasoning {
			hint = "current model does not support reasoning"
			desc += "; current model does not support reasoning"
		} else if len(levels) == 1 {
			hint = "current model has no adjustable reasoning levels"
			desc += "; current model has no adjustable reasoning levels"
		} else {
			desc += "; only levels supported by the current model are shown"
		}
	}
	options := reasoningSettingOptions(levels)
	choice := 0
	for idx, opt := range options {
		if opt.value == reasoning {
			choice = idx
			break
		}
	}

	return settingsItem{
		key:     "reasoning",
		label:   "reasoning level",
		desc:    desc,
		options: options,
		choice:  choice,
		hint:    hint,
	}
}
func (i *Interactive) openReasoningDialog() {
	i.settingsDialog.OpenDirectOption(i.reasoningSettingItem())
}
func (i *Interactive) openSettingsDialog() {
	detected := tui.DetectImageProtocol()
	imgEnabled := detected != tui.ImageProtocolNone
	if i.cfg.InlineImagesEnabled != nil {
		imgEnabled = *i.cfg.InlineImagesEnabled
	}
	imgDisabled := detected == tui.ImageProtocolNone
	imgHint := ""
	if imgDisabled {
		imgEnabled = false
		imgHint = "this terminal does not support inline images"
	} else {
		imgHint = "terminal supports " + imageProtocolName(detected)
	}

	terminalAlerts := terminalAlertsEnabled(i.cfg.TerminalAlertsEnabled)
	terminalTitles := terminalTitleEnabled(i.cfg.TerminalTitleEnabled)
	autoSubagents := false
	if i.cfg.AutoSubagentsEnabled != nil {
		autoSubagents = *i.cfg.AutoSubagentsEnabled
	}
	autoSubagentsDisabled := !i.autoSubagentsAvailable()
	autoSubagentsHint := i.autoSubagentsUnavailableHint()
	if autoSubagentsDisabled {
		autoSubagents = false
	}

	ponytailEnabled := i.ponytailEnabled()
	ponytailDisabled := !i.ponytailSettingsAvailable()
	ponytailHint := ""
	if ponytailDisabled {
		ponytailHint = "requires persistent settings and live prompt refresh support"
	}

	webSearchEnabled := i.webSearchEffectiveEnabled()
	webSearchDisabled := !i.webSearchSettingsAvailable()
	webSearchHint := i.webSearchUnavailableHint()

	fastMode := i.cfg.FastMode != nil && *i.cfg.FastMode
	fastModeHint := "OpenAI service tier"
	if !provider.SupportsFastMode(i.cfg.Provider) {
		fastModeHint = "only supported for OpenAI providers"
	}
	lspEnabled := i.cfg.LSPEnabled == nil || *i.cfg.LSPEnabled
	subagentLSPEnabled := i.cfg.SubagentLSPEnabled == nil || *i.cfg.SubagentLSPEnabled

	jailByDefault := i.cfg.JailByDefault != nil && *i.cfg.JailByDefault
	recursiveFiles := i.cfg.RecursiveFileSuggest != nil && *i.cfg.RecursiveFileSuggest
	respectGitignore := i.cfg.RespectGitignore == nil || *i.cfg.RespectGitignore
	compactMode := i.compactModeEnabled()
	showInstructions := i.cfg.ShowInstructionsAtStartup != nil && *i.cfg.ShowInstructionsAtStartup
	inputStyle := tui.NormalizeInputStyle(i.cfg.TUIInputStyle)
	statusPosition := tui.NormalizeStatusPosition(i.cfg.TUIStatusPosition)
	workingPosition := tui.NormalizeWorkingPosition(i.cfg.TUIWorkingPosition)
	quickItems := i.quickModelSettingItems()

	autoCompactOptions := []settingsOption{
		{value: "0", label: "off", desc: "disable context-percentage triggers; payload-too-large recovery stays enabled"},
		{value: "70", label: "70%", desc: "compact earlier to keep more context headroom"},
		{value: "80", label: "80%", desc: "compact with moderate context headroom"},
		{value: "85", label: "85%", desc: "default balance between history and headroom"},
		{value: "90", label: "90%", desc: "retain more history before compacting"},
		{value: "95", label: "95%", desc: "retain most history before compacting"},
	}
	autoCompactThreshold := NormalizeAutoCompactThreshold(i.cfg.AutoCompactThreshold)
	autoCompactChoice := 0
	for idx, opt := range autoCompactOptions {
		if opt.value == strconv.Itoa(autoCompactThreshold) {
			autoCompactChoice = idx
			break
		}
	}

	reasoningItem := i.reasoningSettingItem()

	themeName := i.cfg.ThemeName
	if themeName == "" {
		themeName = "auto"
	}
	if themeName != "auto" && !tui.ThemeExists(i.cfg.ZutHome, themeName) {
		themeName = "auto"
		i.cfg.ThemeName = ""
		if i.cfg.SettingsStore != nil {
			_ = i.cfg.SettingsStore.SetTheme("auto")
		}
		i.applyThemeNow("auto")
	}
	themeOptions := []settingsOption{}
	themeChoice := 0
	availableThemes := tui.AvailableThemes(i.cfg.ZutHome)
	if i.cfg.ExtensionThemes != nil {
		availableThemes = append(availableThemes, i.cfg.ExtensionThemes()...)
	}
	for idx, opt := range availableThemes {
		themeOptions = append(themeOptions, settingsOption{value: opt.Value, label: opt.Label, desc: opt.Description})
		if opt.Value == themeName {
			themeChoice = idx
		}
	}

	inputStyleOptions := []settingsOption{
		{value: tui.InputStylePlain, label: "plain", desc: "render the input as the normal prompt line"},
		{value: tui.InputStyleLines, label: "lines", desc: "draw separator lines above and below the input"},
		{value: tui.InputStyleBlock, label: "block", desc: "render the input as a user-bubble-style block"},
	}
	inputStyleChoice := 0
	for idx, opt := range inputStyleOptions {
		if opt.value == inputStyle {
			inputStyleChoice = idx
			break
		}
	}
	statusPositionOptions := []settingsOption{
		{value: tui.StatusPositionAboveInput, label: "above input", desc: "show model, usage, and working directory above the input"},
		{value: tui.StatusPositionBelowInput, label: "below input", desc: "show model, usage, and working directory below the input"},
	}
	statusPositionChoice := 0
	for idx, opt := range statusPositionOptions {
		if opt.value == statusPosition {
			statusPositionChoice = idx
			break
		}
	}

	workingPositionOptions := []settingsOption{
		{value: tui.WorkingPositionAboveInput, label: "above input", desc: "show the working spinner above the input"},
		{value: tui.WorkingPositionBelowInput, label: "below input", desc: "show the working spinner below the input"},
	}
	workingPositionChoice := 0
	for idx, opt := range workingPositionOptions {
		if opt.value == workingPosition {
			workingPositionChoice = idx
			break
		}
	}

	items := []settingsItem{
		{
			key:      "inline_images_enabled",
			label:    "render images when supported",
			desc:     "draw screenshots inline instead of showing a text placeholder",
			value:    imgEnabled,
			disabled: imgDisabled,
			hint:     imgHint,
		},
		{
			key:   "terminal_alerts_enabled",
			label: "terminal alerts",
			desc:  "ring the terminal when the main session needs attention or an extension raises an alert",
			value: terminalAlerts,
		},
		{
			key:   "terminal_title_enabled",
			label: "AI terminal titles",
			desc:  "ask the active model for a concise title after the first prompt; uses one extra hidden request",
			value: terminalTitles,
		},
		{
			key:      "auto_subagents_enabled",
			label:    "proactive delegation",
			desc:     "delegate independent sidecar work while keeping the critical path local",
			value:    autoSubagents,
			disabled: autoSubagentsDisabled,
			hint:     autoSubagentsHint,
		},
		{
			key:      "ponytail_enabled",
			label:    "ponytail coding mode",
			desc:     "apply compact coding guidance that favors small, validated, maintainable changes",
			value:    ponytailEnabled,
			disabled: ponytailDisabled,
			hint:     ponytailHint,
		},
		{
			key:      "web_search_enabled",
			label:    "web search",
			desc:     "search DuckDuckGo for bounded public-web sources; sends queries to the public backend",
			value:    webSearchEnabled,
			disabled: webSearchDisabled,
			hint:     webSearchHint,
		},
		{
			key:   "fast_mode",
			label: "fast mode",
			desc:  "request the provider's fast tier where supported; unsupported providers return an error",
			value: fastMode,
			hint:  fastModeHint,
		},
		{
			key:   "lsp_enabled",
			label: "lsp in main session",
			desc:  "enable the lsp tool and code diagnostics for the main agent",
			value: lspEnabled,
		},
		{
			key:   "subagent_lsp_enabled",
			label: "lsp in sub-agents",
			desc:  "allow newly spawned background sub-agents to use the lsp tool",
			value: subagentLSPEnabled,
		},
		{
			key:     "auto_compact_threshold",
			label:   "auto-compact threshold",
			desc:    "choose how full the model context can get before zut condenses conversation history",
			options: autoCompactOptions,
			choice:  autoCompactChoice,
		},
		{
			key:   "jail_by_default",
			label: "jail new sessions by default",
			desc:  "confine tools to the session working directory unless /unjail is used",
			value: jailByDefault,
		},
		{
			key:   "recursive_file_suggest",
			label: "recursive @-file search",
			desc:  "fuzzy-search the whole project tree when picking files with @ instead of browsing one directory at a time",
			value: recursiveFiles,
		},
		{
			key:   "respect_gitignore",
			label: "hide gitignored files in @-picker",
			desc:  "skip files and directories matched by the project's root .gitignore (and .git) when picking files with @",
			value: respectGitignore,
		},
		{
			key:   "compact_mode",
			label: "compact transcript rendering",
			desc:  "reduce visual chrome by rendering tool calls without boxes and sent messages without padded bubbles",
			value: compactMode,
		},
		{
			key:   "show_instructions_at_startup",
			label: "show loaded resources at startup",
			desc:  "list loaded context files, extensions, and user-installed skills above the transcript",
			value: showInstructions,
		},
		{
			key:   "tui_settings",
			label: "tui settings",
			desc:  "choose input chrome in its order around the prompt",
			children: []settingsItem{
				{
					key:     "tui_status_position",
					label:   "status position",
					desc:    "place model, usage, and working directory above or below the input",
					options: statusPositionOptions,
					choice:  statusPositionChoice,
				},
				{
					key:     "tui_working_position",
					label:   "working spinner position",
					desc:    "place the working spinner above or below the input",
					options: workingPositionOptions,
					choice:  workingPositionChoice,
				},
				{
					key:     "tui_input_style",
					label:   "input style",
					desc:    "choose between the plain prompt, lines, and a block input area",
					options: inputStyleOptions,
					choice:  inputStyleChoice,
				},
			},
		},
		reasoningItem,
		{
			key:     "theme",
			label:   "color theme",
			desc:    "choose terminal-owned auto, a built-in dark/light theme, or a loaded theme file",
			options: themeOptions,
			choice:  themeChoice,
		},
	}
	if len(quickItems) > 0 {
		items = append(items, settingsItem{
			key:      "quick_models",
			label:    "model shortcuts",
			desc:     "configure " + quickModelShortcutPrefix() + "+1 through " + quickModelShortcutPrefix() + "+9 quick model switches",
			children: quickItems,
		})
	}
	i.settingsDialog.Open(items)
}
func (i *Interactive) applySettingChange(act settingsAction) {
	switch {
	case strings.HasPrefix(act.Key, "quick_model_"):
		i.applyQuickModelSetting(act.Key, act.StringValue)
	case act.Key == "reasoning":
		i.applyReasoningSetting(act.StringValue)
	case act.Key == "theme":
		i.applyThemeSetting(act.StringValue)
	case act.Key == "auto_compact_threshold":
		i.applyAutoCompactThresholdSetting(act.StringValue)
	case act.Key == "tui_input_style":
		i.applyTUIInputStyleSetting(act.StringValue)
	case act.Key == "tui_status_position":
		i.applyTUIStatusPositionSetting(act.StringValue)
	case act.Key == "tui_working_position":
		i.applyTUIWorkingPositionSetting(act.StringValue)
	default:
		i.applySettingToggle(act.Key, act.Value)
	}
}
func (i *Interactive) applyAutoCompactThresholdSetting(value string) {
	threshold, err := strconv.Atoi(value)
	if err != nil {
		return
	}
	threshold = NormalizeAutoCompactThreshold(&threshold)
	if store, ok := i.cfg.SettingsStore.(autoCompactThresholdSettingsStore); ok {
		if err := store.SetAutoCompactThreshold(threshold); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.mu.Lock()
	i.cfg.AutoCompactThreshold = &threshold
	if threshold == 0 {
		i.statusOK = "auto-compact threshold off"
	} else {
		i.statusOK = fmt.Sprintf("auto-compact threshold %d%%", threshold)
	}
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
}
func (i *Interactive) quickModelSettingItems() []settingsItem {
	if len(i.cfg.QuickModelShortcuts) < 9 {
		next := make([]QuickModelShortcut, 9)
		copy(next, i.cfg.QuickModelShortcuts)
		i.cfg.QuickModelShortcuts = next
	}
	items := make([]settingsItem, 0, 9)
	for slot := 1; slot <= 9; slot++ {
		items = append(items, i.quickModelSettingItem(slot))
	}
	return items
}
func (i *Interactive) quickModelSettingItem(slot int) settingsItem {
	current := QuickModelShortcut{}
	if slot >= 1 && len(i.cfg.QuickModelShortcuts) >= slot {
		current = i.cfg.QuickModelShortcuts[slot-1]
	}
	hint := "not assigned"
	if current.Provider != "" && current.Model != "" {
		hint = current.Provider + " / " + current.Model
	}
	return settingsItem{
		key:    "quick_model_" + strconv.Itoa(slot),
		label:  "model " + strconv.Itoa(slot),
		desc:   quickModelShortcutLabel(slot) + " switches to this model. Enter opens the /model selector, Backspace clears.",
		picker: true,
		hint:   hint,
	}
}
func (i *Interactive) compactModeEnabled() bool {
	return i.cfg.CompactMode != nil && *i.cfg.CompactMode
}
func quickModelShortcutSlot(k tui.Key) int {
	if k.Kind != tui.KeyRune || k.Rune < '1' || k.Rune > '9' {
		return 0
	}
	if runtime.GOOS == "darwin" {
		if !k.Super && !k.Ctrl {
			return 0
		}
	} else if !k.Ctrl {
		return 0
	}
	return int(k.Rune - '0')
}
func quickModelShortcutPrefix() string {
	return "Ctrl"
}
func quickModelShortcutLabel(slot int) string {
	return quickModelShortcutPrefix() + "+" + strconv.Itoa(slot)
}
func (i *Interactive) openQuickModelPicker(slot int) {
	if slot < 1 || slot > 9 {
		return
	}
	i.quickModelAssign = slot
	current := i.cfg.Model
	if len(i.cfg.QuickModelShortcuts) >= slot && i.cfg.QuickModelShortcuts[slot-1].Model != "" {
		current = i.cfg.QuickModelShortcuts[slot-1].Model
	}
	var loggedIn []string
	if i.cfg.LoggedInProviders != nil {
		loggedIn = i.cfg.LoggedInProviders()
	}
	i.modelDialog.Open(current, loggedIn, i.cfg.Reasoning)
}
func (i *Interactive) applyQuickModelSelection(slot int, providerName, model string) {
	i.setQuickModelShortcut(slot, providerName, model)
}
func (i *Interactive) applyQuickModelShortcut(slot int) {
	if slot < 1 || slot > 9 {
		return
	}
	if i.busy {
		i.mu.Lock()
		i.statusErr = "cannot switch model while a turn is running"
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if len(i.cfg.QuickModelShortcuts) < slot {
		i.mu.Lock()
		i.statusErr = quickModelShortcutLabel(slot) + " is not assigned"
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	shortcut := i.cfg.QuickModelShortcuts[slot-1]
	if shortcut.Provider == "" || shortcut.Model == "" {
		i.mu.Lock()
		i.statusErr = quickModelShortcutLabel(slot) + " is not assigned"
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.swapModel(shortcut.Provider, shortcut.Model, i.cfg.BuildAgentFor, false)
	i.invalidate()
}
func (i *Interactive) applyQuickModelSetting(key, value string) {
	slotText := strings.TrimPrefix(key, "quick_model_")
	slot, err := strconv.Atoi(slotText)
	if err != nil || slot < 1 || slot > 9 {
		return
	}
	providerName, model := "", ""
	if value != "" {
		parts := strings.SplitN(value, "\t", 2)
		if len(parts) == 2 {
			providerName, model = parts[0], parts[1]
		}
	}
	i.setQuickModelShortcut(slot, providerName, model)
}
func (i *Interactive) setQuickModelShortcut(slot int, providerName, model string) {
	if len(i.cfg.QuickModelShortcuts) < slot {
		next := make([]QuickModelShortcut, slot)
		copy(next, i.cfg.QuickModelShortcuts)
		i.cfg.QuickModelShortcuts = next
	}
	i.cfg.QuickModelShortcuts[slot-1] = QuickModelShortcut{Provider: providerName, Model: model}
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetQuickModelShortcut(slot, providerName, model); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.mu.Lock()
	if model == "" {
		i.statusOK = quickModelShortcutLabel(slot) + " cleared"
	} else {
		i.statusOK = quickModelShortcutLabel(slot) + " set to " + providerName + " / " + model
	}
	i.statusErr = ""
	i.mu.Unlock()
	i.refreshQuickModelSettingsItem(slot)
	i.invalidate()
}
func (i *Interactive) refreshQuickModelSettingsItem(slot int) {
	if i.settingsDialog == nil || !i.settingsDialog.Active() || len(i.settingsDialog.items) == 0 {
		return
	}
	key := "quick_model_" + strconv.Itoa(slot)
	for idx, it := range i.settingsDialog.items {
		if it.key == key {
			i.settingsDialog.items[idx] = i.quickModelSettingItem(slot)
			return
		}
	}
}
func (i *Interactive) resetSettingsToggle(key string, value bool) {
	if i.settingsDialog == nil {
		return
	}
	for idx := range i.settingsDialog.items {
		if i.settingsDialog.items[idx].key == key {
			i.settingsDialog.items[idx].value = value
			return
		}
	}
}
func (i *Interactive) ponytailEnabled() bool {
	return i.cfg.PonytailEnabled == nil || *i.cfg.PonytailEnabled
}
func (i *Interactive) ponytailSettingsAvailable() bool {
	_, persistent := i.cfg.SettingsStore.(ponytailSettingsStore)
	return persistent && i.cfg.RefreshPrompt != nil
}
func (i *Interactive) webSearchEnabled() bool {
	return i.cfg.WebSearchEnabled == nil || *i.cfg.WebSearchEnabled
}
func (i *Interactive) webSearchToolAllowed() bool {
	return i.cfg.WebSearchToolAllowed == nil || *i.cfg.WebSearchToolAllowed
}
func (i *Interactive) webSearchEffectiveEnabled() bool {
	return i.webSearchToolAllowed() && (i.cfg.WebSearchInvocationOverride || i.webSearchEnabled())
}
func (i *Interactive) webSearchSettingsAvailable() bool {
	_, persistent := i.cfg.SettingsStore.(webSearchSettingsStore)
	return i.webSearchToolAllowed() && !i.cfg.WebSearchInvocationOverride && persistent && i.cfg.RefreshTools != nil
}
func (i *Interactive) webSearchUnavailableHint() string {
	if !i.webSearchToolAllowed() {
		return "unavailable in this session due to the launch-time tool capability ceiling"
	}
	if i.cfg.WebSearchInvocationOverride {
		return "enabled for this session by explicit --tools web_search; the persisted setting applies to invocations without --tools"
	}
	if !i.webSearchSettingsAvailable() {
		return "requires persistent settings and live tool refresh support"
	}
	return ""
}
func (i *Interactive) setWebSearchAvailable(available bool) {
	// Advance before publishing the new gate/child ceiling. A resolver that
	// captured the prior generation can no longer commit after this transition.
	// Callers may hold agentMu and i.mu; the callback must complete without
	// calling back into Interactive or requiring either lock.
	i.webSearchPolicyGeneration.Add(1)
	if i.cfg.SetWebSearchAvailable != nil {
		i.cfg.SetWebSearchAvailable(available)
	}
}
func (i *Interactive) applyWebSearchSettingToggle(key string, value bool) {
	previous := i.webSearchEnabled()
	store, persistent := i.cfg.SettingsStore.(webSearchSettingsStore)
	if !i.webSearchSettingsAvailable() || !persistent {
		i.resetSettingsToggle(key, i.webSearchEffectiveEnabled())
		i.mu.Lock()
		i.statusOK = ""
		i.statusErr = "web search unavailable: " + i.webSearchUnavailableHint()
		i.mu.Unlock()
		return
	}
	if err := store.SetWebSearchEnabled(value); err != nil {
		i.resetSettingsToggle(key, previous)
		i.mu.Lock()
		i.statusOK = ""
		i.statusErr = "settings: " + err.Error()
		i.mu.Unlock()
		return
	}
	val := value
	i.cfg.WebSearchEnabled = &val
	// Revoke and advance the policy generation for both directions. Enabling
	// remains fail-closed until the freshly resolved registry commits, while a
	// stale resolve from before this persisted transition is rejected.
	i.stripWebSearchTool()
	if err := i.cfg.RefreshTools(); err != nil {
		// Refresh failure leaves the old registry's state unknown. Revoke both
		// live execution and generic-child inheritance before rollback.
		i.stripWebSearchTool()
		errMsg := "settings: web search tool refresh: " + err.Error()
		if rollbackErr := store.SetWebSearchEnabled(previous); rollbackErr != nil {
			failClosed := false
			i.cfg.WebSearchEnabled = &failClosed
			i.resetSettingsToggle(key, false)
			errMsg += "; rollback persistence: " + rollbackErr.Error()
		} else {
			previousVal := previous
			i.cfg.WebSearchEnabled = &previousVal
			i.resetSettingsToggle(key, previous)
			if refreshErr := i.cfg.RefreshTools(); refreshErr != nil {
				errMsg += "; rollback refresh: " + refreshErr.Error()
				i.stripWebSearchTool()
			}
		}
		i.mu.Lock()
		i.statusOK = ""
		i.statusErr = errMsg
		i.mu.Unlock()
		return
	}
	i.mu.Lock()
	i.statusOK = "web search " + onOff(value)
	i.statusErr = ""
	i.mu.Unlock()
}
func (i *Interactive) stripWebSearchTool() {
	// Revoke first so a stale tool snapshotted by core cannot execute while
	// the advertised registry is being replaced.
	i.setWebSearchAvailable(false)
	i.agentMu.Lock()
	defer i.agentMu.Unlock()
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.agent == nil {
		return
	}
	tools := i.agent.ToolsSnapshot()
	hasWebCapability := false
	for _, name := range toolspkg.WebCapabilityNames {
		if _, ok := tools[name]; ok {
			hasWebCapability = true
			break
		}
	}
	if !hasWebCapability {
		return
	}
	toolspkg.RemoveWebCapabilities(tools)
	i.agent.SetTools(tools)
}
func (i *Interactive) applySettingToggle(key string, value bool) {
	// Every setting toggle forces a full repaint at the end — same
	// effect as the user pressing Ctrl+L — so any per-setting visual
	// change (image rendering, status copy, future toggles) lands
	// immediately instead of waiting for the next diff frame.
	defer func() {
		i.requestRendererClear()
		i.invalidate()
	}()
	switch key {
	case "inline_images_enabled":
		val := value
		i.cfg.InlineImagesEnabled = &val
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetInlineImages(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		i.mu.Lock()
		i.view.ImageProto = effectiveImageProtocol(i.cfg.InlineImagesEnabled)
		i.view.InvalidateRenderCache()
		i.statusOK = "inline image rendering " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "terminal_alerts_enabled":
		val := value
		i.mu.Lock()
		i.cfg.TerminalAlertsEnabled = &val
		i.mu.Unlock()
		if store, ok := i.cfg.SettingsStore.(terminalAlertsSettingsStore); ok {
			if err := store.SetTerminalAlertsEnabled(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		i.mu.Lock()
		i.statusOK = "terminal alerts " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "terminal_title_enabled":
		val := value
		var cancel context.CancelFunc
		i.mu.Lock()
		i.cfg.TerminalTitleEnabled = &val
		if value {
			i.writeTerminalTitleLocked(i.sessionTitle)
		} else {
			cancel = i.titleCancel
			i.titleCancel = nil
			i.titleVersion++
			i.writeTerminalTitleLocked("")
		}
		i.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if store, ok := i.cfg.SettingsStore.(terminalTitleSettingsStore); ok {
			if err := store.SetTerminalTitleEnabled(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		i.mu.Lock()
		i.statusOK = "AI terminal titles " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "web_search_enabled":
		apply := func() { i.applyWebSearchSettingToggle(key, value) }
		if i.cfg.SessionTransition != nil {
			i.cfg.SessionTransition(apply)
		} else {
			apply()
		}
	case "ponytail_enabled":
		previous := i.ponytailEnabled()
		store, available := i.cfg.SettingsStore.(ponytailSettingsStore)
		if !available || i.cfg.RefreshPrompt == nil {
			i.resetSettingsToggle(key, previous)
			i.mu.Lock()
			i.statusOK = ""
			i.statusErr = "ponytail coding mode unavailable: persistent settings and live prompt refresh are required"
			i.mu.Unlock()
			return
		}
		if err := store.SetPonytailEnabled(value); err != nil {
			i.resetSettingsToggle(key, previous)
			i.mu.Lock()
			i.statusOK = ""
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
		val := value
		i.cfg.PonytailEnabled = &val
		if err := i.cfg.RefreshPrompt(); err != nil {
			errMsg := "settings: ponytail prompt refresh: " + err.Error()
			if rollbackErr := store.SetPonytailEnabled(previous); rollbackErr != nil {
				// The first write succeeded, so keep the in-memory setting at
				// the value that remains durable instead of claiming that the
				// rollback took effect.
				i.resetSettingsToggle(key, value)
				errMsg += "; rollback persistence: " + rollbackErr.Error()
				if reconcileErr := i.cfg.RefreshPrompt(); reconcileErr != nil {
					errMsg += "; durable-state refresh: " + reconcileErr.Error()
				}
			} else {
				previousVal := previous
				i.cfg.PonytailEnabled = &previousVal
				i.resetSettingsToggle(key, previous)
				if refreshErr := i.cfg.RefreshPrompt(); refreshErr != nil {
					errMsg += "; rollback refresh: " + refreshErr.Error()
				}
			}
			i.mu.Lock()
			i.statusOK = ""
			i.statusErr = errMsg
			i.mu.Unlock()
			return
		}
		i.mu.Lock()
		i.statusOK = "ponytail coding mode " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "auto_subagents_enabled":
		if value && !i.autoSubagentsAvailable() {
			i.mu.Lock()
			i.statusOK = ""
			i.statusErr = "subagent orchestrator unavailable: " + i.autoSubagentsUnavailableHint()
			i.mu.Unlock()
			return
		}
		i.mu.Lock()
		previousFlag := i.cfg.AutoSubagentsEnabled
		previous := previousFlag != nil && *previousFlag
		val := value
		i.cfg.AutoSubagentsEnabled = &val
		i.mu.Unlock()
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetAutoSubagents(value); err != nil {
				i.mu.Lock()
				i.cfg.AutoSubagentsEnabled = previousFlag
				i.statusOK = ""
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				i.resetSettingsToggle(key, previous)
				return
			}
		}
		// The subagent tools remain available for explicit user requests.
		// Swap only the system-prompt guidance between strict orchestration and
		// on-demand delegation.
		i.applyAutoSubagentsTool()
		// Also swap the system-prompt addendum so the model knows whether it
		// should orchestrate proactively or delegate only on user request.
		i.applyAutoSubagentsSystemPrompt(value)
		i.mu.Lock()
		i.statusOK = "subagent orchestrator " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "fast_mode":
		previous := i.cfg.FastMode != nil && *i.cfg.FastMode
		if err := provider.ValidateFastMode(i.cfg.Provider, value); err != nil {
			i.resetSettingsToggle(key, previous)
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
		if store, ok := i.cfg.SettingsStore.(fastModeSettingsStore); ok {
			if err := store.SetFastMode(value); err != nil {
				i.resetSettingsToggle(key, previous)
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		val := value
		i.cfg.FastMode = &val
		if i.agent != nil {
			i.agent.SetFastMode(value)
		}
		i.mu.Lock()
		i.statusOK = "fast mode " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "lsp_enabled":
		previous := i.cfg.LSPEnabled == nil || *i.cfg.LSPEnabled
		store, hasStore := i.cfg.SettingsStore.(lspSettingsStore)
		if hasStore {
			if err := store.SetLSPEnabled(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		val := value
		i.cfg.LSPEnabled = &val
		if i.cfg.RefreshTools != nil {
			if err := i.cfg.RefreshTools(); err != nil {
				errMsg := "settings: refresh tools: " + err.Error()
				rollbackErr := error(nil)
				if hasStore {
					rollbackErr = store.SetLSPEnabled(previous)
				}
				if rollbackErr != nil {
					// The first write succeeded, so keep the in-memory setting at
					// the value that remains durable instead of claiming that the
					// rollback took effect.
					errMsg += "; rollback persistence: " + rollbackErr.Error()
					if reconcileErr := i.cfg.RefreshTools(); reconcileErr != nil {
						errMsg += "; durable-state refresh: " + reconcileErr.Error()
					}
				} else {
					previousVal := previous
					i.cfg.LSPEnabled = &previousVal
					if refreshErr := i.cfg.RefreshTools(); refreshErr != nil {
						errMsg += "; rollback refresh: " + refreshErr.Error()
					}
				}
				i.mu.Lock()
				i.statusOK = ""
				i.statusErr = errMsg
				i.mu.Unlock()
				return
			}
		}
		i.mu.Lock()
		i.statusOK = "lsp in main session " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "subagent_lsp_enabled":
		if store, ok := i.cfg.SettingsStore.(lspSettingsStore); ok {
			if err := store.SetSubagentLSPEnabled(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		val := value
		i.cfg.SubagentLSPEnabled = &val
		i.mu.Lock()
		i.statusOK = "lsp in sub-agents " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "jail_by_default":
		val := value
		i.cfg.JailByDefault = &val
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetJailByDefault(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		if i.cfg.Sandbox != nil {
			if value {
				i.cfg.Sandbox.Lock()
			} else {
				i.cfg.Sandbox.Unlock()
			}
		}
		i.mu.Lock()
		i.statusOK = "jail by default " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "recursive_file_suggest":
		val := value
		i.cfg.RecursiveFileSuggest = &val
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetRecursiveFileSuggest(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		// Flip the live picker so the next @ reflects the new mode
		// without restarting zut. SetRecursive drops its cache.
		i.fileSuggest.SetRecursive(value)
		i.mu.Lock()
		i.statusOK = "recursive @-file search " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "respect_gitignore":
		val := value
		i.cfg.RespectGitignore = &val
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetRespectGitignore(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		i.fileSuggest.SetRespectGitignore(value)
		i.mu.Lock()
		i.statusOK = "hide gitignored files in @-picker " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "compact_mode":
		val := value
		i.cfg.CompactMode = &val
		if i.cfg.SettingsStore != nil {
			if err := i.cfg.SettingsStore.SetCompactMode(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		i.mu.Lock()
		i.view.CompactMode = value
		i.view.InvalidateRenderCache()
		i.statusOK = "compact transcript rendering " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	case "show_instructions_at_startup":
		val := value
		i.cfg.ShowInstructionsAtStartup = &val
		if store, ok := i.cfg.SettingsStore.(showInstructionsSettingsStore); ok {
			if err := store.SetShowInstructionsAtStartup(value); err != nil {
				i.mu.Lock()
				i.statusErr = "settings: " + err.Error()
				i.mu.Unlock()
				return
			}
		}
		i.mu.Lock()
		i.view.StartupAgentName = ""
		i.view.StartupContextPaths = nil
		i.view.StartupExtensionNames = nil
		i.view.StartupSkillNames = nil
		i.view.InvalidateRenderCache()
		if value {
			i.view.StartupAgentName = i.cfg.StartupAgentName
			i.view.StartupContextPaths = append(i.view.StartupContextPaths, i.cfg.StartupContextPaths...)
			i.view.StartupExtensionNames = append(i.view.StartupExtensionNames, i.cfg.StartupExtensionNames...)
			i.view.StartupSkillNames = append(i.view.StartupSkillNames, i.cfg.StartupSkillNames...)
		}
		i.statusOK = "show loaded resources at startup " + onOff(value)
		i.statusErr = ""
		i.mu.Unlock()
	}
}
func (i *Interactive) applyTUIInputStyleSetting(style string) {
	defer func() {
		i.requestRendererClear()
		i.invalidate()
	}()
	style = tui.NormalizeInputStyle(style)
	i.cfg.TUIInputStyle = style
	i.applyInputCursorColor()
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetTUIInputStyle(style); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.mu.Lock()
	i.statusOK = "input style " + style
	i.statusErr = ""
	i.mu.Unlock()
}
func (i *Interactive) applyTUIStatusPositionSetting(position string) {
	defer func() {
		i.requestRendererClear()
		i.invalidate()
	}()
	position = tui.NormalizeStatusPosition(position)
	i.cfg.TUIStatusPosition = position
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetTUIStatusPosition(position); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.mu.Lock()
	label := "above input"
	if position == tui.StatusPositionBelowInput {
		label = "below input"
	}
	i.statusOK = "status position " + label
	i.statusErr = ""
	i.mu.Unlock()
}
func (i *Interactive) applyTUIWorkingPositionSetting(position string) {
	defer func() {
		i.requestRendererClear()
		i.invalidate()
	}()
	position = tui.NormalizeWorkingPosition(position)
	i.cfg.TUIWorkingPosition = position
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetTUIWorkingPosition(position); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.mu.Lock()
	label := "above input"
	if position == tui.WorkingPositionBelowInput {
		label = "below input"
	}
	i.statusOK = "working spinner position " + label
	i.statusErr = ""
	i.mu.Unlock()
}
func (i *Interactive) applyThemeSetting(name string) {
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetTheme(name); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	i.cfg.ThemeName = name
	if name == "auto" {
		i.cfg.ThemeName = ""
	}
	if !i.cfg.ThemeForced {
		i.cfg.EffectiveThemeName = name
	}
	i.applyThemeNow(i.cfg.EffectiveThemeName)
}

func (i *Interactive) applyThemeNow(name string) {
	if name == "" {
		name = "auto"
	}
	source, err := tui.LoadThemeSource(i.cfg.ZutHome, name)
	if err != nil {
		if i.cfg.SettingsStore != nil {
			_ = i.cfg.SettingsStore.SetTheme("auto")
		}
		i.cfg.ThemeName = ""
		i.cfg.EffectiveThemeName = "auto"
		i.activeThemeSource = nil
		i.stopThemeWatch()
		i.installResolvedTheme(tui.ResolveTheme("auto", nil, i.terminalProfile))
		i.mu.Lock()
		i.statusErr = "theme missing; reset to auto"
		i.mu.Unlock()
		return
	}
	i.activeThemeSource = source
	i.restartThemeWatch()
	i.installResolvedTheme(tui.ResolveTheme(name, source, i.terminalProfile))
	i.mu.Lock()
	i.statusOK = "theme " + i.cfg.EffectiveThemeName
	if i.cfg.ThemeForced {
		i.statusOK += " (forced by ZUT_THEME)"
	}
	i.statusErr = ""
	i.mu.Unlock()
}

// installResolvedTheme is the one main-loop-owned installation boundary for
// settings, terminal profile, and accepted theme-file revisions.
// applyTerminalProfile records detected appearance independently from the
// currently applied theme. Forced and custom selections are re-resolved from
// their immutable accepted source; no terminal event performs filesystem I/O.
func (i *Interactive) applyTerminalProfile(profile tui.TerminalProfile) {
	i.terminalProfile = profile
	i.cfg.TerminalProfile = profile
	resolution := tui.ResolveTheme(i.cfg.EffectiveThemeName, i.activeThemeSource, profile)
	i.installResolvedTheme(resolution)
}

func (i *Interactive) installResolvedTheme(resolution tui.ThemeResolution) {
	th := resolution.Theme
	i.mu.Lock()
	i.cfg.Theme = th
	i.view.Theme = th
	i.view.InvalidateRenderCache()
	i.chatCacheValid = false
	i.stableChatCacheValid = false
	i.ed.Prompt = th.AccentBar(th.Accent)
	i.mu.Unlock()
	i.btwDialog.setTheme(th)
	if i.residentChildSession != nil {
		i.residentChildSession.setTheme(th)
	}
	i.applyInputCursorColor()
	i.spin.Configure(th)
	i.requestRendererTheme(th)
	i.invalidate()
}
func (i *Interactive) applyReasoningSetting(level string) {
	defer func() {
		i.requestRendererClear()
		i.invalidate()
	}()
	level = provider.NormalizeReasoning(level)
	i.cfg.Reasoning = level
	if i.cfg.SettingsStore != nil {
		if err := i.cfg.SettingsStore.SetReasoning(level); err != nil {
			i.mu.Lock()
			i.statusErr = "settings: " + err.Error()
			i.mu.Unlock()
			return
		}
	}
	if i.cfg.OnReasoningChanged != nil {
		i.cfg.OnReasoningChanged(level)
	}
	i.mu.Lock()
	if i.agent != nil {
		i.agent.Reasoning = level
	}
	label := level
	if label == "" {
		label = "off"
	}
	i.statusOK = "reasoning level " + label
	i.statusErr = ""
	i.mu.Unlock()
}
