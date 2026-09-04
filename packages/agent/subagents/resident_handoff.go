package subagents

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/bnema/zut/packages/core"
)

const residentHandoffBytes = 16 << 10

// residentBudgetHandoff is an evidence-only fallback, not a model-generated
// claim of completion. The durable transcript retains full tool arguments and
// results, including side effects that cannot be inferred from a patch.
func residentBudgetHandoff(dir string, spec ResidentChildSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Partial result: budget exhausted; work is NOT complete.\nHistory: %s\nResult: %s\nWorkspace: %s\n", HistoryRef(spec.ID), ResultRef(spec.ID), spec.Workspace)
	b.WriteString("Recovery: inspect the saved history and artifacts before repeating side effects, then use subagent_resume with the remaining task. Explicit resume grants a fresh allowance of the same size; required work remains unmet until success.\n")
	b.WriteString("The following are bounded recent observations, not verified findings or an exhaustive list of side effects. Unfinished steps must be reconciled against the accepted task. Raw tool arguments and output are omitted because they may contain secrets; inspect them in the child history.\n")
	items, err := ReadResidentHistory(dir, 16)
	if err != nil {
		b.WriteString("Recent history unavailable; inspect the durable child session before retrying.\n")
		return b.String()
	}
	// Do not mix in earlier turns when the latest prompt is in this page.
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Type == residentRecordUser {
			items = items[i:]
			break
		}
	}
	for _, item := range items {
		switch item.Type {
		case residentRecordUser, residentRecordAssistant:
			message, err := core.DecodeMessageJSON(item.Message)
			if err == nil {
				fmt.Fprintf(&b, "%s: %s\n", item.Type, handoffExcerpt(residentAssistantSummary(message), 1024))
			}
		case residentRecordToolCall:
			fmt.Fprintf(&b, "tool %s (%s): called; arguments retained in history\n", item.ToolName, item.ToolID)
		case residentRecordToolResult:
			var result struct {
				IsError bool
			}
			if err := json.Unmarshal(item.ToolResult, &result); err == nil {
				fmt.Fprintf(&b, "tool result (%s): is_error=%t; output retained in history\n", item.ToolID, result.IsError)
			}
		}
		if b.Len() >= residentHandoffBytes {
			break
		}
	}
	return handoffExcerpt(b.String(), residentHandoffBytes)
}

func handoffExcerpt(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit - len("…")
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "…"
}
