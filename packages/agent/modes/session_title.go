package modes

import (
	"context"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

const sessionTitleGenerationTimeout = 10 * time.Second

func terminalTitleEnabled(enabled *bool) bool {
	return enabled == nil || *enabled
}

// hasRealUserPrompt ignores transcript rows that are context for the model but
// not a prompt that started work: shell escapes and image mirrors are both
// user-role messages internally.
func hasRealUserPrompt(messages []provider.Message) bool {
	for _, message := range messages {
		if message.Role != provider.RoleUser || isHiddenTranscriptMessage(message) || message.Meta[shellEscapeMetaKey] == "true" {
			continue
		}
		if strings.TrimSpace(userMessageText(message)) != "" || len(message.Content) > 0 {
			return true
		}
	}
	return false
}

// maybeStartSessionTitle marks the first real prompt and starts the hidden
// title request. It is safe to call at every prompt boundary; the state guard
// makes it a one-shot per interactive session/branch.
func (i *Interactive) maybeStartSessionTitle(parent context.Context, prompt string) {
	if parent == nil {
		parent = i.runCtx
	}
	if parent == nil {
		parent = context.Background()
	}

	i.mu.Lock()
	if !i.interactiveStarted || i.agent == nil || i.titleRealPromptSeen || i.titleGenerationStarted {
		i.mu.Unlock()
		return
	}
	i.titleRealPromptSeen = true
	i.firstRealPrompt = prompt
	if !terminalTitleEnabled(i.cfg.TerminalTitleEnabled) {
		i.mu.Unlock()
		return
	}
	i.titleGenerationStarted = true
	agent := i.agent
	client := agent.Client
	model := agent.Model
	version := i.titleVersion
	i.mu.Unlock()

	if strings.TrimSpace(prompt) == "" {
		i.finishSessionTitle(version, "new session")
		return
	}

	ctx, cancel := context.WithTimeout(parent, sessionTitleGenerationTimeout)
	i.mu.Lock()
	if version != i.titleVersion {
		i.mu.Unlock()
		cancel()
		return
	}
	i.titleCancel = cancel
	i.mu.Unlock()

	go func() {
		defer cancel()
		title, err := core.GenerateSessionTitle(ctx, client, model, prompt)
		if err != nil {
			// Title generation is deliberately best effort. The main turn
			// must not depend on this second request succeeding.
			title = core.NormalizeSessionTitle(prompt)
		}
		if parent.Err() != nil {
			return
		}
		if title == "" {
			title = "new session"
		}
		i.finishSessionTitle(version, title)
	}()
}

// finishSessionTitle applies a generated or fallback title only if the
// session has not been switched, forked, or manually renamed meanwhile.
func (i *Interactive) finishSessionTitle(version uint64, title string) {
	title = core.NormalizeSessionTitle(title)
	if title == "" {
		title = "new session"
	}

	i.mu.Lock()
	if version != i.titleVersion || !terminalTitleEnabled(i.cfg.TerminalTitleEnabled) {
		i.mu.Unlock()
		return
	}
	i.titleCancel = nil
	i.sessionTitle = title
	if i.cfg.PersistTitle != nil {
		if err := i.cfg.PersistTitle(title); err != nil {
			i.statusErr = "session title: " + err.Error()
		}
	}
	i.writeTerminalTitleLocked(title)
	i.mu.Unlock()
	i.invalidate()
}

func (i *Interactive) writeTerminalTitleLocked(subject string) {
	if i.cfg.Terminal == nil {
		return
	}
	subject = core.NormalizeSessionTitle(subject)
	if subject != "" && !terminalTitleEnabled(i.cfg.TerminalTitleEnabled) {
		return
	}
	display := "zut"
	if subject != "" {
		display += ": " + subject
	}
	_, _ = i.cfg.Terminal.Write([]byte(tui.SetTitle(display)))
}

func (i *Interactive) markSessionTitleSwitching() {
	i.mu.Lock()
	cancel := i.titleCancel
	i.titleCancel = nil
	i.titleVersion++
	i.titleGenerationStarted = true
	i.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// CancelPendingSessionTitle invalidates a title request before a host swaps
// the session it owns. Hosts must call it before changing their session
// pointer so a late result cannot be persisted into the replacement session.
func (i *Interactive) CancelPendingSessionTitle() {
	i.markSessionTitleSwitching()
}

func (i *Interactive) cancelSessionTitle() {
	i.mu.Lock()
	cancel := i.titleCancel
	i.titleCancel = nil
	i.titleVersion++
	i.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (i *Interactive) restoreLoadedSessionTitle() {
	pending := false
	if i.cfg.CurrentSessionTitlePending != nil {
		pending = i.cfg.CurrentSessionTitlePending()
	}
	title := ""
	if !pending && i.cfg.CurrentSessionTitle != nil {
		title = i.cfg.CurrentSessionTitle()
	}

	i.mu.Lock()
	i.titleVersion++
	i.sessionTitle = core.NormalizeSessionTitle(title)
	i.firstRealPrompt = ""
	if pending {
		i.titleRealPromptSeen = false
		i.titleGenerationStarted = false
	} else {
		i.titleRealPromptSeen = hasRealUserPrompt(i.agentMessagesLocked())
		i.titleGenerationStarted = i.titleRealPromptSeen || i.sessionTitle != ""
	}
	i.writeTerminalTitleLocked(i.sessionTitle)
	i.mu.Unlock()
}

func (i *Interactive) restoreFailedSessionTitle() {
	i.mu.Lock()
	prompt := i.firstRealPrompt
	i.mu.Unlock()
	if prompt == "" {
		i.restoreLoadedSessionTitle()
		return
	}
	i.restoreLoadedSessionTitle()
	i.mu.Lock()
	if i.sessionTitle != "" {
		i.mu.Unlock()
		return
	}
	i.firstRealPrompt = prompt
	i.titleRealPromptSeen = false
	i.titleGenerationStarted = false
	i.mu.Unlock()
	i.maybeStartSessionTitle(i.runCtx, prompt)
}

func (i *Interactive) resetSessionTitleForFreshBranch() {
	i.mu.Lock()
	cancel := i.resetSessionTitleLocked()
	i.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// resetSessionTitleLocked drops the current title, shows the neutral terminal
// title, and lets the next real prompt generate a new one. Callers hold i.mu
// and must call the returned cancel func (if any) after unlocking.
func (i *Interactive) resetSessionTitleLocked() context.CancelFunc {
	cancel := i.titleCancel
	i.titleCancel = nil
	i.titleVersion++
	i.sessionTitle = ""
	i.titleRealPromptSeen = false
	i.titleGenerationStarted = false
	i.firstRealPrompt = ""
	i.writeTerminalTitleLocked("")
	return cancel
}

func (i *Interactive) agentMessagesLocked() []provider.Message {
	if i.agent == nil {
		return nil
	}
	return i.agent.Messages()
}

func (i *Interactive) setManualSessionTitle(title string) {
	title = core.NormalizeSessionTitle(title)
	if title == "" {
		return
	}
	if i.portMode.Load() {
		i.portSettingsCall("title", func(ctx context.Context, port session.Port) error {
			return port.SetTitle(ctx, title)
		})
		return
	}
	i.mu.Lock()
	cancel := i.titleCancel
	i.titleCancel = nil
	i.titleVersion++
	i.sessionTitle = title
	i.titleRealPromptSeen = true
	i.titleGenerationStarted = true
	i.firstRealPrompt = ""
	i.writeTerminalTitleLocked(title)
	onChanged := i.cfg.OnSessionTitleChanged
	i.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if onChanged != nil {
		onChanged(title)
	}
}

func (i *Interactive) markInteractiveStarted() {
	i.mu.Lock()
	i.interactiveStarted = true
	if i.sessionTitle != "" {
		i.writeTerminalTitleLocked(i.sessionTitle)
	}
	i.mu.Unlock()
}
