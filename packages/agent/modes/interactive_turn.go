package modes

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bnema/zut/packages/agent/session"
	"github.com/bnema/zut/packages/agent/tools"
	"github.com/bnema/zut/packages/provider"
)

func shellEscapeCommand(text string) (string, bool) {
	return ShellEscapeCommand(text)
}
func ShellEscapeCommand(text string) (string, bool) {
	trimmed := strings.TrimLeft(text, " \t")
	if !strings.HasPrefix(trimmed, "!") {
		return "", false
	}
	cmd := strings.TrimSpace(strings.TrimPrefix(trimmed, "!"))
	if cmd == "" {
		return "", false
	}
	return cmd, true
}
func (i *Interactive) startShellEscape(parent context.Context, cmd string) {
	i.mu.Lock()
	if i.busy || i.shellRunning {
		i.statusErr = "busy — wait for the current turn to finish before running a shell command"
		i.statusOK = ""
		i.mu.Unlock()
		i.invalidate()
		return
	}
	if parent == nil {
		parent = i.runCtx
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	i.busy = true
	i.shellRunning = true
	i.shellLive = "$ " + cmd + "\n\n"
	i.cancelTurn = cancel
	i.statusErr = ""
	i.statusOK = ""
	i.spin.Start()
	i.activity = agentActivity{activity: activity{kind: activityRunningShellCommand}}
	// Clear stale extension notes the same way a new turn would so the
	// screen doesn't accumulate transient state.
	i.extNotes = nil
	i.scrollOffset = 0
	i.parkedTurn = 0
	i.parkedTotal = 0
	i.helpBlock = nil
	sandbox := i.cfg.Sandbox
	cwd := i.cfg.CWD
	i.mu.Unlock()
	i.invalidate()

	go func() {
		defer cancel()
		raw, _ := json.Marshal(map[string]any{
			"command": cmd,
			"timeout": ShellEscapeTimeoutSeconds,
		})
		bash := &tools.BashTool{CWD: cwd, Env: os.Environ(), Sandbox: sandbox}
		progress := func(chunk string) {
			i.mu.Lock()
			i.shellLive += chunk
			i.mu.Unlock()
			i.invalidate()
		}
		res, err := bash.Execute(ctx, raw, progress)

		var out string
		if err != nil {
			out = "$ " + cmd + "\n\n" + err.Error() + "\n\n[error]"
		} else {
			for _, c := range res.Content {
				if tb, ok := c.(provider.TextBlock); ok {
					out += tb.Text
				}
			}
		}
		cancelled := ctx.Err() != nil
		failed := err != nil || res.IsError || cancelled
		if cancelled {
			out += "\n\n[cancelled]"
		}

		var contextErr error
		if port := i.SessionPort(); port != nil {
			// The daemon owns the transcript: send the output so the next
			// prompt sees it, as the in-process path did below.
			contextErr = appendPortShellContext(port, out)
		} else if i.agent != nil {
			i.agent.AppendUserContext(out, map[string]string{shellEscapeMetaKey: "true"})
		}

		i.mu.Lock()
		i.shellRunning = false
		i.shellLive = ""
		i.busy = false
		i.cancelTurn = nil
		pendingIdleWork := i.takePendingIdleWorkLocked()
		awaitingPre := i.awaitingStartupPre
		if failed {
			if cancelled {
				i.statusErr = "shell command cancelled"
			} else {
				i.statusErr = "shell command failed"
			}
			i.statusOK = ""
		} else {
			i.statusOK = "shell command finished"
			i.statusErr = ""
		}
		if contextErr != nil {
			i.statusOK = ""
			i.statusErr = "shell output not added to the session: " + contextErr.Error()
		}
		i.mu.Unlock()
		runPendingIdleWork(pendingIdleWork)
		i.invalidate()
		if awaitingPre {
			i.completeStartupPre()
		}
	}()
}

func mainAlertReason(ctx context.Context, err, turnErr error, stop provider.StopReason, awaitingPre, hasNext, rescue, recovering, autoCompacting bool) alertReason {
	if awaitingPre || ctx.Err() != nil || hasNext || recovering || autoCompacting {
		return ""
	}
	if rescue {
		return alertRescueRequired
	}
	if stop == provider.StopAborted {
		return ""
	}
	if err != nil || turnErr != nil || stop == provider.StopError {
		return alertAgentError
	}
	if stop == provider.StopLength {
		return alertResponseTruncated
	}
	return alertAgentDone
}

func NormalizeAutoCompactThreshold(threshold *int) int {
	const defaultAutoCompactThreshold = 85
	if threshold == nil {
		return defaultAutoCompactThreshold
	}
	switch *threshold {
	case 0, 70, 80, 85, 90, 95:
		return *threshold
	default:
		return defaultAutoCompactThreshold
	}
}
func ShouldAutoCompact(inputTokens, contextWindow, thresholdPercent int) bool {
	if inputTokens <= 0 || contextWindow <= 0 || thresholdPercent <= 0 {
		return false
	}
	return float64(inputTokens)/float64(contextWindow) >= float64(thresholdPercent)/100
}
func classifyCompactionContinuation(origin compactContinuationOrigin, statusRescueActive bool, stop provider.StopReason, turnErr error, msgs []provider.Message) compactContinuationReason {
	if origin != compactOriginManual && structuralCompactionContinuation(msgs) {
		return compactContinuationStructuralTail
	}
	if (origin != compactOriginAfterTurnThreshold && !statusRescueActive) || stop != provider.StopEnd || turnErr != nil {
		return compactContinuationNone
	}
	if likelyForwardWorkStatus(msgs) {
		return compactContinuationStatusRescue
	}
	return compactContinuationNone
}
func structuralCompactionContinuation(msgs []provider.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	if last.Role != provider.RoleAssistant {
		return true
	}
	hasText := false
	for _, content := range last.Content {
		switch block := content.(type) {
		case provider.ToolCallBlock:
			return true
		case provider.TextBlock:
			if strings.TrimSpace(block.Text) != "" {
				hasText = true
			}
		case provider.ReasoningBlock:
			// Reasoning without a visible answer is not terminal.
		default:
			return true
		}
	}
	return !hasText
}
func likelyForwardWorkStatus(msgs []provider.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	if last.Role != provider.RoleAssistant {
		return false
	}
	var text strings.Builder
	for _, content := range last.Content {
		block, ok := content.(provider.TextBlock)
		if !ok {
			if _, hasTool := content.(provider.ToolCallBlock); hasTool {
				return false
			}
			continue
		}
		if strings.TrimSpace(block.Text) == "" {
			continue
		}
		if text.Len() > 0 {
			text.WriteByte(' ')
		}
		text.WriteString(block.Text)
	}
	if text.Len() == 0 {
		return false
	}
	normalized := strings.ToLower(strings.Join(strings.Fields(text.String()), " "))
	normalized = strings.NewReplacer("’", "'", "‘", "'", "`", "'").Replace(normalized)
	for _, phrase := range []string{
		"next i will",
		"next, i will",
		"i'll now",
		"i will now",
		"i am going to",
		"i need to continue",
		"then i will",
		"then, i will",
		"i will proceed",
	} {
		if containsFutureWorkPhrase(normalized, phrase) {
			return true
		}
	}
	return false
}
func containsFutureWorkPhrase(text, phrase string) bool {
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], phrase)
		if index < 0 {
			return false
		}
		index += offset
		end := index + len(phrase)
		if !wordRuneBefore(text, index) && !wordRuneAfter(text, end) {
			return true
		}
		offset = end
	}
	return false
}
func wordRuneBefore(text string, index int) bool {
	if index == 0 {
		return false
	}
	rune_, _ := utf8.DecodeLastRuneInString(text[:index])
	return unicode.IsLetter(rune_) || unicode.IsDigit(rune_) || rune_ == '_'
}
func wordRuneAfter(text string, index int) bool {
	if index == len(text) {
		return false
	}
	rune_, _ := utf8.DecodeRuneInString(text[index:])
	return unicode.IsLetter(rune_) || unicode.IsDigit(rune_) || rune_ == '_'
}

// appendPortShellContext sends shell-escape output to the daemon session,
// keeping the tail within the session's size limit.
func appendPortShellContext(port session.Port, out string) error {
	if len(out) > session.MaxShellContextBytes {
		const marker = "[shell output truncated]\n"
		out = marker + strings.ToValidUTF8(out[len(out)-(session.MaxShellContextBytes-len(marker)):], "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), shellContextTimeout)
	defer cancel()
	return port.AppendShellContext(ctx, out)
}

const shellContextTimeout = 30 * time.Second
