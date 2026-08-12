package subagents

import "time"

// Operation is a factual operation inferred from paired trace boundaries. An
// empty FinishedAt means that no terminal boundary has been observed yet.
type Operation struct {
	Type       string
	AgentID    string
	TurnID     string
	StartedAt  time.Time
	FinishedAt time.Time
	Outcome    string
}

// Open reports whether the trace contains a start boundary without a matching
// terminal boundary.
func (o Operation) Open() bool { return o.FinishedAt.IsZero() }

// Duration measures an observed operation. Open operations are measured at
// now; callers supply now to make rendering and tests deterministic.
func (o Operation) Duration(now time.Time) time.Duration {
	end := o.FinishedAt
	if end.IsZero() {
		end = now
	}
	if end.Before(o.StartedAt) {
		return 0
	}
	return end.Sub(o.StartedAt)
}

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
			open[event.AgentID][key] = Operation{Type: event.Type, AgentID: event.AgentID, TurnID: event.TurnID, StartedAt: event.Timestamp}
		}
		if ends {
			if operation, ok := open[event.AgentID][key]; ok {
				operation.FinishedAt = event.Timestamp
				operation.Outcome = event.Type
				delete(open[event.AgentID], key)
			}
		}
		if terminal != "" {
			view.Terminal = terminal
		}
		views[event.AgentID] = view
	}
	for agentID, operations := range open {
		view := views[agentID]
		for _, operation := range operations {
			view.OpenOperations = append(view.OpenOperations, operation)
		}
		views[agentID] = view
	}
	return views
}

func traceBoundary(event TraceEvent) (key string, starts, ends bool, terminal string) {
	key = event.Type + ":" + event.TurnID
	switch event.Type {
	case "turn.started":
		return "turn:" + event.TurnID, true, false, ""
	case "turn.finished", "turn.failed":
		return "turn:" + event.TurnID, false, true, ""
	case "tool.started":
		return "tool:" + event.TurnID, true, false, ""
	case "tool.finished":
		return "tool:" + event.TurnID, false, true, ""
	case "agent.finished":
		return key, false, false, "completed"
	case "agent.failed":
		return key, false, false, "failed"
	}
	return key, false, false, ""
}
