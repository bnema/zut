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
	Name      string
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
func (o Operation) Label() string {
	label := strings.TrimSuffix(o.Type, ".started")
	if o.Name != "" {
		label += " " + o.Name
	}
	return label + " open"
}

// LiveObservation is the last safe, factual signal received from a worker.
// It intentionally contains no worker text, tool arguments, or output.
type LiveObservation struct {
	Type   string
	Source string
	At     time.Time
	TurnID string
	CallID string
	Name   string
}

// Label returns a concise safe description of this observation.
func (o LiveObservation) Label() string {
	switch o.Type {
	case "assistant.stream.observed":
		return "assistant streaming"
	case "reasoning.stream.observed":
		return "reasoning stream observed"
	case "tool.output.observed":
		if o.Name != "" {
			return "tool output observed " + o.Name
		}
		return "tool output observed"
	case "worker.protocol.observed":
		if o.Source != "" {
			return "worker " + o.Source
		}
		return "worker protocol event"
	default:
		return strings.ReplaceAll(o.Type, ".", " ")
	}
}

// ResultFact describes a durable result and whether the parent accepted its
// required-work notification. Available is not equivalent to Delivered.
type ResultFact struct {
	Available bool
	Ref       string
	Delivered bool
	Failed    bool
	At        time.Time
}

// AgentTraceView is the only user-facing execution state. It intentionally
// contains operations and terminal facts rather than lifecycle guesses such as
// working, idle, or alive.
type AgentTraceView struct {
	AgentID          string
	LastEvent        TraceEvent
	LastObservation  *LiveObservation
	OpenOperations   []Operation
	PrimaryOperation *Operation
	Terminal         string
	Result           *ResultFact
}

// Summary returns the most specific factual activity or terminal result fact.
func (v AgentTraceView) Summary() string {
	if v.Terminal != "" {
		if v.Result != nil && v.Result.Available {
			if v.Result.Delivered {
				return v.Terminal + " · result delivered"
			}
			if v.Result.Failed {
				return v.Terminal + " · result delivery failed"
			}
			return v.Terminal + " · result available"
		}
		return v.Terminal
	}
	primary := v.PrimaryOperation
	if primary == nil && len(v.OpenOperations) != 0 {
		primary = &v.OpenOperations[0]
	}
	if primary != nil {
		if observation := v.ObservationFor(*primary); observation != nil {
			return primary.Label() + " · " + observation.Label()
		}
		return primary.Label()
	}
	if v.LastEvent.Type != "" {
		return "last event " + v.LastEvent.Type
	}
	return "no observable operation"
}

// ProjectTrace reduces a chronologically ordered trace into per-agent facts.
// Unknown worker protocol events remain factual observations and are never
// guessed into operations.
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
			name, _ := event.Data["name"].(string)
			open[event.AgentID][key] = Operation{Type: event.Type, AgentID: event.AgentID, TurnID: event.TurnID, CallID: callID, Name: name, StartedAt: event.Timestamp}
		}
		if ends {
			delete(open[event.AgentID], key)
		}
		if observation := traceObservation(event); observation != nil {
			view.LastObservation = observation
		}
		if result := traceResultFact(event, view.Result); result != nil {
			view.Result = result
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
			if operationPriority(left) != operationPriority(right) {
				return operationPriority(left) < operationPriority(right)
			}
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
		if len(view.OpenOperations) != 0 {
			primary := view.OpenOperations[0]
			view.PrimaryOperation = &primary
		}
		views[agentID] = view
	}
	return views
}

// ObservationFor returns the last observation only when it belongs to this
// operation's turn. A prior turn's stream is not live activity for a resumed
// or subsequent turn.
func (v AgentTraceView) ObservationFor(operation Operation) *LiveObservation {
	if v.LastObservation == nil || operation.TurnID == "" || v.LastObservation.TurnID == "" || operation.TurnID != v.LastObservation.TurnID {
		return nil
	}
	return v.LastObservation
}

func operationPriority(o Operation) int {
	switch o.Type {
	case "tool.started":
		return 0
	case "provider.request.started":
		return 1
	case "agent.wait.started":
		return 2
	default:
		return 3
	}
}

func traceObservation(event TraceEvent) *LiveObservation {
	typeName := event.Type
	source, _ := event.Data["source_event"].(string)
	if source == "" {
		source, _ = event.Data["worker_event"].(string)
	}
	name, _ := event.Data["name"].(string)
	switch typeName {
	case "assistant.stream.observed", "reasoning.stream.observed", "tool.output.observed", "worker.protocol.observed":
		return &LiveObservation{Type: typeName, Source: source, At: event.Timestamp, TurnID: event.TurnID, Name: name}
	}
	return nil
}

func traceResultFact(event TraceEvent, previous *ResultFact) *ResultFact {
	if event.Type != "result.available" && event.Type != "result.delivered" && event.Type != "result.delivery.failed" {
		return previous
	}
	result := ResultFact{}
	if previous != nil {
		result = *previous
	}
	if ref, _ := event.Data["ref"].(string); ref != "" {
		result.Ref = ref
	}
	result.At = event.Timestamp
	switch event.Type {
	case "result.available":
		result.Available, result.Delivered, result.Failed = true, false, false
	case "result.delivered":
		result.Available, result.Delivered, result.Failed = true, true, false
	case "result.delivery.failed":
		result.Available, result.Failed = true, true
	}
	return &result
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
	case "provider.request.started":
		return "provider:" + event.TurnID, true, false, ""
	case "provider.request.finished", "provider.request.failed", "provider.request.cancelled":
		return "provider:" + event.TurnID, false, true, ""
	case "agent.wait.started":
		return "wait:" + event.TurnID, true, false, ""
	case "agent.wait.finished":
		return "wait:" + event.TurnID, false, true, ""
	case "agent.finished":
		return key, false, false, "completed"
	case "agent.failed":
		return key, false, false, "failed"
	case "agent.cancelled":
		return key, false, false, "cancelled"
	}
	return key, false, false, ""
}
