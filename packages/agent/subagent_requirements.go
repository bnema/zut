package agent

import (
	"fmt"
	"strings"

	"github.com/bnema/zut/packages/core"
)

const requiredWorkerContextHeader = "[required-subagents update]"

// WireRequiredWorkerGate prevents a parent terminal response while required
// resident work remains unresolved. The manager is the sole authority; there
// is no worker reload or process-side acknowledgement path.
func (rt *subagentRuntime) WireRequiredWorkerGate(parent *core.Agent) {
	if rt == nil || parent == nil || rt.residentManagerForTools() == nil {
		return
	}
	previousBeforeAssistant := parent.BeforeAssistantMessage
	parent.BeforeAssistantMessage = func(text string) (bool, string, string) {
		unmet := rt.residentManagerForTools().UnmetRequired()
		if len(unmet) != 0 {
			var b strings.Builder
			b.WriteString(requiredWorkerContextHeader)
			b.WriteString("\nTerminal completion is blocked by required resident delegated work:")
			for _, child := range unmet {
				fmt.Fprintf(&b, "\n- %s: %s", child.ID, child.State)
			}
			return false, b.String(), ""
		}
		if previousBeforeAssistant != nil {
			return previousBeforeAssistant(text)
		}
		return true, "", ""
	}
}
