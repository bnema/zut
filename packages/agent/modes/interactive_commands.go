package modes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/auth"
	"github.com/bnema/zut/packages/tui"
)

func buildStudyPrompt(arg, cwd string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "Read and understand everything in the current directory."
	}
	abs := arg
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, abs)
	}
	display := arg
	if rel, err := filepath.Rel(cwd, abs); err == nil && !strings.HasPrefix(rel, "..") {
		display = rel
	}
	if info, err := os.Stat(abs); err == nil && !info.IsDir() {
		return "Read and understand the file " + display + "."
	}
	return "Read and understand everything in the directory " + display + "."
}
func tryPathTabCompleteEditor(ed *tui.Editor, cwd string) bool {
	if ed == nil {
		return false
	}
	val := ed.Value()
	// Find the trailing run of non-whitespace.
	start := len(val)
	for start > 0 {
		r := val[start-1]
		if r == ' ' || r == '\t' || r == '\n' {
			break
		}
		start--
	}
	token := val[start:]
	if token == "" {
		return false
	}
	if !looksLikePathToken(token) {
		return false
	}

	// Resolve the absolute parent directory + base prefix to match.
	parentAbs, basePrefix, displayParent, ok := resolvePathTabToken(token, cwd)
	if !ok {
		return true
	}
	entries, err := os.ReadDir(parentAbs)
	if err != nil {
		return true
	}
	var names []string
	var isDir []bool
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, basePrefix) {
			continue
		}
		// Hide dotfiles unless the user explicitly typed a leading dot,
		// mirroring bash's default behaviour.
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(basePrefix, ".") {
			continue
		}
		names = append(names, name)
		isDir = append(isDir, e.IsDir())
	}
	if len(names) == 0 {
		return true
	}

	var completed string
	var completedIsDir bool
	if len(names) == 1 {
		completed = names[0]
		completedIsDir = isDir[0]
	} else {
		completed = longestCommonPrefix(names)
		if completed == basePrefix {
			// Already at the deepest unambiguous prefix; nothing to add.
			return true
		}
	}

	// Build the replacement token in the same display form the user
	// typed (preserve ~ vs absolute vs relative).
	newToken := displayParent + completed
	if len(names) == 1 && completedIsDir {
		newToken += "/"
	}

	ed.SetValue(val[:start] + newToken)
	return true
}
func (i *Interactive) tryPathTabComplete() bool {
	if tryPathTabCompleteEditor(i.ed, i.cfg.CWD) {
		i.invalidate()
		return true
	}
	return false
}
func looksLikePathToken(tok string) bool {
	if tok == "" {
		return false
	}
	if tok[0] == '~' || tok[0] == '/' {
		return true
	}
	if strings.HasPrefix(tok, "./") || strings.HasPrefix(tok, "../") {
		return true
	}
	return strings.Contains(tok, "/")
}
func resolvePathTabToken(tok, cwd string) (parentAbs, basePrefix, displayParent string, ok bool) {
	// Detect ~ expansion.
	expanded := tok
	homePrefix := ""
	if tok == "~" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", "", "", false
		}
		// "~" alone: complete in $HOME. parent = home, base = "".
		return home, "", "~/", true
	}
	if strings.HasPrefix(tok, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", "", "", false
		}
		expanded = home + tok[1:]
		homePrefix = "~"
	}

	dir, base := splitDirBase(expanded)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}

	// Reconstruct the display form the user typed for the parent,
	// keeping ~ when they used it. The base is dropped — the caller
	// substitutes the completed name.
	display := tok[:len(tok)-len(base)]
	if homePrefix != "" && !strings.HasPrefix(display, "~") {
		display = homePrefix + display[len(homePrefix):]
	}
	return dir, base, display, true
}
func splitDirBase(p string) (dir, base string) {
	if p == "" {
		return ".", ""
	}
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ".", p
	}
	return p[:i+1], p[i+1:]
}
func longestCommonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	prefix := ss[0]
	for _, s := range ss[1:] {
		n := 0
		for n < len(prefix) && n < len(s) && prefix[n] == s[n] {
			n++
		}
		prefix = prefix[:n]
		if prefix == "" {
			return ""
		}
	}
	return prefix
}
func (i *Interactive) runSlash(ctx context.Context, cmd string) (done bool) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return false
	}

	if isSkillCommand(cmd) {
		var available []*skills.Skill
		if i.cfg.SkillSnapshot != nil {
			available = i.cfg.SkillSnapshot()
		}
		prompt, _, err := expandSkillCommand(cmd, available)
		if err != nil {
			i.mu.Lock()
			i.statusErr = err.Error()
			i.statusOK = ""
			i.mu.Unlock()
			return false
		}
		i.submitOrQueuePrompt(ctx, prompt)
		return false
	}

	switch strings.ToLower(parts[0]) {
	case "/goal":
		i.runGoalCommand(ctx, cmd, parts)
	case "/exit":
		return true
	case "/clear":
		if i.agent != nil {
			i.agent.SetMessages(nil)
		}
		i.mu.Lock()
		i.toolCalls = map[string]*tui.ToolCallView{}
		i.toolOrder = nil
		i.toolGate = map[string]int{}
		i.statusErr = ""
		i.statusOK = ""
		i.helpBlock = nil
		i.sessionInfoBlocks = nil
		i.parkedTurn = 0
		i.parkedTotal = 0
		i.scrollOffset = 0
		i.extNotes = nil
		i.reloadErrors = nil
		handoff, persistHandoff := i.resetCompactContinuationLocked()
		i.view.InvalidateRenderCache()
		i.mu.Unlock()
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
	case "/help":
		i.mu.Lock()
		i.helpBlock = renderHelpBlock(i.cfg.Theme, i.lastCols(), i.llamaConfigured)
		i.statusErr = ""
		i.statusOK = ""
		// Pin the viewport to the newest content so the help block,
		// which we just appended to the end of the transcript, is
		// what the user actually sees.
		i.scrollOffset = 0
		i.mu.Unlock()
	case "/info":
		i.showSessionInfo()
	case "/login":
		i.dialog.Open(i.cfg.ZutHome)
	case "/logout":
		if len(parts) >= 2 {
			// Explicit target: /logout anthropic | openai | all
			i.doLogout(parts[1])
			break
		}
		// No arg: open the picker over whichever providers are
		// currently logged in. If nothing's logged in, bail with a
		// status line.
		i.openLogoutDialog()
	case "/model":
		if len(parts) >= 2 {
			i.applyModelSelection("", parts[1])
		} else if i.llamaConfigured && i.cfg.RefreshLlamaCPPModels != nil {
			i.mu.Lock()
			i.modelRefreshing = true
			i.statusOK = "Refreshing models"
			i.statusErr = ""
			i.mu.Unlock()
			go func() {
				refreshCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
				i.modelRefresh <- modelRefreshResult{err: i.cfg.RefreshLlamaCPPModels(refreshCtx)}
			}()
		} else {
			i.openModelPickerAfterRefresh(nil)
		}
	case "/llama":
		if i.cfg.LlamaCPPConfig == nil {
			i.mu.Lock()
			i.statusErr = "llama.cpp configuration is unavailable"
			i.statusOK = ""
			i.mu.Unlock()
			break
		}
		baseURL, apiKey, configErr := i.cfg.LlamaCPPConfig()
		if configErr == nil {
			configErr = i.llamaDialog.Open(baseURL, apiKey, i.invalidate)
		}
		if configErr != nil {
			i.mu.Lock()
			i.statusErr = configErr.Error()
			i.statusOK = ""
			i.mu.Unlock()
		}
	case "/reasoning":
		i.openReasoningDialog()
	case "/fast":
		enabled := i.cfg.FastMode == nil || !*i.cfg.FastMode
		i.applySettingToggle("fast_mode", enabled)
	case "/orchestrator":
		enabled := i.cfg.AutoSubagentsEnabled == nil || !*i.cfg.AutoSubagentsEnabled
		i.applySettingToggle("auto_subagents_enabled", enabled)
	case "/settings":
		i.openSettingsDialog()
	case "/sessions":
		i.mu.Lock()
		i.sessionLoads = i.sessionDialog.Open(ctx, i.sessionsRoot(), i.cfg.CWD)
		i.mu.Unlock()
	case "/fork":
		i.doSessionFork()
	case "/jump":
		i.openJumpDialog(parts[1:])
	case "/btw":
		i.openBtwDialog(parts[1:])
	case "/skills":
		i.openSkillsDialog()
	case "/compact":
		i.runCompact(ctx, compactContinuationRequest{origin: compactOriginManual})
	case "/study":
		// Canned prompt that tells the agent to read every file
		// in some target so its later turns have the whole thing
		// in context. With no argument, the target is the current
		// directory. With an argument, the target is whatever the
		// user passed — typed by hand, drag-dropped, or selected
		// via the @ file picker (which is why we accept both files
		// and directories; the @-picker chips for both have already
		// been expanded to absolute paths by expandFileChips above).
		// Dispatched through the normal queue-or-start path so it
		// behaves identically to typing the prompt by hand.
		studyPrompt := buildStudyPrompt(strings.TrimSpace(strings.TrimPrefix(cmd, parts[0])), i.cfg.CWD)
		i.mu.Lock()
		busy := i.busy
		compacting := i.compacting
		ag := i.agent
		i.mu.Unlock()
		if busy {
			var handoff json.RawMessage
			var persistHandoff bool
			if ag != nil && !compacting {
				i.mu.Lock()
				handoff, persistHandoff = i.resetCompactContinuationLocked()
				ag.QueueMessage(studyPrompt)
				i.mu.Unlock()
			} else {
				i.mu.Lock()
				handoff, persistHandoff = i.resetCompactContinuationLocked()
				i.queued = append(i.queued, studyPrompt)
				i.mu.Unlock()
			}
			if persistHandoff {
				i.persistCompactHandoff(handoff)
			}
			i.invalidate()
			break
		}
		i.startTurn(ctx, studyPrompt)
	case "/cd":
		// Hidden command: switch the running session's cwd. Not in
		// slash_suggest, not in /help. Used by the workspaces
		// extension's panel-key Enter handler so picking a row
		// jumps zut into that directory without relaunching.
		//
		// Recovers the raw argument (path) from the original cmd
		// string rather than parts, so paths with spaces survive.
		// The host's ChangeCWD hook handles validation, session
		// close + reopen, agent rebuild, sandbox re-rooting, and
		// re-jail-if-jailed semantics.
		if i.cfg.ChangeCWD == nil {
			i.mu.Lock()
			i.statusErr = "/cd unavailable: host did not wire ChangeCWD"
			i.mu.Unlock()
			break
		}
		path := strings.TrimSpace(strings.TrimPrefix(cmd, parts[0]))
		if path == "" {
			i.mu.Lock()
			i.statusErr = "/cd: missing path"
			i.mu.Unlock()
			break
		}
		if err := i.cfg.ChangeCWD(path); err != nil {
			i.mu.Lock()
			i.statusErr = "/cd: " + err.Error()
			i.statusOK = ""
			i.mu.Unlock()
			break
		}
		// ChangeCWD has already updated i.cfg.CWD and swapped the
		// agent + session. Reset transient TUI state so the new
		// session opens clean.
		i.mu.Lock()
		i.toolCalls = map[string]*tui.ToolCallView{}
		i.toolOrder = nil
		i.toolGate = map[string]int{}
		i.helpBlock = nil
		i.sessionInfoBlocks = nil
		i.parkedTurn = 0
		i.statusOK = "cwd " + i.cfg.CWD
		i.statusErr = ""
		i.mu.Unlock()
		i.fileSuggest.Reset()
		i.fileSuggest.SetCWD(i.cfg.CWD)
		i.invalidate()
	case "/jail":
		if i.cfg.Sandbox == nil {
			i.mu.Lock()
			i.statusErr = "sandbox not available in this build"
			i.mu.Unlock()
			break
		}
		i.cfg.Sandbox.Lock()
		i.mu.Lock()
		i.statusOK = "jailed to " + i.cfg.CWD + " (tools cannot touch paths outside this directory)"
		i.statusErr = ""
		i.mu.Unlock()
	case "/unjail":
		if i.cfg.Sandbox == nil {
			i.mu.Lock()
			i.statusErr = "sandbox not available in this build"
			i.mu.Unlock()
			break
		}
		i.cfg.Sandbox.Unlock()
		i.mu.Lock()
		i.statusOK = "unjailed"
		i.statusErr = ""
		i.mu.Unlock()
	case "/reload-ext":
		i.runReloadExt(ctx)
	case "/telegram", "/tg":
		if len(parts) >= 2 {
			i.doTelegram(parts[1])
			break
		}
		i.openTelegramDialog()
	case "/session":
		if len(parts) >= 2 {
			action := parts[1]
			arg := ""
			if len(parts) >= 3 {
				arg = strings.Join(parts[2:], " ")
			}
			i.doSessionOp(action, arg)
			break
		}
		i.openSessionOpsDialog()
	case "/subagents":
		i.runSubagents(ctx, parts[1:])
	default:
		// Last-resort fallback: try the extension manager. Built-in
		// cases above always win; this branch only fires for slash
		// commands the extension manager registered. Same routing as
		// the editor's submit-handler dispatch path so the autocomplete
		// "enter on highlighted suggestion" flow also works.
		extName := strings.TrimPrefix(parts[0], "/")
		if i.cfg.Extensions != nil && i.cfg.Extensions.HasCommand(extName) {
			rest := ""
			if len(parts) > 1 {
				rest = strings.Join(parts[1:], " ")
			}
			go i.invokeExtensionCommand(ctx, extName, rest)
			return false
		}
		i.mu.Lock()
		i.statusErr = "unknown command: " + parts[0]
		i.mu.Unlock()
	}
	return false
}
func (i *Interactive) openModelPickerAfterRefresh(refreshErr error) {
	var loggedIn []string
	if i.cfg.LoggedInProviders != nil {
		loggedIn = i.cfg.LoggedInProviders()
	}
	i.modelDialog.Open(i.cfg.Model, loggedIn, i.cfg.Reasoning)
	i.mu.Lock()
	if refreshErr != nil {
		i.statusErr = "llama.cpp model refresh: " + refreshErr.Error()
		i.statusOK = ""
	} else if i.statusOK == "Refreshing models" {
		i.statusOK = ""
	}
	i.mu.Unlock()
}
func (i *Interactive) openLogoutDialog() {
	if i.cfg.AuthManager == nil {
		i.mu.Lock()
		i.statusErr = "no auth manager configured"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	store := i.cfg.AuthManager.Store()
	if store == nil {
		i.mu.Lock()
		i.statusErr = "auth store is not available"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	creds, err := store.Load()
	if err != nil {
		i.mu.Lock()
		i.statusErr = "read auth store: " + err.Error()
		i.mu.Unlock()
		i.invalidate()
		return
	}

	var items []logoutItem
	for _, p := range []string{"anthropic", "kimi", "google", "github-copilot"} {
		if creds.Has(p) {
			method := creds.Method(p)
			if method == "oauth" {
				method = "subscription"
			}
			items = append(items, logoutItem{
				label:  providerLabel(p),
				target: p,
				method: method,
			})
		}
	}
	if creds.OpenAI.APIKey != "" || creds.OpenAI.APIKeyCommand != nil {
		items = append(items, logoutItem{label: providerLabel("openai"), target: "openai", method: "api key"})
	}
	if creds.OpenAI.OAuth != nil {
		items = append(items, logoutItem{label: providerLabel("openai-codex"), target: "openai-codex", method: "subscription"})
	}
	for p, c := range creds.AdditionalAPIKeyCreds {
		if c.APIKey != "" || c.APIKeyCommand != nil || c.BaseURL != "" {
			items = append(items, logoutItem{label: providerLabel(p), target: p, method: "api key"})
		}
	}
	if len(items) == 0 {
		i.mu.Lock()
		i.statusOK = "no credentials stored; already logged out"
		i.statusErr = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if len(items) > 1 {
		items = append(items, logoutItem{label: "all", target: "all"})
	}

	i.logoutDialog.Open(items)
	i.invalidate()
}
func (i *Interactive) doLogout(target string) {
	if i.cfg.AuthManager == nil {
		i.mu.Lock()
		i.statusErr = "no auth manager configured"
		i.mu.Unlock()
		return
	}
	store := i.cfg.AuthManager.Store()
	if store == nil {
		i.mu.Lock()
		i.statusErr = "auth store is not available"
		i.mu.Unlock()
		return
	}

	var providers []string
	switch target {
	case "", "all":
		providers = append([]string{"anthropic", "openai", "openai-codex", "kimi", "google", "github-copilot"}, auth.APIKeyProviders()...)
	case "anthropic", "openai", "openai-codex", "kimi", "google", "github-copilot":
		providers = []string{target}
	default:
		known := false
		for _, p := range auth.APIKeyProviders() {
			if target == p {
				known = true
				break
			}
		}
		if !known {
			i.mu.Lock()
			i.statusErr = "unknown provider: " + target
			i.mu.Unlock()
			return
		}
		providers = []string{target}
	}

	var errs []string
	clearedCurrent := false
	for _, p := range providers {
		var err error
		switch p {
		case "openai":
			err = store.ClearAPIKey("openai")
		case "openai-codex":
			err = store.ClearOAuth("openai")
		default:
			err = store.Clear(p)
		}
		if err != nil {
			errs = append(errs, p+": "+err.Error())
			continue
		}
		if p == "kimi" && i.cfg.SetKimiCLIFallbackDisabled != nil {
			if err := i.cfg.SetKimiCLIFallbackDisabled(true); err != nil {
				errs = append(errs, p+": "+err.Error())
				continue
			}
		}
		if p == i.cfg.Provider {
			clearedCurrent = true
		}
	}

	llamaConfigured := i.llamaConfigured
	if i.cfg.LlamaCPPConfig != nil {
		baseURL, _, err := i.cfg.LlamaCPPConfig()
		llamaConfigured = err == nil && baseURL != ""
	}

	i.mu.Lock()
	i.llamaConfigured = llamaConfigured
	if len(errs) > 0 {
		i.statusErr = "logout errors: " + strings.Join(errs, "; ")
		i.mu.Unlock()
		return
	}
	i.statusErr = ""
	var handoff json.RawMessage
	var persistHandoff bool
	if clearedCurrent {
		// The running agent was using a credential we just wiped. Drop
		// it so prompts can't go out with the stale client, and hint at
		// /login. Close any LSP processes owned by its tools first.
		if i.agent != nil {
			_ = tools.CloseLSPManagers(i.agent.ToolsSnapshot())
		}
		i.agent = nil
		handoff, persistHandoff = i.resetCompactContinuationLocked()
		i.statusOK = "logged out of " + strings.Join(providers, ", ") + ". type /login to sign back in."
	} else {
		i.statusOK = "logged out of " + strings.Join(providers, ", ")
	}
	i.mu.Unlock()
	if persistHandoff {
		i.persistCompactHandoff(handoff)
	}
}
func providerSetupInfo(provider string) (string, []string, bool) {
	const docsURL = "https://raw.githubusercontent.com/bnema/zut/main/docs/providers.md"
	switch provider {
	case "amazon-bedrock":
		return "Amazon Bedrock setup", []string{
			"Amazon Bedrock uses AWS credentials instead of a generic zut API-key entry.",
			"Configure an AWS profile, IAM keys, bearer token, or role-based credentials.",
			"",
			"For Bedrock API keys, set:",
			"  AWS_BEARER_TOKEN_BEDROCK=...",
			"  AWS_REGION=us-east-1",
			"",
			"Docs:",
			"  " + docsURL,
		}, true
	case "google-vertex":
		return "Google Vertex AI setup", []string{
			"Google Vertex AI usually uses Google Cloud credentials and project settings.",
			"Set a Google API key, application-default credentials, or a service account.",
			"",
			"Common environment:",
			"  GOOGLE_CLOUD_API_KEY=...",
			"  GOOGLE_CLOUD_PROJECT=...",
			"  GOOGLE_CLOUD_LOCATION=us-central1",
			"",
			"Docs:",
			"  " + docsURL,
		}, true
	case "cloudflare-workers-ai":
		return "Cloudflare Workers AI setup", []string{
			"Cloudflare Workers AI needs both an API token and an account ID.",
			"",
			"Set:",
			"  CLOUDFLARE_API_KEY=...",
			"  CLOUDFLARE_ACCOUNT_ID=...",
			"",
			"Docs:",
			"  " + docsURL,
		}, true
	case "cloudflare-ai-gateway":
		return "Cloudflare AI Gateway setup", []string{
			"Cloudflare AI Gateway needs an API token, account ID, and gateway ID.",
			"",
			"Set:",
			"  CLOUDFLARE_API_KEY=...",
			"  CLOUDFLARE_ACCOUNT_ID=...",
			"  CLOUDFLARE_GATEWAY_ID=...",
			"",
			"Docs:",
			"  " + docsURL,
		}, true
	case "azure-openai-responses":
		return "Azure OpenAI Responses setup", []string{
			"Azure OpenAI needs an API key plus your Azure endpoint or deployment setup.",
			"",
			"Set:",
			"  AZURE_OPENAI_API_KEY=...",
			"  AZURE_OPENAI_BASE_URL=https://your-resource.openai.azure.com",
			"  AZURE_OPENAI_API_VERSION=2024-02-01",
			"",
			"Docs:",
			"  " + docsURL,
		}, true
	default:
		return "", nil, false
	}
}
func (i *Interactive) saveLlamaCPPLogin(baseURL, apiKey string) {
	if i.cfg.AuthManager == nil || i.cfg.AuthManager.Store() == nil {
		i.dialog.ShowResult(false, "auth store is unavailable")
		return
	}
	if err := i.cfg.AuthManager.Store().SetEndpointCredential(provider.LlamaCPPProviderID, baseURL, apiKey); err != nil {
		i.dialog.ShowResult(false, err.Error())
		return
	}
	i.mu.Lock()
	i.llamaConfigured = true
	i.statusErr = ""
	i.statusOK = "configured llama.cpp router"
	i.mu.Unlock()
	i.dialog.ShowResult(true, "")
}
func (i *Interactive) startAPIKeyFlow(provider string) {
	if title, lines, ok := providerSetupInfo(provider); ok {
		i.dialog.ShowInfo(title, lines)
		return
	}
	if provider == "kimi" && i.cfg.SetKimiCLIFallbackDisabled != nil {
		_ = i.cfg.SetKimiCLIFallbackDisabled(false)
	}
	url, err := i.cfg.AuthManager.StartAPIKey(provider)
	if err != nil {
		i.dialog.ShowResult(false, err.Error())
		return
	}
	i.dialog.ShowWaiting(url)
}
func (i *Interactive) startOAuthFlow(provider string) {
	if provider == "kimi" && i.cfg.SetKimiCLIFallbackDisabled != nil {
		_ = i.cfg.SetKimiCLIFallbackDisabled(false)
	}
	// Device-code providers already support headless login and must only
	// start one polling flow.
	if provider == "kimi" || provider == "xai" || provider == "github-copilot" {
		loginURL, err := i.cfg.AuthManager.StartOAuth(provider)
		if err != nil {
			i.dialog.ShowResult(false, err.Error())
			return
		}
		i.dialog.ShowWaiting(loginURL)
		return
	}
	// Always run the manual/copy-code flow in parallel with the local
	// callback server so headless environments (docker, SSH) can paste
	// the authorization code directly without first pressing 'p'.
	_, err := i.cfg.AuthManager.StartOAuth(provider)
	if err != nil {
		i.dialog.ShowResult(false, err.Error())
		return
	}
	manualURL, mErr := i.cfg.AuthManager.StartManualOAuth(provider)
	if mErr == nil {
		i.dialog.ShowWaiting(manualURL)
	} else {
		i.dialog.ShowResult(false, mErr.Error())
	}
}
func (i *Interactive) startManualOAuthFlow(provider string) {
	if i.cfg.AuthManager == nil {
		return
	}
	i.cfg.AuthManager.CancelOAuth()
	url, err := i.cfg.AuthManager.StartManualOAuth(provider)
	if err != nil {
		i.dialog.ShowResult(false, err.Error())
		return
	}
	i.dialog.url = url
	i.invalidate()
}
func (i *Interactive) submitManualOAuthCode(code string) {
	if i.cfg.AuthManager == nil {
		return
	}
	go func() {
		if err := i.cfg.AuthManager.CompleteManualOAuth(i.runCtx, code); err != nil {
			i.dialog.ShowResult(false, err.Error())
			i.invalidate()
		}
	}()
}
func (i *Interactive) cancelAndWaitForIdle() {
	i.mu.Lock()
	busy := i.busy
	cancel := i.cancelTurn
	i.mu.Unlock()
	if !busy {
		return
	}
	if cancel != nil {
		i.mu.Lock()
		handoff, persistHandoff := i.resetCompactContinuationLocked()
		i.mu.Unlock()
		cancel()
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
	}
	// ConfirmToolCall waits on its response channel rather than the turn
	// context, so cancellation must also resolve every pending prompt.
	i.confirmDialog.CancelAll("turn cancelled")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		i.mu.Lock()
		done := !i.busy
		i.mu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func (i *Interactive) openBtwDialog(args []string) {
	i.agentMu.Lock()
	defer i.agentMu.Unlock()
	i.mu.Lock()
	ag := i.agent
	system := ""
	if ag != nil {
		system, _ = ag.PromptConfig()
	}
	theme := i.cfg.Theme
	model := i.cfg.Model
	cwd := i.cfg.CWD
	compact := i.compactModeEnabled()
	flatTools := i.cfg.FlatTools
	lineInput := tui.NormalizeInputStyle(i.cfg.TUIInputStyle) == tui.InputStyleLines
	i.mu.Unlock()
	if ag == nil {
		i.mu.Lock()
		i.statusErr = "not logged in. type /login first."
		i.mu.Unlock()
		return
	}
	seed := strings.TrimSpace(strings.Join(args, " "))
	i.btwDialog.Open(theme, ag, system, model, cwd, seed, compact, flatTools, lineInput, i.invalidate)
	i.invalidate()
}
func (i *Interactive) submitOrQueuePrompt(ctx context.Context, prompt string) {
	i.mu.Lock()
	if i.agent == nil {
		i.statusErr = "not logged in. type /login first."
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if i.busy {
		// Keep the mutex held while enqueueing so the turn-completion
		// goroutine cannot publish idle and inspect an empty queue between
		// this check and the append.
		var handoff json.RawMessage
		var persistHandoff bool
		if i.compacting {
			handoff, persistHandoff = i.resetCompactContinuationLocked()
			i.queued = append(i.queued, prompt)
		} else {
			handoff, persistHandoff = i.resetCompactContinuationLocked()
			i.agent.QueueMessage(prompt)
		}
		i.mu.Unlock()
		if persistHandoff {
			i.persistCompactHandoff(handoff)
		}
		i.invalidate()
		return
	}
	i.mu.Unlock()
	i.startTurn(ctx, prompt)
}
func (i *Interactive) openSkillsDialog() {
	var list []*skills.Skill
	if i.cfg.SkillSnapshot != nil {
		list = i.cfg.SkillSnapshot()
	}
	i.skillsDialog.Open(list)
	i.invalidate()
}
func (i *Interactive) openJumpDialog(args []string) {
	if i.view == nil || len(i.view.Messages) == 0 {
		i.mu.Lock()
		i.statusErr = "nothing to jump to \u2014 the session is empty"
		i.mu.Unlock()
		return
	}
	filter := strings.TrimSpace(strings.Join(args, " "))
	i.jumpDialog.Open(i.view.Messages, filter)
	// Shortcut: with a filter argument that matches exactly one turn,
	// jump immediately and skip the picker.
	if filter != "" {
		if tgts := i.jumpDialog.Targets(); len(tgts) == 1 {
			t := tgts[0]
			i.jumpDialog.Close()
			i.applyJumpSelection(t.MessageIdx, t.TurnNo)
		}
	}
}
func (i *Interactive) applyJumpSelection(msgIdx, turnNo int) {
	cols := i.lastCols()
	chat, anchors := i.view.BuildWithAnchors(cols)
	var row int
	found := false
	for _, a := range anchors {
		if a.MessageIdx == msgIdx {
			row = a.Row
			found = true
			break
		}
	}
	if !found {
		i.mu.Lock()
		i.statusErr = "could not resolve jump target"
		i.mu.Unlock()
		return
	}

	chatLen := len(chat)
	page := i.chatPage()
	if page < 1 {
		page = 1
	}
	// scrollOffset is measured from the bottom of the chat slice, so
	// to place `row` at the top of the viewport we want:
	//     chatLen - scrollOffset - page == row
	// Solve for scrollOffset and clamp to [0, chatLen-page].
	offset := chatLen - (row + page)
	if offset < 0 {
		offset = 0
	}
	maxOffset := chatLen - page
	if maxOffset < 0 {
		maxOffset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}

	i.mu.Lock()
	i.scrollOffset = offset
	i.parkedTurn = turnNo
	i.parkedTotal = totalTurnsLocked(i.view.Messages)
	i.statusOK = fmt.Sprintf("jumped to turn %d", turnNo)
	i.statusErr = ""
	i.mu.Unlock()
}
func totalTurnsLocked(msgs []provider.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == provider.RoleUser {
			n++
		}
	}
	return n
}
func (i *Interactive) applySessionSelection(path string) {
	if i.cfg.LoadSession == nil {
		i.mu.Lock()
		i.statusErr = "session loading is not wired in this build"
		i.mu.Unlock()
		return
	}
	i.mu.Lock()
	if i.sessionLoading {
		i.statusErr = "already resuming a session"
		i.mu.Unlock()
		i.invalidate()
		return
	}
	i.sessionLoading = true
	i.statusOK = "resuming session: " + path
	i.statusErr = ""
	i.mu.Unlock()
	i.invalidate()
	i.markSessionTitleSwitching()

	go func() {
		err := i.cfg.LoadSession(path)
		if err != nil {
			i.restoreFailedSessionTitle()
			i.mu.Lock()
			i.sessionLoading = false
			i.statusErr = err.Error()
			i.statusOK = ""
			i.mu.Unlock()
			i.invalidate()
			return
		}
		i.restoreLoadedSessionTitle()
		state := decodeCompactHandoff(i.currentCompactHandoff())
		i.mu.Lock()
		i.compactContinuation = state
		i.sessionLoading = false
		i.statusOK = "resumed session: " + path
		i.statusErr = ""
		i.parkedTurn = 0
		i.parkedTotal = 0
		i.scrollOffset = 0
		// Fresh transcript swapped in: drop the auto-follow baseline so
		// the next render's follow guard doesn't see the wholesale
		// length change as a delta and jump the viewport.
		i.prevChatLen = 0
		i.prevChatCols = 0
		i.extNotes = nil
		i.view.InvalidateRenderCache()
		if i.agent != nil {
			i.view.Messages = filterHiddenTranscriptMessages(i.agent.Messages())
			i.cumUsage = i.agent.Cost()
			if last := i.agent.LastTurnUsage(); last.InputTokens > 0 || last.CacheReadTokens > 0 || last.CacheWriteTokens > 0 {
				i.lastCtxInput = last.InputTokens + last.CacheReadTokens + last.CacheWriteTokens
			} else {
				i.lastCtxInput = 0
			}
			// Snap to the tail again — the swap brought in a fresh
			// transcript whose markdown / chroma cost we don't want
			// blocking the redraw.
			if len(i.view.Messages) > initialResumeTailLimit {
				i.view.TailLimit = initialResumeTailLimit
			} else {
				i.view.TailLimit = 0
			}
		}
		i.mu.Unlock()
		i.invalidate()
		if state.reason != compactContinuationNone {
			i.startRestoredCompactHandoff(i.runCtx)
		}
	}()
}
func (i *Interactive) applyModelSelection(prov, model string) {
	i.swapModel(prov, model, i.cfg.BuildAgentFor, false)
}
func (i *Interactive) applyRescueModelSelection(prov, model string) {
	builder := i.cfg.BuildAgentForRescue
	if builder == nil {
		builder = i.cfg.BuildAgentFor
	}
	i.swapModel(prov, model, builder, true)
}
func (i *Interactive) swapModel(prov, model string, builder func(string, string) (*core.Agent, string, string, error), rescue bool) {
	var replaced bool
	swap := func() {
		replaced = i.swapModelUnserialized(prov, model, builder, rescue)
	}
	if i.cfg.SessionTransition != nil {
		i.cfg.SessionTransition(swap)
	} else {
		swap()
	}
	if replaced {
		i.resetCompactHandoff()
	}
}
func (i *Interactive) swapModelUnserialized(prov, model string, builder func(string, string) (*core.Agent, string, string, error), rescue bool) bool {
	if model == "" {
		return false
	}
	m, err := provider.FindModel(prov, model)
	if err != nil {
		i.mu.Lock()
		i.statusErr = err.Error()
		i.mu.Unlock()
		return false
	}
	// Same provider AND not a rescue retry: just swap the model on
	// the existing agent. Mixed-API providers dispatch from model metadata,
	// so the client remains reusable. Rescue retries always rebuild so a stale
	// auth header / base URL can't carry over.
	if !rescue && i.agent != nil && m.Provider == i.cfg.Provider {
		i.mu.Lock()
		i.cfg.Model = m.ID
		i.agent.Model = m.ID
		i.statusOK = "model: " + m.ID
		i.statusErr = ""
		i.mu.Unlock()
		if i.cfg.PersistModel != nil {
			i.cfg.PersistModel(i.cfg.Provider, m.ID)
		}
		return false
	}
	if builder == nil {
		i.mu.Lock()
		i.statusErr = "cannot switch provider: no builder configured"
		i.mu.Unlock()
		return false
	}
	// Snapshot the current transcript and cumulative usage BEFORE we
	// build the replacement agent so we can hand them off. Without
	// this the user perceives the entire session as wiped on a
	// cross-provider /model swap.
	var carryMsgs []provider.Message
	var carryCost provider.Usage
	if i.agent != nil {
		carryMsgs = i.agent.Messages()
		carryCost = i.agent.Cost()
	}

	ag, p, md, err := builder(m.Provider, m.ID)
	if err != nil {
		i.mu.Lock()
		i.statusErr = err.Error()
		i.mu.Unlock()
		return false
	}

	// Replay the transcript and seed the cost on the freshly-built
	// agent. Messages travel cleanly between providers because they
	// use the same provider.Message shape; tool-call ids are local
	// to a turn so cross-provider continuation never confuses the
	// new model (it just sees the assistant's reply, no orphan
	// tool_use blocks because /model swaps are gated to idle state).
	if len(carryMsgs) > 0 {
		ag.SetMessages(carryMsgs)
	}
	ag.SeedCost(carryCost)

	if i.agent != nil && i.agent != ag {
		_ = tools.CloseLSPManagers(i.agent.ToolsSnapshot())
	}
	i.agentMu.Lock()
	i.mu.Lock()
	i.prepareReplacementAgentLocked(ag)
	i.agent = ag
	i.cfg.Provider = p
	i.cfg.Model = md
	if rescue {
		i.statusOK = "rescue retry: switched to " + p + " / " + md + " (ignored --api-key / --base-url)"
	} else {
		i.statusOK = "switched to " + p + " / " + md
	}
	i.statusErr = ""
	// Render cache keys are width+content based, so the new agent's
	// identical messages will reuse the existing entries. Nothing
	// to invalidate.
	i.mu.Unlock()
	i.agentMu.Unlock()
	// The new agent was built off the base tool registry, so any
	// dynamically-registered tools need to be reattached. The apply
	// helpers are no-ops when their feature is inactive, so the
	// cross-provider path still works on a vanilla setup.
	i.applyAutoSubagentsTool()
	i.applyTelegramTools(i.telegramBridge != nil)
	if i.cfg.PersistModel != nil {
		i.cfg.PersistModel(p, md)
	}
	return true
}
func (i *Interactive) handleAuthEvent(ev auth.Event) {
	switch ev.Kind {
	case "started":
		i.dialog.ShowWaiting(ev.URL)
	case "browser_open":
		// no-op
	case "error":
		i.dialog.ShowResult(false, ev.Message)
	case "success":
		// Keep the credential-driven resolve and its final replacement in the
		// same transition as settings/session changes. Otherwise a build that
		// started under the old web-search policy could commit afterward.
		var buildErr error
		var clearHandoff bool
		buildAndCommitLogin := func() {
			ag, prov, model, err := i.cfg.BuildAgent()
			if err != nil {
				buildErr = err
				return
			}
			i.agentMu.Lock()
			i.mu.Lock()
			oldAgent := i.agent
			i.prepareReplacementAgentLocked(ag)
			_, clearHandoff = i.resetCompactContinuationLocked()
			i.agent = ag
			i.cfg.Provider = prov
			i.cfg.Model = model
			i.statusErr = ""
			i.statusOK = "logged in to " + ev.Provider + " via " + ev.Method
			i.mu.Unlock()
			i.agentMu.Unlock()
			if oldAgent != nil {
				_ = tools.CloseLSPManagers(oldAgent.ToolsSnapshot())
			}
			i.applyAutoSubagentsTool()
			i.applyTelegramTools(i.telegramBridge != nil)
			// Authentication can change the provider used by the live
			// agent. Persist it on the active session just like /model.
			if i.cfg.PersistModel != nil {
				i.cfg.PersistModel(prov, model)
			}
		}
		if i.cfg.SessionTransition != nil {
			i.cfg.SessionTransition(buildAndCommitLogin)
		} else {
			buildAndCommitLogin()
		}
		if buildErr != nil {
			i.dialog.ShowResult(false, buildErr.Error())
			return
		}
		if clearHandoff {
			i.persistCompactHandoff(nil)
		}
		i.dialog.ShowResult(true, "")
	}
}
