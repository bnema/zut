package subagents

import (
	"sort"
	"strings"
	"time"
)

// Operation is a currently open factual operation inferred from its start
// boundary. Completed operations are deliberately not retained in the public
// projection: their terminal trace fact remains available as LastEvent.
type Operation struct {
	Type      string
	AgentID   string
	TurnID    string
	CallID    string
	StartedAt time.Time
}

// Open is always true because the projection exposes only unmatched starts.
func (o Operation) Open() bool { return true }

// Duration measures this open operation at now; callers supply now to make
// rendering and tests deterministic.
func (o Operation) Duration(now time.Time) time.Duration {
	if now.Before(o.StartedAt) {
		return 0
	}
	return now.Sub(o.StartedAt)
}

// Label returns the shared concise label for an open operation.
func (o Operation) Label() string { return strings.TrimSuffix(o.Type, ".started") + " open" }

// AgentTraceView is the only user-facing execution state. It intentionally
// contains operations and terminal facts rather than lifecycle guesses such as
// working, idle, or alive.
type AgentTraceView struct {
	AgentID        string
	LastEvent      TraceEvent
	OpenOperations []Operation
	Terminal       string
}

// ProjectTrace reduces a chronologically ordered trace into per-agent facts.
// Unknown events are retained as LastEvent but never guessed into an operation.
func ProjectTrace(events []TraceEvent) map[string]AgentTraceView {
	views := make(map[string]AgentTraceView)
	open := make(map[string]map[string]Operation)
	for _, event := range events {
		if event.AgentID == "" {
			continue
		}
		view := views[event.AgentID]
		view.AgentID = event.AgentID
		view.LastEvent = event
		key, starts, ends, terminal := traceBoundary(event)
		if starts {
			if open[event.AgentID] == nil {
				open[event.AgentID] = make(map[string]Operation)
			}
			callID, _ := event.Data["call_id"].(string)
			open[event.AgentID][key] = Operation{Type: event.Type, AgentID: event.AgentID, TurnID: event.TurnID, CallID: callID, StartedAt: event.Timestamp}
		}
		if ends {
			delete(open[event.AgentID], key)
		}
		if terminal != "" {
			view.Terminal = terminal
			delete(open, event.AgentID)
		}
		views[event.AgentID] = view
	}
	for agentID, operations := range open {
		view := views[agentID]
		for _, operation := range operations {
			view.OpenOperations = append(view.OpenOperations, operation)
		}
		sort.Slice(view.OpenOperations, func(i, j int) bool {
			left, right := view.OpenOperations[i], view.OpenOperations[j]
			if !left.StartedAt.Equal(right.StartedAt) {
				return left.StartedAt.Before(right.StartedAt)
			}
			if left.Type != right.Type {
				return left.Type < right.Type
			}
			if left.TurnID != right.TurnID {
				return left.TurnID < right.TurnID
			}
			return left.CallID < right.CallID
		})
		views[agentID] = view
	}
	return views
}

func traceBoundary(event TraceEvent) (key string, starts, ends bool, terminal string) {
	callID, _ := event.Data["call_id"].(string)
	key = event.Type + ":" + event.TurnID + ":" + callID
	switch event.Type {
	case "turn.started":
		return "turn:" + event.TurnID, true, false, ""
	case "turn.finished", "turn.failed", "turn.cancelled":
		return "turn:" + event.TurnID, false, true, ""
	case "tool.started":
		return "tool:" + event.TurnID + ":" + callID, true, false, ""
	case "tool.finished", "tool.failed", "tool.cancelled":
		return "tool:" + event.TurnID + ":" + callID, false, true, ""
	case "agent.finished":
		return key, false, false, "completed"
	case "agent.failed":
		return key, false, false, "failed"
	case "agent.cancelled":
		return key, false, false, "cancelled"
	}
	return key, false, false, ""
}
