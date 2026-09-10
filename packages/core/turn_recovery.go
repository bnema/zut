package core

import (
	"errors"
	"strings"

	"github.com/bnema/zut/packages/provider"
)

// ErrIncompleteTurn reports that a normal turn ended without a final answer
// twice in one top-level invocation, even after one bounded recovery
// continuation. Hosts surface it as a visible incomplete outcome, never as
// success. It matches with errors.Is.
var ErrIncompleteTurn = errors.New("agent turn ended without a final answer")

// Bounded normal-turn recovery reasons emitted on EvTurnRecovery.
const (
	TurnRecoveryReasonMissingAnswer  = "missing_answer"
	TurnRecoveryReasonCommentaryOnly = "commentary_only"
)

// turnRecoveryMetaKey marks the synthetic user-context message appended for
// a recovery continuation, following the existing goal synthetic-context
// pattern.
const turnRecoveryMetaKey = "zut_turn_recovery"

// turnRecoveryPrompt is the fixed bounded continuation appended on an
// eligible incomplete normal stop.
const turnRecoveryPrompt = "The request ended without a final answer. Continue the authorized work if it remains actionable. Otherwise provide the result, the concrete blocker, or the specific decision required. Do not merely promise to continue. Respect the user's scope, tool permissions, required confirmations, and any pending required work."

// classifyTerminalAssistant classifies the current terminal assistant
// message of a successful normal stop. It returns the recovery reason and
// true when the message carries no final answer: empty, whitespace-only, or
// reasoning-only content yields missing_answer; nonempty text exclusively
// marked commentary yields commentary_only.
//
// A non-whitespace text block with empty, unknown, or final phase is an
// answer, as is any user-facing non-text output such as an image. Reasoning
// and tool-call blocks are never answers. A nonempty refusal, clarification
// question, or short final answer arrives as ordinary text and therefore
// counts as complete; no prose sniffing is performed here.
func classifyTerminalAssistant(msg provider.Message) (reason string, incomplete bool) {
	commentaryOnly := false
	for _, c := range msg.Content {
		switch b := c.(type) {
		case provider.TextBlock:
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			if b.Phase == provider.TextPhaseCommentary {
				commentaryOnly = true
				continue
			}
			// Empty, unknown, and final phases all retain compatibility
			// behavior: the text counts as an answer.
			return "", false
		case provider.ImageBlock:
			return "", false
		default:
			// Reasoning, tool-call, and tool-result blocks are not
			// user-facing answers.
			continue
		}
	}
	if commentaryOnly {
		return TurnRecoveryReasonCommentaryOnly, true
	}
	return TurnRecoveryReasonMissingAnswer, true
}

// ToolDeniedError marks a tool execution refusal (permission scope,
// allowlist, jail confinement, or admission gate) as distinct from an
// ordinary tool failure. Tools return it through Execute; runOneTool
// records the refusal so a later incomplete normal stop does not gain a
// recovery prompt urging more actions. The Reason text is shown to the
// model as the tool result, unchanged from the untyped errors it replaces.
type ToolDeniedError struct {
	Reason string
}

func (e *ToolDeniedError) Error() string {
	if e == nil || e.Reason == "" {
		return "tool call denied"
	}
	return e.Reason
}

// asToolDeniedError reports whether err chains to a ToolDeniedError.
func asToolDeniedError(err error) bool {
	var denied *ToolDeniedError
	return errors.As(err, &denied)
}

// markDeniedToolCall records that a tool invocation was refused at the
// execution boundary (guard or confirmation denial, or a denied tool
// execution). A denied invocation must not gain a recovery prompt urging
// more actions.
func (a *Agent) markDeniedToolCall() {
	a.mu.Lock()
	a.deniedToolCall = true
	a.mu.Unlock()
}

// sawDeniedToolCall reports whether any tool invocation was denied during
// the current top-level runLoop invocation.
func (a *Agent) sawDeniedToolCall() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.deniedToolCall
}

// resetDeniedToolCalls clears the execution-local denial flag at the start
// of each top-level Prompt/Continue invocation.
func (a *Agent) resetDeniedToolCalls() {
	a.mu.Lock()
	a.deniedToolCall = false
	a.mu.Unlock()
}
