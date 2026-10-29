// Package daemonproto defines version 1 local daemon NDJSON frames.
// Transport and lossless agent-event codecs are implemented separately.
package daemonproto

import (
	"encoding/json"
	"github.com/bnema/zut/packages/agent/session"
)

// ProtocolVersion is the required wire protocol version.
const ProtocolVersion = 1

// Envelope permits frame dispatch before decoding the concrete frame.
// IDs are opaque strings echoed by results; notifications omit them.
type Envelope struct {
	ID      string     `json:"id,omitempty"`
	Type    string     `json:"type"`
	Session session.ID `json:"session,omitempty"`
}

// HelloRequest is the hello frame. Type must be "hello".
type HelloRequest struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocol_version"`
	ClientVersion   string `json:"client_version"`
	ClientKind      string `json:"client_kind"`
	PID             int    `json:"pid"`
}

// CreateRequest is the create frame. Type must be "create".
type CreateRequest struct {
	ID   string             `json:"id"`
	Type string             `json:"type"`
	Spec session.CreateSpec `json:"spec"`
}

// AttachRequest is the attach frame. Type must be "attach".
type AttachRequest struct {
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	Session     session.ID `json:"session"`
	AfterSeq    uint64     `json:"after_seq"`
	Controlling bool       `json:"controlling"`
}

// DetachRequest is the detach frame. Type must be "detach".
type DetachRequest struct {
	ID      string     `json:"id"`
	Type    string     `json:"type"`
	Session session.ID `json:"session"`
}

// CloseRequest is the close frame. Type must be "close".
type CloseRequest struct {
	ID      string     `json:"id"`
	Type    string     `json:"type"`
	Session session.ID `json:"session"`
	Reason  string     `json:"reason"`
}

// ClearRequest is the clear frame. Type must be "clear".
type ClearRequest struct {
	ID      string     `json:"id"`
	Type    string     `json:"type"`
	Session session.ID `json:"session"`
}

// ListRequest is the list frame. Type must be "list".
type ListRequest struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// TreeRequest is the tree frame. Type must be "tree".
type TreeRequest struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// SnapshotRequest is the snapshot frame. Type must be "snapshot".
type SnapshotRequest struct {
	ID      string     `json:"id"`
	Type    string     `json:"type"`
	Session session.ID `json:"session"`
}

// PortRequest is the port frame. Type must be "port".
type PortRequest struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Session session.ID      `json:"session"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// DaemonStopRequest is the daemon_stop frame. Type must be "daemon_stop".
type DaemonStopRequest struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	GracefulMS int    `json:"graceful_ms"`
}

// DaemonStatusRequest is the daemon_status frame. Type must be "daemon_status".
type DaemonStatusRequest struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// Result is the result frame. Type must be "result".
type Result struct {
	ID    string          `json:"id"`
	Type  string          `json:"type"`
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// EventNotification is the event frame. Type must be "event".
type EventNotification struct {
	Type    string            `json:"type"`
	Session session.ID        `json:"session"`
	Seq     uint64            `json:"seq"`
	Kind    session.EventKind `json:"kind"`
	Data    json.RawMessage   `json:"data"`
}

// DetachedByOtherNotification is the detached_by_other frame. Type must be "detached_by_other".
type DetachedByOtherNotification struct {
	Type    string     `json:"type"`
	Session session.ID `json:"session"`
}

// ClosedNotification is the closed frame. Type must be "closed".
type ClosedNotification struct {
	Type      string     `json:"type"`
	Session   session.ID `json:"session"`
	Successor session.ID `json:"successor,omitempty"`
}

// HelloData is successful hello result data. Version skew is a warning;
// ProtocolVersion mismatch is a hard error.
type HelloData struct {
	ProtocolVersion int    `json:"protocol_version"`
	DaemonVersion   string `json:"daemon_version"`
}

// DaemonStatusData mirrors the daemon status fields: PID, version, uptime and count.
type DaemonStatusData struct {
	PID      int    `json:"pid"`
	Version  string `json:"version"`
	UptimeMS int64  `json:"uptime_ms"`
	Sessions int    `json:"sessions"`
}
