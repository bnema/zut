package session

import (
	"github.com/bnema/zut/packages/agent/extproto"
	"github.com/bnema/zut/packages/core"
	"time"
)

// EventKind identifies a session notification.
type EventKind string

const (
	// EvAgent carries a core.AgentEvent in Event.Agent.
	EvAgent EventKind = "agent"
	// EvInfo carries InfoEvent in Event.Data.
	EvInfo EventKind = "info"
	// EvQueue carries QueueEvent in Event.Data.
	EvQueue EventKind = "queue"
	// EvPending carries PendingEvent in Event.Data.
	EvPending EventKind = "pending"
	// EvPendingResolved carries PendingResolvedEvent in Event.Data.
	EvPendingResolved EventKind = "pending_resolved"
	// EvDeferred carries DeferredEvent in Event.Data.
	EvDeferred EventKind = "deferred"
	// EvNote carries NoteEvent in Event.Data.
	EvNote EventKind = "note"
	// EvExtChrome carries ExtChromeEvent in Event.Data.
	EvExtChrome EventKind = "ext_chrome"
	// EvExtPanel carries ExtPanelEvent in Event.Data.
	EvExtPanel EventKind = "ext_panel"
	// EvExtAlert carries ExtAlertEvent in Event.Data.
	EvExtAlert EventKind = "ext_alert"
	// EvExtDisplay carries ExtDisplayEvent in Event.Data.
	EvExtDisplay EventKind = "ext_display"
	// EvExtInsert carries ExtInsertEvent in Event.Data.
	EvExtInsert EventKind = "ext_insert"
	// EvPlan carries PlanEvent in Event.Data.
	EvPlan EventKind = "plan"
	// EvGoal carries GoalEvent in Event.Data.
	EvGoal EventKind = "goal"
	// EvTitle carries TitleEvent in Event.Data.
	EvTitle EventKind = "title"
	// EvCompaction carries CompactionEvent in Event.Data.
	EvCompaction EventKind = "compaction"
	// EvMessagesReplaced carries MessagesReplacedEvent in Event.Data.
	EvMessagesReplaced EventKind = "messages_replaced"
	// EvInterSession carries InterSessionEvent in Event.Data.
	EvInterSession EventKind = "inter_session"
	// EvAttach carries AttachEvent in Event.Data.
	EvAttach EventKind = "attach"
	// EvClosed carries ClosedEvent in Event.Data.
	EvClosed EventKind = "closed"
)

// Event is a terminal-free session contract payload.
type Event struct {
	Seq     uint64          `json:"seq"`
	Session ID              `json:"session"`
	At      time.Time       `json:"at"`
	Kind    EventKind       `json:"kind"`
	Agent   core.AgentEvent `json:"agent"`
	Data    any             `json:"data"`
}

// InfoEvent is a terminal-free session contract payload.
type InfoEvent struct {
	Info Info `json:"info"`
}

// QueueEvent is a terminal-free session contract payload.
type QueueEvent struct {
	Queue []QueuedInput `json:"queue"`
}

// PendingEvent is a terminal-free session contract payload.
type PendingEvent struct {
	Pending PendingRequest `json:"pending"`
}

// PendingResolvedEvent is a terminal-free session contract payload.
type PendingResolvedEvent struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
}

// DeferredEvent is a terminal-free session contract payload.
type DeferredEvent struct {
	Deferred DeferredAction `json:"deferred"`
}

// NoteEvent is a terminal-free session contract payload.
type NoteEvent struct {
	Note Note `json:"note"`
}

// ExtChromeEvent is a terminal-free session contract payload.
type ExtChromeEvent struct {
	ExtChrome ExtChrome `json:"ext_chrome"`
}

// ExtPanelEvent is a terminal-free session contract payload.
type ExtPanelEvent struct {
	Op    string   `json:"op"`
	Panel ExtPanel `json:"panel"`
}

// ExtAlertEvent is a terminal-free session contract payload.
type ExtAlertEvent struct {
	Ext   string                `json:"ext,omitempty"`
	Alert extproto.AlertRequest `json:"alert"`
}

// ExtDisplayEvent is a terminal-free session contract payload.
type ExtDisplayEvent struct {
	Ext  string `json:"ext"`
	Text string `json:"text"`
}

// ExtInsertEvent is a terminal-free session contract payload.
type ExtInsertEvent struct {
	// Submit asks the attached client to execute the inserted input.
	Submit bool   `json:"submit,omitempty"`
	Text   string `json:"text"`
}

// PlanEvent is a terminal-free session contract payload.
type PlanEvent struct {
	Plan map[string]core.PlanUpdate `json:"plan"`
}

// GoalEvent is a terminal-free session contract payload.
type GoalEvent struct {
	Goal   *core.SessionGoal `json:"goal"`
	Status string            `json:"status"`
}

// TitleEvent is a terminal-free session contract payload.
type TitleEvent struct {
	Title string `json:"title"`
}

// CompactionEvent is a terminal-free session contract payload.
type CompactionEvent struct {
	Phase   string `json:"phase"`
	Summary string `json:"summary"`
}

// MessagesReplacedEvent instructs clients to fetch a fresh snapshot.
type MessagesReplacedEvent struct{}

// InterSessionEvent is a terminal-free session contract payload.
type InterSessionEvent struct {
	From Sender `json:"from"`
	Text string `json:"text"`
}

// AttachEvent is a terminal-free session contract payload.
type AttachEvent struct {
	State  AttachState `json:"state"`
	Reason string      `json:"reason"`
}

// ClosedEvent is a terminal-free session contract payload.
type ClosedEvent struct {
	Reason    string `json:"reason"`
	Successor ID     `json:"successor"`
}
