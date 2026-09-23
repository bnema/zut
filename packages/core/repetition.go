package core

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"strings"
	"time"

	"github.com/bnema/zut/packages/provider"
)

// ErrRepetitiveLoop reports that an agent repeated identical work after being
// warned to try a different approach. It matches with errors.Is.
var ErrRepetitiveLoop = errors.New("agent stopped after detecting a repetitive loop")

const (
	repetitionWarnThreshold = 5
	repetitionStopThreshold = 8
	repetitionWindowSize    = 64

	// RepetitionGuardMetaKey marks the hidden prompt added when the agent
	// detects repeated content.
	RepetitionGuardMetaKey = "zut_repetition_guard"
)

// RepetitionKind identifies the exact content pattern that repeated.
type RepetitionKind string

const (
	RepetitionKindToolCall         RepetitionKind = "tool_call_result"
	RepetitionKindAssistantMessage RepetitionKind = "assistant_message"
)

// RepetitionGuardStage identifies a warning or a terminal stop.
type RepetitionGuardStage string

const (
	RepetitionGuardWarning RepetitionGuardStage = "warning"
	RepetitionGuardStopped RepetitionGuardStage = "stopped"
)

type repetitionObservation struct {
	fingerprint string
	kind        RepetitionKind
	toolName    string
	at          time.Time
}

type repetitionPattern struct {
	count     int
	firstSeen time.Time
	warned    bool
	kind      RepetitionKind
	toolName  string
}

type repetitionGuardState struct {
	patterns        map[string]repetitionPattern
	history         []repetitionObservation
	stopped         bool
	stoppedDecision repetitionDecision
}

type repetitionDecision struct {
	stage    RepetitionGuardStage
	kind     RepetitionKind
	toolName string
	count    int
	elapsed  time.Duration
}

// observeRepetition counts one exact content fingerprint in a bounded sliding
// window. Fingerprints contain no call ID, timing, or raw content, so repeated
// executions can match without retaining private tool arguments or output.
func (a *Agent) observeRepetition(fingerprint string, kind RepetitionKind, toolName string) repetitionDecision {
	if fingerprint == "" || a.DisableRepetitionGuard {
		return repetitionDecision{}
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.repetition.stopped {
		decision := a.repetition.stoppedDecision
		decision.stage = RepetitionGuardStopped
		return decision
	}
	if a.repetition.patterns == nil {
		a.repetition.patterns = make(map[string]repetitionPattern)
	}
	a.repetition.history = append(a.repetition.history, repetitionObservation{
		fingerprint: fingerprint,
		kind:        kind,
		toolName:    toolName,
		at:          now,
	})
	if len(a.repetition.history) > repetitionWindowSize {
		oldest := a.repetition.history[0]
		a.repetition.history[0] = repetitionObservation{}
		a.repetition.history = a.repetition.history[1:]
		oldPattern := a.repetition.patterns[oldest.fingerprint]
		oldPattern.count--
		if oldPattern.count <= 0 {
			delete(a.repetition.patterns, oldest.fingerprint)
		} else {
			if oldPattern.count < repetitionWarnThreshold {
				oldPattern.warned = false
			}
			oldPattern.firstSeen = firstRepetitionSeen(a.repetition.history, oldest.fingerprint)
			a.repetition.patterns[oldest.fingerprint] = oldPattern
		}
	}
	pattern := a.repetition.patterns[fingerprint]
	if pattern.count == 0 {
		pattern.firstSeen = now
	}
	pattern.count++
	pattern.kind = kind
	if toolName != "" {
		pattern.toolName = toolName
	}
	a.repetition.patterns[fingerprint] = pattern
	decision := repetitionDecision{
		kind:     pattern.kind,
		toolName: pattern.toolName,
		count:    pattern.count,
		elapsed:  now.Sub(pattern.firstSeen),
	}
	if pattern.count >= repetitionStopThreshold && pattern.warned {
		a.repetition.stopped = true
		decision.stage = RepetitionGuardStopped
		a.repetition.stoppedDecision = decision
		return decision
	}
	if pattern.count >= repetitionWarnThreshold && !pattern.warned {
		pattern.warned = true
		a.repetition.patterns[fingerprint] = pattern
		decision.stage = RepetitionGuardWarning
	}
	return decision
}

func firstRepetitionSeen(history []repetitionObservation, fingerprint string) time.Time {
	for _, observation := range history {
		if observation.fingerprint == fingerprint {
			return observation.at
		}
	}
	return time.Now()
}

func (a *Agent) repetitionStopped() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.repetition.stopped && !a.DisableRepetitionGuard
}

func (a *Agent) repetitiveLoopError() error {
	a.mu.Lock()
	decision := a.repetition.stoppedDecision
	a.mu.Unlock()
	if decision.toolName != "" {
		return fmt.Errorf("%w: tool %q repeated %d times with identical results over %s", ErrRepetitiveLoop, decision.toolName, decision.count, decision.elapsed.Round(time.Second))
	}
	return fmt.Errorf("%w: identical assistant message repeated %d times over %s", ErrRepetitiveLoop, decision.count, decision.elapsed.Round(time.Second))
}

func (a *Agent) handleRepetitionDecision(decision repetitionDecision, sink func(AgentEvent)) bool {
	if decision.stage == RepetitionGuardWarning {
		prompt := "You appear to be stuck in a loop: the same assistant message has repeated. Try a substantively different action or workaround. If you are blocked, explain the blocker and ask the user for help instead of repeating the same work."
		if decision.toolName != "" {
			prompt = fmt.Sprintf("You appear to be stuck repeating tool %q with identical arguments and results (%d repetitions). Try a different action or workaround; if blocked, ask the user for help instead of repeating the tool.", decision.toolName, decision.count)
		}
		a.AppendUserContext(prompt, map[string]string{RepetitionGuardMetaKey: "true"})
	}
	sink(EvRepetitionGuard{
		Kind:    decision.kind,
		Stage:   decision.stage,
		Count:   decision.count,
		Elapsed: decision.elapsed,
	})
	if decision.stage != RepetitionGuardStopped {
		return false
	}
	sink(EvDone{})
	return true
}

func assistantMessageFingerprint(message provider.Message) string {
	h := sha256.New()
	writeRepetitionField(h, []byte("assistant-message"))
	var hasText bool
	for _, content := range message.Content {
		text, ok := content.(provider.TextBlock)
		if !ok || strings.TrimSpace(text.Text) == "" {
			continue
		}
		hasText = true
		writeRepetitionField(h, []byte(text.Text))
	}
	if !hasText {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func toolCallResultFingerprint(call provider.ToolCallBlock, result provider.ToolResultBlock) string {
	h := sha256.New()
	writeRepetitionField(h, []byte("tool-call-result"))
	writeRepetitionField(h, []byte(call.Name))
	writeRepetitionField(h, canonicalRepetitionJSON(call.Arguments))
	if result.IsError {
		writeRepetitionField(h, []byte{1})
	} else {
		writeRepetitionField(h, []byte{0})
	}
	for _, content := range result.Content {
		switch block := content.(type) {
		case provider.TextBlock:
			writeRepetitionField(h, []byte("text"))
			writeRepetitionField(h, []byte(block.Text))
			writeRepetitionField(h, []byte(block.ThoughtSignature))
			writeRepetitionField(h, []byte(block.Phase))
		case provider.ImageBlock:
			writeRepetitionField(h, []byte("image"))
			writeRepetitionField(h, []byte(block.MimeType))
			writeRepetitionField(h, block.Data)
			writeRepetitionField(h, []byte(block.ThoughtSignature))
		case provider.ToolCallBlock:
			writeRepetitionField(h, []byte("tool_call"))
			writeRepetitionField(h, []byte(block.Name))
			writeRepetitionField(h, canonicalRepetitionJSON(block.Arguments))
		case provider.ToolResultBlock:
			writeRepetitionField(h, []byte("tool_result"))
			if block.IsError {
				writeRepetitionField(h, []byte{1})
			} else {
				writeRepetitionField(h, []byte{0})
			}
			for _, nested := range block.Content {
				if encoded, err := json.Marshal(nested); err == nil {
					writeRepetitionField(h, encoded)
				}
			}
		case provider.ReasoningBlock:
			writeRepetitionField(h, []byte("reasoning"))
			writeRepetitionField(h, []byte(block.ID))
			writeRepetitionField(h, []byte(block.Summary))
			writeRepetitionField(h, []byte(block.Encrypted))
		default:
			encoded, err := json.Marshal(content)
			if err != nil {
				writeRepetitionField(h, []byte(fmt.Sprintf("%T", content)))
			} else {
				writeRepetitionField(h, encoded)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeRepetitionField(h hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(value)
}

func repetitionCallObservations(assistant, toolMessage provider.Message) []repetitionObservation {
	results := make(map[string]provider.ToolResultBlock, len(toolMessage.Content))
	for _, content := range toolMessage.Content {
		if result, ok := content.(provider.ToolResultBlock); ok {
			results[result.CallID] = result
		}
	}
	observations := make([]repetitionObservation, 0, len(results))
	seen := make(map[string]bool, len(results))
	for _, content := range assistant.Content {
		call, ok := content.(provider.ToolCallBlock)
		if !ok {
			continue
		}
		result, ok := results[call.ID]
		if !ok {
			continue
		}
		fingerprint := toolCallResultFingerprint(call, result)
		if seen[fingerprint] {
			continue
		}
		seen[fingerprint] = true
		observations = append(observations, repetitionObservation{
			fingerprint: fingerprint,
			kind:        RepetitionKindToolCall,
			toolName:    call.Name,
		})
	}
	return observations
}

func canonicalRepetitionJSON(value json.RawMessage) []byte {
	decoder := json.NewDecoder(strings.NewReader(string(value)))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return value
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return value
	}
	return canonical
}
