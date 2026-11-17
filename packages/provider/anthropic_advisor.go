package provider

import (
	"encoding/json"
	"strings"
)

// Anthropic's native advisor server tool.
const (
	anthropicAdvisorBeta     = "advisor-tool-2026-03-01"
	anthropicAdvisorToolType = "advisor_20260301"
)

// AdvisorToolName is the tool name of the advisor, shared by the native
// server tool and the client-side advisor tool.
const AdvisorToolName = "advisor"

// anthropicAdvisorMinTokens is the documented minimum advisor max_tokens.
const anthropicAdvisorMinTokens = 1024

// anthAdvisorTool is the server tool definition. It has no input schema,
// so it cannot reuse anthTool.
type anthAdvisorTool struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens,omitempty"`
}

// firstPartyAnthropic reports whether this client talks to Anthropic itself.
// Anthropic-Messages-compatible endpoints (kimi, minimax, copilot, ...) reuse
// anthropicClient under another name and never receive server tool blocks.
func (c *anthropicClient) firstPartyAnthropic() bool {
	return c.Name() == "anthropic"
}

// prepareAnthServerBlocks filters ServerToolBlocks before conversion.
//
// When replay is false (non-first-party client, or a client tool occupies the
// advisor name without a server advisor) every server block is removed; a
// plaintext advisor result becomes a "[advisor advice] ..." text note.
//
// Otherwise, for Anthropic:
//   - an advisor_tool_result whose server_tool_use never appeared earlier is
//     dropped;
//   - a server_tool_use without a later result is kept only when keepPending
//     is set (resuming requires the advisor tool definition) and every later
//     message is a tool-result message; the API rejects a pending call
//     followed by anything else;
//   - blocks from another provider, without an ID, or with invalid Raw JSON
//     are dropped.
//
// Messages emptied by filtering are removed. hasServer reports whether any
// server block survives, which requires the advisor beta header. msgs is
// returned unchanged when it holds no server block.
func prepareAnthServerBlocks(msgs []Message, replay, keepPending bool) (out []Message, hasServer bool) {
	found := false
	for _, m := range msgs {
		for _, c := range m.Content {
			if _, ok := c.(ServerToolBlock); ok {
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		return msgs, false
	}
	usable := func(b ServerToolBlock) bool {
		return replay && b.Provider == "anthropic" && b.ID != "" && len(b.Raw) > 0 && json.Valid(b.Raw)
	}
	// onlyToolAfter[i] reports whether every message after i is a tool-result message.
	onlyToolAfter := make([]bool, len(msgs))
	tail := true
	for i := len(msgs) - 1; i >= 0; i-- {
		onlyToolAfter[i] = tail
		tail = tail && msgs[i].Role == RoleTool
	}
	resultAt := map[string]int{} // tool_use id -> global position of its last result
	pos := 0
	for _, m := range msgs {
		for _, c := range m.Content {
			if b, ok := c.(ServerToolBlock); ok && usable(b) && b.Kind == ServerToolKindAdvisorResult {
				resultAt[b.ID] = pos
			}
			pos++
		}
	}
	pos = 0
	seenUse := map[string]bool{}
	matched := map[string]bool{}
	out = make([]Message, 0, len(msgs))
	for i, m := range msgs {
		var filtered []Content // allocated on the first change
		for j, c := range m.Content {
			here := pos
			pos++
			b, ok := c.(ServerToolBlock)
			keep := !ok
			var note Content
			if ok && usable(b) {
				switch b.Kind {
				case ServerToolKindUse:
					if seenUse[b.ID] {
						break
					}
					if at, found := resultAt[b.ID]; found && at > here {
						keep = true
					} else if keepPending && onlyToolAfter[i] {
						keep = true
					}
					if keep {
						seenUse[b.ID] = true
					}
				case ServerToolKindAdvisorResult:
					keep = seenUse[b.ID] && !matched[b.ID]
					if keep {
						matched[b.ID] = true
					}
				}
			} else if ok && !replay && b.Kind == ServerToolKindAdvisorResult && strings.TrimSpace(b.AdvisorText) != "" {
				note = TextBlock{Text: "[advisor advice] " + b.AdvisorText}
			}
			if keep {
				if ok {
					hasServer = true
				}
				if filtered != nil {
					filtered = append(filtered, c)
				}
				continue
			}
			if filtered == nil {
				filtered = make([]Content, j, len(m.Content))
				copy(filtered, m.Content[:j])
			}
			if note != nil {
				filtered = append(filtered, note)
			}
		}
		if filtered == nil {
			out = append(out, m)
			continue
		}
		if len(filtered) == 0 {
			continue
		}
		cp := m
		cp.Content = filtered
		out = append(out, cp)
	}
	return out, hasServer
}

// anthServerBlockStart converts a streamed server block into a
// ServerToolBlock and the event announcing it. ok is false for server
// block types zut does not understand.
func anthServerBlockStart(raw json.RawMessage) (ServerToolBlock, EventServerTool, bool) {
	var head struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		ToolUseID string `json:"tool_use_id"`
		Content   struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			ErrorCode string `json:"error_code"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return ServerToolBlock{}, EventServerTool{}, false
	}
	verbatim := append(json.RawMessage(nil), raw...)
	switch head.Type {
	case ServerToolKindUse:
		if head.ID == "" {
			return ServerToolBlock{}, EventServerTool{}, false
		}
		b := ServerToolBlock{Provider: "anthropic", Kind: ServerToolKindUse, ID: head.ID, Name: head.Name, Raw: verbatim}
		return b, EventServerTool{ID: head.ID, Name: head.Name, Phase: ServerToolPhaseStart}, true
	case ServerToolKindAdvisorResult:
		if head.ToolUseID == "" {
			return ServerToolBlock{}, EventServerTool{}, false
		}
		b := ServerToolBlock{Provider: "anthropic", Kind: ServerToolKindAdvisorResult, ID: head.ToolUseID, Name: AdvisorToolName, Raw: verbatim}
		switch head.Content.Type {
		case "advisor_result":
			b.AdvisorText = head.Content.Text
		case "advisor_redacted_result":
			b.Redacted = true
		default:
			// Any other result type is treated as a failure; its exact
			// shape is not documented, so prefer error_code, then the type.
			b.ErrorCode = head.Content.ErrorCode
			if b.ErrorCode == "" {
				b.ErrorCode = head.Content.Type
			}
			if b.ErrorCode == "" {
				b.ErrorCode = "unknown"
			}
		}
		ev := EventServerTool{ID: b.ID, Name: b.Name, Phase: ServerToolPhaseResult, Text: b.AdvisorText, ErrorCode: b.ErrorCode, Redacted: b.Redacted}
		return b, ev, true
	}
	return ServerToolBlock{}, EventServerTool{}, false
}

// anthUsageIteration is one entry of message_delta usage.iterations.
type anthUsageIteration struct {
	Type                     string `json:"type"`
	Model                    string `json:"model"`
	InputTokens              int    `json:"input_tokens"`
	OutputTokens             int    `json:"output_tokens"`
	CacheReadInputTokens     int    `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int    `json:"cache_creation_input_tokens"`
}

// anthAdvisorIterationsCost prices advisor_message iterations at the
// advisor model's catalog rates. A model missing from the catalog adds no
// cost, so the session total undercounts it; there is no price to apply.
func anthAdvisorIterationsCost(iterations []anthUsageIteration) float64 {
	var cost float64
	for _, it := range iterations {
		if it.Type != "advisor_message" || strings.TrimSpace(it.Model) == "" {
			continue
		}
		m, err := FindModel("anthropic", it.Model)
		if err != nil {
			continue
		}
		cost += ComputeCost(m, Usage{
			InputTokens:      it.InputTokens,
			OutputTokens:     it.OutputTokens,
			CacheReadTokens:  it.CacheReadInputTokens,
			CacheWriteTokens: it.CacheCreationInputTokens,
		})
	}
	return cost
}

// anthAdvisorMaxTokens returns the wire max_tokens for the advisor tool:
// zero leaves it unset, positive values are raised to the documented minimum.
func anthAdvisorMaxTokens(n int) int {
	if n <= 0 {
		return 0
	}
	return max(n, anthropicAdvisorMinTokens)
}

func hasToolNamed(tools []Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// lastAnthExecutorIteration returns the last executor ("message") iteration.
func lastAnthExecutorIteration(iterations []anthUsageIteration) (anthUsageIteration, bool) {
	for i := len(iterations) - 1; i >= 0; i-- {
		if iterations[i].Type == "message" {
			return iterations[i], true
		}
	}
	return anthUsageIteration{}, false
}

// pendingAnthServerUses returns server_tool_use IDs in msgs without a later
// result: the API runs them at the start of the next response.
func pendingAnthServerUses(msgs []Message) map[string]bool {
	pending := map[string]bool{}
	for _, m := range msgs {
		for _, c := range m.Content {
			if b, ok := c.(ServerToolBlock); ok {
				if b.Kind == ServerToolKindUse {
					pending[b.ID] = true
				} else {
					delete(pending, b.ID)
				}
			}
		}
	}
	return pending
}
