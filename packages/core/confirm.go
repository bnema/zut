package core

import (
	"encoding/json"
	"strings"
	"sync"
)

// ConfirmDecision is the outcome of a confirmation prompt for a
// tool call. It answers two questions: should this call run, and
// should future calls of this shape skip the prompt for the rest
// of the session.
type ConfirmDecision struct {
	Allow bool
	// Reason is shown to the model as the tool error when
	// Allow=false. Examples: "user declined", "user refused: rm -rf
	// looks dangerous".
	Reason string
	// RememberTool, when true, auto-allows every future call with the
	// same confirmation key for the rest of the session without prompting.
	// The key is the tool name unless the tool implements ConfirmationKeyer
	// to expose a finer grant (for example one action of a multiplexed tool).
	RememberTool bool
	// RememberAll, when true, auto-allows every future call of any
	// tool for the rest of the session without prompting.
	// Effectively turns yolo back on for this session.
	RememberAll bool
}

// Confirmer asks the user to approve or refuse a single tool call.
// Implementations block until the user responds (or the agent's
// context is cancelled, in which case they should return
// Allow=false with a cancellation reason).
//
// preview is a short one-line summary of the args (the shell
// command, the file path, the URL) that the TUI shows alongside
// the tool name. It is intentionally short: no full tool outputs.
type Confirmer interface {
	Confirm(toolName string, preview string) ConfirmDecision
}

// ToolCallConfirmation contains the summary and optional side-effect-free
// content shown while approving a specific tool call.
type ToolCallConfirmation struct {
	ID      string
	Name    string
	Summary string
	Content string
	// Origin routes a confirmation to its owning host view. It is opaque to
	// the confirmation gate and never shown to the model.
	Origin string
	// Key is the session-scoped identity the gate remembers for "always
	// allow". It lets one tool expose several confirmable actions
	// ("subagent:status") without granting the others. Empty falls back to
	// Name so every other tool keeps the per-name grant.
	Key string
}

// ConfirmationKeyer is optionally implemented by tools whose confirmation
// grant should be narrower than the tool name. The returned key is what the
// ConfirmGate stores for "always allow".
//
// Implementations must fail closed: when the effective key cannot be derived
// from args, return an empty string so the gate falls back to the tool name,
// which never widens an existing grant.
type ConfirmationKeyer interface {
	ConfirmationKey(args json.RawMessage) string
}

// ConfirmationKey resolves the session grant identity for one tool call. A
// tool that implements ConfirmationKeyer may report a finer key (for example
// one action of a multiplexed tool); an empty key falls back to name, so an
// undetermined key cannot widen the remembered grant beyond the tool name.
func ConfirmationKey(tool Tool, name string, args json.RawMessage) string {
	if keyer, ok := tool.(ConfirmationKeyer); ok {
		if key := keyer.ConfirmationKey(args); key != "" {
			return key
		}
	}
	return name
}

// ToolCallConfirmer is optionally implemented by confirmation UIs that can
// attach rich preview content to the matching live tool call.
type ToolCallConfirmer interface {
	ConfirmToolCall(call ToolCallConfirmation) ConfirmDecision
}

// ConfirmGate wraps a Confirmer with session-scoped memory for the
// "allow, always" decisions. Once the user picks RememberTool on a
// given confirmation key (the tool name, or a finer per-action key when
// the tool implements ConfirmationKeyer), the gate short-circuits
// subsequent calls with that key. Once the user picks RememberAll, the
// gate short-circuits everything for the rest of the session.
//
// Safe for concurrent use: the allow-lists are guarded by a mutex
// because the agent can queue tool calls from different goroutines.
type ConfirmGate struct {
	inner Confirmer

	mu          sync.Mutex
	allowAll    bool
	allowedTool map[string]bool
}

// NewConfirmGate returns a gate backed by inner. Inner can be nil;
// in that case every not-yet-allowed tool call is refused with a
// fixed reason (the gate is effectively a blocker until AllowAll /
// SetConfirmer is called).
func NewConfirmGate(inner Confirmer) *ConfirmGate {
	return &ConfirmGate{
		inner:       inner,
		allowedTool: map[string]bool{},
	}
}

// Check is the BeforeToolExecute-style entry point. Returns
// allowed, reason, modifiedArgs. modifiedArgs is always nil: the
// gate never rewrites args; it only allows or denies.
//
// A nil ConfirmGate always allows (treat as yolo mode).
func (g *ConfirmGate) Check(toolName, preview string) (bool, string, json.RawMessage) {
	return g.CheckToolCall(ToolCallConfirmation{Name: toolName, Summary: preview})
}

// CheckToolCall behaves like Check and additionally gives rich confirmation
// UIs the call ID and side-effect-free preview content.
func (g *ConfirmGate) CheckToolCall(call ToolCallConfirmation) (bool, string, json.RawMessage) {
	if g == nil {
		return true, "", nil
	}
	g.mu.Lock()
	if g.allowAll {
		g.mu.Unlock()
		return true, "", nil
	}
	if g.allowedTool[call.confirmationKey()] {
		g.mu.Unlock()
		return true, "", nil
	}
	inner := g.inner
	g.mu.Unlock()
	if inner == nil {
		return false, "tool call refused: --no-yolo is active and there is no interactive prompt in this mode; ask the user what to do instead", nil
	}

	var decision ConfirmDecision
	if detailed, ok := inner.(ToolCallConfirmer); ok {
		decision = detailed.ConfirmToolCall(call)
	} else {
		decision = inner.Confirm(call.Name, call.Summary)
	}

	g.mu.Lock()
	if decision.Allow {
		if decision.RememberAll {
			g.allowAll = true
		}
		if decision.RememberTool {
			g.allowedTool[call.confirmationKey()] = true
		}
	}
	g.mu.Unlock()

	reason := strings.TrimSpace(decision.Reason)
	if !decision.Allow && reason == "" {
		reason = "tool call refused by user"
	}
	return decision.Allow, reason, nil
}

// confirmationKey is the grant identity for the call: an explicit action-aware
// key when the caller supplied one, else the registered tool name.
func (call ToolCallConfirmation) confirmationKey() string {
	if call.Key != "" {
		return call.Key
	}
	return call.Name
}

// Reset clears the session memory. Invoked when the user toggles
// yolo mode back on via /yolo or closes the session.
func (g *ConfirmGate) Reset() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.allowAll = false
	g.allowedTool = map[string]bool{}
	g.mu.Unlock()
}

// AllowAll flips the gate into "always allow" for the rest of the
// session. Used by /yolo on to turn confirmation off at runtime
// without having to restart with the flag flipped.
func (g *ConfirmGate) AllowAll() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.allowAll = true
	g.mu.Unlock()
}

// SetConfirmer replaces the inner Confirmer. Used by the cli to
// hand the Interactive TUI to a gate that was constructed before
// the TUI existed (a construction-order knot we unwind here).
// Safe to call from any goroutine.
func (g *ConfirmGate) SetConfirmer(c Confirmer) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.inner = c
	g.mu.Unlock()
}

// BuildPreview turns a tool call's JSON args into a short one-line
// summary the TUI can show in the confirmation prompt. Prioritises
// obvious human-readable fields (command, path, url) over raw JSON.
// Returns at most maxLen characters.
func BuildPreview(args json.RawMessage, maxLen int) string {
	if maxLen <= 0 {
		maxLen = 120
	}
	if len(args) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return truncatePreview(string(args), maxLen)
	}
	for _, k := range []string{"command", "path", "file_path", "url", "query", "name"} {
		if v, ok := m[k].(string); ok && v != "" {
			return truncatePreview(v, maxLen)
		}
	}
	b, _ := json.Marshal(m)
	return truncatePreview(string(b), maxLen)
}

func truncatePreview(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return "..."[:n]
	}
	return s[:n-3] + "..."
}
