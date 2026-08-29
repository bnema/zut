// Package extproto defines the JSON-over-stdin/stdout wire format
// spoken between zut and its extension subprocesses. Both the host
// (packages/agent/extensions) and the SDK (packages/agent/ext) marshal/
// unmarshal the same types, so changes here ripple through both.
//
// All frames are one JSON object terminated by a single LF. Object
// boundaries follow newline boundaries; no multi-line JSON.
//
// Direction conventions in this file:
//   - Type names ending in "FromExt" are sent by the extension to zut.
//   - Type names ending in "FromHost" are sent by zut to the extension.
//   - Names without a suffix are direction-neutral payloads or shared
//     value types.
//
// Every frame has a top-level Type discriminator. Optional ID is
// present on commands and on responses to commands so the sender can
// correlate; events and notifications never carry an ID.
package extproto

import "encoding/json"

// ProtocolVersion is the extension protocol major. Version 2 introduces the
// zut_version wire key and requires both sides to reject mismatched majors.
const ProtocolVersion = 2

type Frame struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

type HelloFromExt struct {
	Type            string   `json:"type"`
	ProtocolVersion int      `json:"protocol_version"`
	Name            string   `json:"name"`
	Version         string   `json:"version"`
	Capabilities    []string `json:"capabilities,omitempty"`
}

type RegisterCommandFromExt struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type RegisterToolFromExt struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
	Deferred    bool            `json:"deferred,omitempty"`
}

type ReadyFromExt struct {
	Type string `json:"type"`
}

type SubscribeFromExt struct {
	Type      string   `json:"type"`
	Events    []string `json:"events,omitempty"`
	Intercept []string `json:"intercept,omitempty"`
}

type EventInterceptResponseFromExt struct {
	Type         string          `json:"type"`
	ID           string          `json:"id"`
	Block        bool            `json:"block,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	ModifiedArgs json.RawMessage `json:"modified_args,omitempty"`
	ReplaceText  string          `json:"replace_text,omitempty"`
	// Context is hidden, bounded context for the upcoming model turn. It is
	// not appended to the transcript or shown as a user message.
	Context string `json:"context,omitempty"`
}

type ToolResultFromExt struct {
	Type          string         `json:"type"`
	ID            string         `json:"id"`
	Content       []ContentBlock `json:"content"`
	IsError       bool           `json:"is_error,omitempty"`
	ActivateTools []string       `json:"activate_tools,omitempty"`
	// Details is extension-owned metadata. The host never includes it in
	// provider requests, but may persist it with the active session.
	Details json.RawMessage `json:"details,omitempty"`
}

type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Data     string `json:"data,omitempty"`
}

type CommandResponseFromExt struct {
	Type      string     `json:"type"`
	ID        string     `json:"id"`
	Action    string     `json:"action"`
	Prompt    string     `json:"prompt,omitempty"`
	Insert    string     `json:"insert,omitempty"`
	Display   string     `json:"display,omitempty"`
	OpenPanel *PanelSpec `json:"open_panel,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type PanelSpec struct {
	ID     string   `json:"id"`
	Title  string   `json:"title,omitempty"`
	Lines  []string `json:"lines,omitempty"`
	Footer string   `json:"footer,omitempty"`
}

// OpenPanelFromExt is a spontaneous one-way frame an extension can send at
// any time to open an interactive panel. Unlike the open_panel action inside
// CommandResponseFromExt, this form is uncoupled from any command invocation
// and may be sent from a tool handler goroutine or any background context.
type OpenPanelFromExt struct {
	Type  string    `json:"type"` // "open_panel"
	Panel PanelSpec `json:"panel"`
}

type PanelRenderFromExt struct {
	Type    string   `json:"type"`
	PanelID string   `json:"panel_id"`
	Title   string   `json:"title,omitempty"`
	Lines   []string `json:"lines,omitempty"`
	Footer  string   `json:"footer,omitempty"`
}

type PanelCloseFromExt struct {
	Type    string `json:"type"`
	PanelID string `json:"panel_id"`
}

// StatusFromExt sets or replaces one persistent status item owned by an
// extension. An empty text clears the item.
type StatusFromExt struct {
	Type  string `json:"type"`
	Key   string `json:"key"`
	Level string `json:"level,omitempty"`
	Text  string `json:"text,omitempty"`
}

const (
	WidgetPositionAboveInput = "above_input"
	// WidgetPositionRightBar is retained for source compatibility. The right
	// sidebar no longer exists, so this placement resolves above the input.
	WidgetPositionRightBar = WidgetPositionAboveInput
)

// NormalizeWidgetPosition keeps extension widgets in the single supported
// above-input placement. Unknown and removed placement hints remain compatible.
func NormalizeWidgetPosition(string) string { return WidgetPositionAboveInput }

// WidgetFromExt sets or replaces one persistent widget owned by an
// extension. Position is a host-defined placement hint. Interactive hosts
// render widgets above the input; empty and unknown values use that placement.
type WidgetFromExt struct {
	Type     string   `json:"type"`
	ID       string   `json:"id"`
	Position string   `json:"position,omitempty"`
	Title    string   `json:"title,omitempty"`
	Lines    []string `json:"lines,omitempty"`
}

type ClearWidgetFromExt struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

const AlertKindBell = "bell"

// AlertRequest is a host-owned alert request. Extensions provide semantic
// metadata; the host decides whether and how to render it.
type AlertRequest struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason,omitempty"`
}

// AlertFromExt is a spontaneous one-way frame an extension can send at any
// time to request a host alert.
type AlertFromExt struct {
	Type string `json:"type"`
	AlertRequest
}

type NotifyFromExt struct {
	Type    string `json:"type"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// ClearNotesFromExt is a spontaneous frame an extension can send to
// retract every note it previously pushed via notify/display, so
// transient status lines (e.g. an approval prompt) do not stack up
// forever in the bottom-sticky notes block.
type ClearNotesFromExt struct {
	Type string `json:"type"`
}

// SubmitSlashFromExt is a spontaneous frame an extension can send at
// any time (typically from a panel_key handler) to invoke a slash
// command in the host's TUI as if the user had typed it. Text must
// start with '/'. Reserved for internal / opt-in extensions today;
// the wire format is stable but not yet exposed in the public docs.
type SubmitSlashFromExt struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// SubmitFromExt is a spontaneous frame an extension can send to queue
// a model prompt in the interactive host. The manager routes it through
// HostHooks.Submit, so the TUI handles busy state and turn startup.
type SubmitFromExt struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ShutdownAckFromExt struct {
	Type string `json:"type"`
}

type HelloAckFromHost struct {
	Type            string          `json:"type"`
	ProtocolVersion int             `json:"protocol_version"`
	ZutVersion      string          `json:"zut_version"`
	Provider        string          `json:"provider"`
	Model           string          `json:"model"`
	CWD             string          `json:"cwd"`
	ExtensionDir    string          `json:"extension_dir,omitempty"`
	DataDir         string          `json:"data_dir,omitempty"`
	Session         *SessionContext `json:"session,omitempty"`
}

// SessionContext identifies the active transcript branch. ID is the current
// session/branch id; ParentID is empty for a root session.
type SessionContext struct {
	ID        string `json:"id"`
	ParentID  string `json:"parent_id,omitempty"`
	Path      string `json:"path,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	ForkPoint int    `json:"fork_point,omitempty"`
}

type CommandInvokedFromHost struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

type ToolCallFromHost struct {
	Type string          `json:"type"`
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type EventFromHost struct {
	Type        string          `json:"type"`
	Event       string          `json:"event"`
	Step        int             `json:"step,omitempty"`
	Stop        string          `json:"stop,omitempty"`
	Error       string          `json:"error,omitempty"`
	ToolID      string          `json:"tool_id,omitempty"`
	ToolName    string          `json:"tool_name,omitempty"`
	ToolArgs    json.RawMessage `json:"tool_args,omitempty"`
	ToolPreview string          `json:"tool_preview,omitempty"`
	Text        string          `json:"text,omitempty"`
	Session     *SessionContext `json:"session,omitempty"`
	// State is private to the receiving extension. The manager selects the
	// matching extension-owned snapshot before sending the event.
	State json.RawMessage `json:"state,omitempty"`
}

type EventInterceptFromHost struct {
	Type     string          `json:"type"`
	ID       string          `json:"id"`
	Event    string          `json:"event"`
	ToolID   string          `json:"tool_id,omitempty"`
	ToolName string          `json:"tool_name,omitempty"`
	ToolArgs json.RawMessage `json:"tool_args,omitempty"`
	Step     int             `json:"step,omitempty"`
	Text     string          `json:"text,omitempty"`
}

type PanelKeyFromHost struct {
	Type    string `json:"type"`
	PanelID string `json:"panel_id"`
	Key     string `json:"key"`
	Text    string `json:"text,omitempty"`
}

type PanelResizeFromHost struct {
	Type    string `json:"type"`
	PanelID string `json:"panel_id"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
}

type PanelCloseFromHost struct {
	Type    string `json:"type"`
	PanelID string `json:"panel_id"`
}

type ShutdownFromHost struct {
	Type string `json:"type"`
}

func Encode(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
