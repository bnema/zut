package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// SessionsToolName is the stable built-in inter-session tool name.
const SessionsToolName = "sessions"

// SessionEntry is the tool-local view of one open daemon session. It mirrors
// the fields of session.Info the model needs; tools must not import session.
type SessionEntry struct {
	ID       string
	Title    string
	CWD      string
	Attach   string
	Run      string
	Activity string
	Model    string
}

// SessionDirectory is the daemon directory bound to the calling session.
// Send delivers text from the calling session to the session with ID to.
type SessionDirectory interface {
	List(ctx context.Context) ([]SessionEntry, error)
	Tree(ctx context.Context) (string, error)
	Send(ctx context.Context, to, text string) error
}

var (
	// ErrSenderDetached reports a send from a session without an attached user.
	ErrSenderDetached = errors.New("sending requires an attached session (the user must be present)")
	// ErrSenderLoop reports a send from a turn started by an inter-session message.
	ErrSenderLoop = errors.New("cannot send from a turn started by another session's message")
)

const sessionsToolSchema = `{"type":"object","properties":{"action":{"type":"string","enum":["list","tree","send"],"description":"list shows open sessions; tree groups them by workspace with their sub-agents, plan step and goal; send delivers a message to another session."},"to":{"type":"string","description":"Target session: full ID, unique ID prefix, or exact title. Required for send."},"text":{"type":"string","description":"Message text. Required for send."}},"required":["action"],"additionalProperties":false}`

// SessionsTool lists the daemon's open sessions and sends messages between
// them. Directory is bound to the calling session.
type SessionsTool struct {
	Directory SessionDirectory
}

type sessionsToolArgs struct {
	Action string `json:"action"`
	To     string `json:"to,omitempty"`
	Text   string `json:"text,omitempty"`
}

func (t *SessionsTool) Name() string { return SessionsToolName }

func (t *SessionsTool) Description() string {
	return "List, inspect, and message the other open zut sessions of this daemon. One action per call. " +
		"list shows each session's ID, attach state, run state, activity, model, working directory and title. " +
		"tree groups sessions by workspace (repository root or working directory) and shows each session's resident sub-agents, current plan step and goal. " +
		"send delivers text to another session as a tagged inter-session message; `to` is a session ID, a unique ID prefix, or an exact title. The target may be detached. " +
		"Rules: list and tree are always allowed. send requires this session to be attached (the user must be present), and it is refused during a turn started by another session's message, so sessions cannot loop messages between themselves."
}

func (t *SessionsTool) Schema() json.RawMessage { return json.RawMessage(sessionsToolSchema) }

func (t *SessionsTool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	if t == nil || t.Directory == nil {
		return toolErr("sessions: directory is unavailable"), nil
	}
	var args sessionsToolArgs
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return toolErr("sessions: invalid arguments"), nil
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return toolErr("sessions: invalid arguments"), nil
	}
	switch strings.TrimSpace(args.Action) {
	case "list":
		if args.To != "" || args.Text != "" {
			return toolErr("sessions: to and text are only valid for action send"), nil
		}
		entries, err := t.Directory.List(ctx)
		if err != nil {
			return toolErr("sessions: " + err.Error()), nil
		}
		return sessionsText(formatSessionEntries(entries)), nil
	case "tree":
		if args.To != "" || args.Text != "" {
			return toolErr("sessions: to and text are only valid for action send"), nil
		}
		tree, err := t.Directory.Tree(ctx)
		if err != nil {
			return toolErr("sessions: " + err.Error()), nil
		}
		if strings.TrimSpace(tree) == "" {
			tree = "no open sessions"
		}
		return sessionsText(tree), nil
	case "send":
		return t.send(ctx, strings.TrimSpace(args.To), args.Text)
	case "":
		return toolErr("sessions: action is required"), nil
	default:
		return toolErr("sessions: unknown action"), nil
	}
}

func (t *SessionsTool) send(ctx context.Context, to, text string) (core.ToolResult, error) {
	if to == "" {
		return toolErr("sessions: to is required for send"), nil
	}
	if strings.TrimSpace(text) == "" {
		return toolErr("sessions: text is required for send"), nil
	}
	entries, err := t.Directory.List(ctx)
	if err != nil {
		return toolErr("sessions: " + err.Error()), nil
	}
	target, err := resolveSessionTarget(entries, to)
	if err != nil {
		return toolErr("sessions: " + err.Error()), nil
	}
	if err := t.Directory.Send(ctx, target.ID, text); err != nil {
		switch {
		case errors.Is(err, ErrSenderDetached):
			return toolErr(ErrSenderDetached.Error()), nil
		case errors.Is(err, ErrSenderLoop):
			return toolErr(ErrSenderLoop.Error()), nil
		}
		return toolErr("sessions: send: " + err.Error()), nil
	}
	return sessionsText(fmt.Sprintf("message sent to session %s", target.ID)), nil
}

// resolveSessionTarget matches an exact ID, then a unique ID prefix, then a
// unique exact title. A unique ID prefix that is also the exact title of a
// different session is ambiguous.
func resolveSessionTarget(entries []SessionEntry, to string) (SessionEntry, error) {
	for _, e := range entries {
		if e.ID == to {
			return e, nil
		}
	}
	var prefix []SessionEntry
	for _, e := range entries {
		if strings.HasPrefix(e.ID, to) {
			prefix = append(prefix, e)
		}
	}
	var titled []SessionEntry
	for _, e := range entries {
		if e.Title != "" && e.Title == to {
			titled = append(titled, e)
		}
	}
	if len(prefix) > 1 {
		return SessionEntry{}, fmt.Errorf("session prefix %q is ambiguous: %s", to, joinSessionIDs(prefix))
	}
	if len(prefix) == 1 {
		for _, e := range titled {
			if e.ID != prefix[0].ID {
				return SessionEntry{}, fmt.Errorf("%q is ambiguous: ID prefix of %s and title of %s", to, prefix[0].ID, e.ID)
			}
		}
		return prefix[0], nil
	}
	switch len(titled) {
	case 1:
		return titled[0], nil
	case 0:
		return SessionEntry{}, fmt.Errorf("no open session matches %q", to)
	default:
		return SessionEntry{}, fmt.Errorf("session title %q is ambiguous: %s", to, joinSessionIDs(titled))
	}
}

func joinSessionIDs(entries []SessionEntry) string {
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return strings.Join(ids, ", ")
}

func formatSessionEntries(entries []SessionEntry) string {
	if len(entries) == 0 {
		return "no open sessions"
	}
	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteByte('\n')
		}
		title := SanitizeSessionText(e.Title)
		if title == "" {
			title = "(untitled)"
		}
		fmt.Fprintf(&b, "%s  %s  %s  %s  %s  %s  %s", e.ID, orDash(e.Attach), orDash(e.Run), orDash(e.Activity), orDash(SanitizeSessionText(e.Model)), orDash(SanitizeSessionText(e.CWD)), title)
	}
	return b.String()
}

// SanitizeSessionText replaces control characters, including newlines, with
// spaces so a session-supplied field cannot fake extra lines in a listing.
func SanitizeSessionText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func sessionsText(text string) core.ToolResult {
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: text}}}
}
