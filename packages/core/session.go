package core

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zut/packages/provider"
	"github.com/google/uuid"
)

// Session is a JSONL-backed conversation transcript tied to a cwd.
type Session struct {
	ID   string
	Path string
	Meta SessionMeta
	// Title is the effective title after applying append-only rename rows.
	// Meta.Title remains the original meta-line value for compatibility.
	Title string
	// ExtensionState contains the latest opaque state snapshot recorded by
	// each extension for this session/branch. It is never sent to providers.
	ExtensionState map[string]json.RawMessage
	writer         *os.File
	buf            *bufio.Writer

	// freshFile is true when the file was created by NewSession (this
	// process owns it) and false when OpenSession reopened an existing
	// transcript. Used by Close() to delete the file if the run never
	// appended any messages — prevents a flood of empty session files
	// from sessions the user opens then exits without prompting.
	freshFile bool

	// messagesAppended counts AppendMessage calls. Combined with
	// freshFile it tells Close() whether the session left any content
	// worth keeping.
	messagesAppended int

	retryMu               sync.Mutex
	pendingRetryLifecycle []RetryLifecycleRecord
}

// GoalStatus is the persisted lifecycle state of an autonomous session goal.
type GoalStatus string

const (
	GoalActive        GoalStatus = "active"
	GoalPaused        GoalStatus = "paused"
	GoalBlocked       GoalStatus = "blocked"
	GoalDone          GoalStatus = "done"
	GoalBudgetLimited GoalStatus = "budget_limited"
	GoalStalled       GoalStatus = "stalled"
)

// GoalOwner identifies who established a session goal. Missing owners in
// legacy session data are interpreted as user-owned.
type GoalOwner string

const (
	GoalOwnerUser    GoalOwner = "user"
	GoalOwnerManager GoalOwner = "manager"
)

// MissionStatus is the durable lifecycle state of the user intent that owns
// a linear sequence of manager and user goals.
type MissionStatus string

const (
	MissionActive    MissionStatus = "active"
	MissionPaused    MissionStatus = "paused"
	MissionCompleted MissionStatus = "completed"
	MissionBlocked   MissionStatus = "blocked"
)

// MissionSource identifies who established the mission objective.
type MissionSource string

const (
	MissionSourceUser    MissionSource = "user"
	MissionSourceManager MissionSource = "manager"
)

// SessionMission is the bounded, linear container for session goals.
type SessionMission struct {
	ID              string        `json:"id"`
	Objective       string        `json:"objective"`
	Status          MissionStatus `json:"status"`
	Source          MissionSource `json:"source"`
	ActiveGoalID    string        `json:"active_goal_id,omitempty"`
	TransitionCount int           `json:"transition_count,omitempty"`
	Reason          string        `json:"reason,omitempty"`
}

// SessionGoal is the concise autonomous objective attached to one session.
type SessionGoal struct {
	ID                         string     `json:"id,omitempty"`
	MissionID                  string     `json:"mission_id,omitempty"`
	Objective                  string     `json:"objective"`
	Status                     GoalStatus `json:"status"`
	Owner                      GoalOwner  `json:"owner,omitempty"`
	Ordinal                    int        `json:"ordinal,omitempty"`
	Reason                     string     `json:"reason,omitempty"`
	TokenBudget                *uint64    `json:"token_budget,omitempty"`
	TokensUsed                 uint64     `json:"tokens_used,omitempty"`
	ContinuationID             string     `json:"continuation_id,omitempty"`
	ConsecutiveNoProgressTurns int        `json:"consecutive_no_progress_turns,omitempty"`
}

// SessionMeta is written as the first line of every session file.
type SessionMeta struct {
	ID       string    `json:"id"`
	CWD      string    `json:"cwd"`
	Model    string    `json:"model"`
	Provider string    `json:"provider"`
	Started  time.Time `json:"started"`
	Version  string    `json:"version"`
	Title    string    `json:"title,omitempty"`
	// Timezone and TimezoneOffset capture the local semantics at session
	// start. They are optional so older session headers remain compatible.
	Timezone       string `json:"timezone,omitempty"`
	TimezoneOffset string `json:"timezone_offset,omitempty"`

	// CompactHandoff is opaque host-managed state for a live compaction
	// continuation. It is session metadata, never provider context.
	// Missing or invalid values are handled by the owning host as no handoff.
	CompactHandoff json.RawMessage `json:"compact_handoff,omitempty"`
	// Mission owns the linear history of autonomous goals. Legacy sessions with
	// only Goal are normalized on read.
	Mission *SessionMission `json:"mission,omitempty"`
	// Goal is the current autonomous objective for this session. Completed
	// goals remain inspectable until the user clears or replaces them.
	Goal *SessionGoal `json:"goal,omitempty"`
	// GoalHistory records prior durable goal states in chronological order.
	// It stays linear: there is only one current goal at a time.
	GoalHistory []SessionGoal `json:"goal_history,omitempty"`

	// Parent is the ID of the session this one was forked from, or
	// empty for top-level sessions. The tree picker walks parents
	// upward and sibling files (same cwd dir, same parent ID)
	// laterally to render the branch topology.
	Parent string `json:"parent,omitempty"`
	// ForkPoint is the 0-indexed message position within the parent
	// transcript where this branch diverges. Messages 0..ForkPoint-1
	// are copied from the parent verbatim; the user's next turn on
	// the child session continues from there.
	ForkPoint int `json:"fork_point,omitempty"`
	// HideFromSessions hides internal tree-navigation branches from the
	// flat /sessions picker while keeping them available to /session tree.
	HideFromSessions bool `json:"hide_from_sessions,omitempty"`
}

// sessionLine is the on-disk row type. Message is kept as a raw
// JSON message on reads (because Content is an interface slice that
// the default unmarshaler cannot reconstruct); it is written with a
// regular provider.Message value.
type sessionLine struct {
	Type           string                 `json:"type"`
	Title          string                 `json:"title,omitempty"`
	Generated      bool                   `json:"generated,omitempty"`
	Meta           *SessionMeta           `json:"meta,omitempty"`
	Message        *provider.Message      `json:"message,omitempty"`
	Messages       *[]provider.Message    `json:"messages,omitempty"`
	Usage          *provider.Usage        `json:"usage,omitempty"`
	Cumulative     *provider.Usage        `json:"cumulative,omitempty"`
	RetryLifecycle []RetryLifecycleRecord `json:"retry_lifecycle,omitempty"`
	Extension      string                 `json:"extension,omitempty"`
	State          json.RawMessage        `json:"state,omitempty"`
}

type sessionLineHead struct {
	Type string `json:"type"`
}

// SessionUsageCheckpoint records the cumulative usage known when a usage
// row was written. MessageCount uses the effective transcript (after the
// latest compaction and tool-pair repair), not the number of raw JSONL rows.
type SessionUsageCheckpoint struct {
	MessageCount         int
	Cumulative           provider.Usage
	CompactionGeneration int
}

// SessionSnapshot is the effective, in-memory view of a session file.
// Messages are the transcript that a resumed agent should use. The raw
// JSONL audit history is deliberately not exposed here because message
// indices used by tree navigation and branching must agree.
type SessionSnapshot struct {
	Meta                 SessionMeta
	Title                string
	Messages             []provider.Message
	ExtensionState       map[string]json.RawMessage
	UsageCheckpoints     []SessionUsageCheckpoint
	CompactionGeneration int
}

// SessionHistorySegment is one provider-valid transcript era. A compaction
// starts a new segment containing the replacement summary and preserved tail;
// earlier segments remain available for session-tree checkout without being
// used to resume the live agent.
type SessionHistorySegment struct {
	Compacted        bool
	Messages         []provider.Message
	UsageCheckpoints []SessionUsageCheckpoint
}

// SessionHistory contains every persisted transcript era in chronological
// order. Unlike SessionSnapshot, it intentionally retains segments replaced by
// compaction so navigation can fork from older user and assistant messages.
type SessionHistory struct {
	Meta     SessionMeta
	Segments []SessionHistorySegment
}

// SessionsDir returns the per-cwd sessions directory under root.
func SessionsDir(root, cwd string) string {
	sum := sha256.Sum256([]byte(cwd))
	short := hex.EncodeToString(sum[:8])
	return filepath.Join(root, "sessions", short)
}

// NewSession creates and opens a new session file under
// SessionsDir(root, cwd) with an autogenerated, time-stamped name.
func NewSession(root, cwd, providerName, model, version string) (*Session, error) {
	dir := SessionsDir(root, cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	name := fmt.Sprintf("%s-%s.jsonl", time.Now().UTC().Format("20060102-150405"), id[:8])
	p := filepath.Join(dir, name)
	return newSessionAt(p, cwd, providerName, model, version)
}

// NewSessionAtPath creates a session at an explicit file path. It is useful
// whenever a caller needs a location other than SessionsDir. Returns an error
// if the file already exists — use OpenSession for that case.
func NewSessionAtPath(path, cwd, providerName, model, version string) (*Session, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return newSessionAt(path, cwd, providerName, model, version)
}

// newSessionAt is the shared implementation. Both NewSession and
// NewSessionAtPath funnel through here so the meta-line layout,
// freshFile bookkeeping, and id format stay identical.
func newSessionAt(p, cwd, providerName, model, version string) (*Session, error) {
	id := uuid.NewString()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	timezone, timezoneOffset := localTimeMetadata(now)
	s := &Session{
		ID:   id,
		Path: p,
		Meta: SessionMeta{
			ID:             id,
			CWD:            cwd,
			Provider:       providerName,
			Model:          model,
			Started:        now.UTC(),
			Version:        version,
			Timezone:       timezone,
			TimezoneOffset: timezoneOffset,
		},
		writer:         f,
		buf:            bufio.NewWriter(f),
		freshFile:      true,
		ExtensionState: map[string]json.RawMessage{},
	}
	if err := s.writeLine(sessionLine{Type: "meta", Meta: &s.Meta}); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

func forEachJSONLLine(r io.Reader, fn func([]byte) error) error {
	return forEachJSONLLineContext(context.Background(), r, fn)
}

func forEachJSONLLineContext(ctx context.Context, r io.Reader, fn func([]byte) error) error {
	br := bufio.NewReader(r)
	for {
		if err := contextErr(ctx); err != nil {
			return err
		}
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			if len(line) > 0 {
				if ferr := fn(line); ferr != nil {
					return ferr
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// SessionUsage returns the most recent cumulative usage row stored in
// a session file. Sessions append one usage row per completed turn; the
// latest row's cumulative field is the session total. Missing usage rows
// are valid for old/empty sessions and return the zero value.
func SessionUsage(path string) (provider.Usage, error) {
	cum, _, err := SessionUsageDetail(path)
	return cum, err
}

// SessionUsageDetail returns the latest cumulative usage and the
// per-turn usage of the final completed turn. It reads through the same
// strict snapshot path as OpenSession, so a truncated or malformed session
// cannot silently reset the usage display.
func SessionUsageDetail(path string) (cumulative, lastTurn provider.Usage, err error) {
	snapshot, err := ReadSessionSnapshot(path)
	if err != nil {
		return provider.Usage{}, provider.Usage{}, err
	}
	if len(snapshot.UsageCheckpoints) == 0 {
		return provider.Usage{}, provider.Usage{}, nil
	}
	latestCheckpoint := snapshot.UsageCheckpoints[len(snapshot.UsageCheckpoints)-1]
	if latestCheckpoint.CompactionGeneration < snapshot.CompactionGeneration {
		// The latest usage row belongs to the pre-compaction context. Keep
		// cumulative cost and totals, but reset the live context gauge until
		// a usage row from the rebuilt transcript is persisted.
		return latestCheckpoint.Cumulative, provider.Usage{}, nil
	}
	last := latestCheckpoint.Cumulative
	var prev provider.Usage
	if len(snapshot.UsageCheckpoints) > 1 {
		prev = snapshot.UsageCheckpoints[len(snapshot.UsageCheckpoints)-2].Cumulative
	}
	lastTurn.InputTokens = nonNegDelta(last.InputTokens, prev.InputTokens)
	lastTurn.OutputTokens = nonNegDelta(last.OutputTokens, prev.OutputTokens)
	lastTurn.ReasoningTokens = nonNegDelta(last.ReasoningTokens, prev.ReasoningTokens)
	lastTurn.ReasoningTokensKnown = last.ReasoningTokensKnown
	lastTurn.CacheReadTokens = nonNegDelta(last.CacheReadTokens, prev.CacheReadTokens)
	lastTurn.CacheWriteTokens = nonNegDelta(last.CacheWriteTokens, prev.CacheWriteTokens)
	lastTurn.CostUSD = last.CostUSD - prev.CostUSD
	if lastTurn.CostUSD < 0 {
		lastTurn.CostUSD = 0
	}
	return last, lastTurn, nil
}

func nonNegDelta(cur, prev int) int {
	if cur < prev {
		return cur
	}
	return cur - prev
}

// ReadSessionSnapshot reads a complete session file and reconstructs the
// effective transcript. It is intentionally stricter than the lightweight
// listing helpers: a caller that is about to resume, render, or branch a
// session must not receive a plausible-looking partial transcript.
func ReadSessionSnapshot(path string) (SessionSnapshot, error) {
	return readSessionSnapshot(context.Background(), path)
}

func readSessionSnapshot(ctx context.Context, path string) (SessionSnapshot, error) {
	if err := contextErr(ctx); err != nil {
		return SessionSnapshot{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return SessionSnapshot{}, fmt.Errorf("session snapshot: open %q: %w", path, err)
	}
	defer f.Close()

	var snapshot SessionSnapshot
	var sawMeta bool
	generation := 0
	type rawCheckpoint struct {
		messageCount int
		cumulative   provider.Usage
		generation   int
	}
	var rawCheckpoints []rawCheckpoint
	titleFromRename := false
	extensionState := map[string]json.RawMessage{}

	err = forEachStrictJSONLLineContext(ctx, f, func(line []byte, lineNo int) error {
		var head sessionLineHead
		if err := json.Unmarshal(line, &head); err != nil {
			return fmt.Errorf("line %d: invalid JSON: %w", lineNo, err)
		}
		if head.Type == "" {
			return fmt.Errorf("line %d: missing row type", lineNo)
		}
		if !sawMeta && head.Type != "meta" {
			return fmt.Errorf("line %d: first row is not meta", lineNo)
		}

		switch head.Type {
		case "meta":
			var row sessionLine
			if err := json.Unmarshal(line, &row); err != nil {
				return fmt.Errorf("line %d: invalid meta row: %w", lineNo, err)
			}
			if row.Meta == nil || row.Meta.ID == "" {
				return fmt.Errorf("line %d: meta row has no session id", lineNo)
			}
			snapshot.Meta = *row.Meta
			if !titleFromRename && row.Meta.Title != "" {
				snapshot.Title = row.Meta.Title
			}
			sawMeta = true

		case "message":
			msg, err := hydrateMessage(line)
			if err != nil {
				return fmt.Errorf("line %d: invalid message row: %w", lineNo, err)
			}
			snapshot.Messages = append(snapshot.Messages, msg)

		case "compaction":
			compacted, err := hydrateCompaction(line)
			if err != nil {
				return fmt.Errorf("line %d: invalid compaction row: %w", lineNo, err)
			}
			snapshot.Messages = compacted
			generation++
			cumulative, err := hydrateCompactionUsage(line)
			if err != nil {
				return fmt.Errorf("line %d: invalid compaction usage: %w", lineNo, err)
			}
			if cumulative != nil {
				rawCheckpoints = append(rawCheckpoints, rawCheckpoint{
					messageCount: len(snapshot.Messages),
					cumulative:   *cumulative,
					generation:   generation,
				})
			}

		case "usage":
			var row struct {
				Usage      *provider.Usage `json:"usage"`
				Cumulative *provider.Usage `json:"cumulative"`
			}
			if err := json.Unmarshal(line, &row); err != nil {
				return fmt.Errorf("line %d: invalid usage row: %w", lineNo, err)
			}
			if row.Cumulative == nil {
				return fmt.Errorf("line %d: usage row has no cumulative usage", lineNo)
			}
			rawCheckpoints = append(rawCheckpoints, rawCheckpoint{
				messageCount: len(snapshot.Messages),
				cumulative:   *row.Cumulative,
				generation:   generation,
			})

		case "rename":
			var row struct {
				Title *string `json:"title"`
			}
			if err := json.Unmarshal(line, &row); err != nil {
				return fmt.Errorf("line %d: invalid rename row: %w", lineNo, err)
			}
			if row.Title == nil {
				return fmt.Errorf("line %d: rename row has no title", lineNo)
			}
			titleFromRename = true
			snapshot.Title = *row.Title

		case "extension_state":
			var row sessionLine
			if err := json.Unmarshal(line, &row); err != nil {
				return fmt.Errorf("line %d: invalid extension state row: %w", lineNo, err)
			}
			if row.Extension != "" && len(row.State) <= maxExtensionStateBytes {
				if len(row.State) == 0 || strings.TrimSpace(string(row.State)) == "null" {
					delete(extensionState, row.Extension)
				} else if json.Valid(row.State) {
					extensionState[row.Extension] = append(json.RawMessage(nil), row.State...)
				}
			}

		default:
			return fmt.Errorf("line %d: unknown row type %q", lineNo, head.Type)
		}
		return nil
	})
	if err != nil {
		return SessionSnapshot{}, fmt.Errorf("session snapshot %q: %w", path, err)
	}
	if !sawMeta {
		return SessionSnapshot{}, fmt.Errorf("session snapshot %q: file is empty", path)
	}

	normalizeSessionGoalMeta(&snapshot.Meta)
	snapshot.CompactionGeneration = generation
	snapshot.ExtensionState = extensionState

	// Repair only after compaction replacement. This makes synthetic tool
	// results and the removal of orphaned results part of the same indexed
	// stream that OpenSession returns and that BranchSession materializes.
	rawMessages := snapshot.Messages
	repaired := repairSessionMessages(rawMessages)
	snapshot.Messages = repaired.messages
	for _, checkpoint := range rawCheckpoints {
		count := checkpoint.messageCount
		if checkpoint.generation == generation {
			count = repaired.prefixCount(count)
		} else if count > len(rawMessages) {
			// A compaction replaces the effective stream. Preserve the
			// cumulative cost represented by an older checkpoint at the
			// end of the replacement rather than making every post-summary
			// branch appear to have zero usage.
			count = len(rawMessages)
		}
		snapshot.UsageCheckpoints = append(snapshot.UsageCheckpoints, SessionUsageCheckpoint{
			MessageCount:         count,
			Cumulative:           checkpoint.cumulative,
			CompactionGeneration: checkpoint.generation,
		})
	}
	return snapshot, nil
}

// ReadSessionHistory reads every provider-valid transcript era in a session.
// The effective snapshot remains the source of truth for resume and ordinary
// branching; this audit projection exists for session-tree navigation so a
// compaction cannot make earlier fork points disappear.
func ReadSessionHistory(path string) (SessionHistory, error) {
	return readSessionHistory(context.Background(), path)
}

// ReadSessionHistoryContext is the cancellation-aware counterpart to
// ReadSessionHistory. It is intended for background consumers such as the
// interactive session-tree loader.
func ReadSessionHistoryContext(ctx context.Context, path string) (SessionHistory, error) {
	return readSessionHistory(ctx, path)
}

func readSessionHistory(ctx context.Context, path string) (SessionHistory, error) {
	if err := contextErr(ctx); err != nil {
		return SessionHistory{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return SessionHistory{}, fmt.Errorf("session history: open %q: %w", path, err)
	}
	defer f.Close()

	var history SessionHistory
	var sawMeta bool
	type rawCheckpoint struct {
		messageCount int
		cumulative   provider.Usage
	}
	type rawSegment struct {
		compacted   bool
		messages    []provider.Message
		checkpoints []rawCheckpoint
	}
	current := rawSegment{}

	appendCurrent := func() {
		repaired := repairSessionMessages(current.messages)
		segment := SessionHistorySegment{
			Compacted: current.compacted,
			Messages:  repaired.messages,
		}
		for _, checkpoint := range current.checkpoints {
			segment.UsageCheckpoints = append(segment.UsageCheckpoints, SessionUsageCheckpoint{
				MessageCount: repaired.prefixCount(checkpoint.messageCount),
				Cumulative:   checkpoint.cumulative,
			})
		}
		if segment.Compacted || len(segment.Messages) > 0 || len(segment.UsageCheckpoints) > 0 {
			history.Segments = append(history.Segments, segment)
		}
		current = rawSegment{}
	}

	err = forEachStrictJSONLLineContext(ctx, f, func(line []byte, lineNo int) error {
		var head sessionLineHead
		if err := json.Unmarshal(line, &head); err != nil {
			return fmt.Errorf("line %d: invalid JSON: %w", lineNo, err)
		}
		if head.Type == "" {
			return fmt.Errorf("line %d: missing row type", lineNo)
		}
		if !sawMeta && head.Type != "meta" {
			return fmt.Errorf("line %d: first row is not meta", lineNo)
		}

		switch head.Type {
		case "meta":
			var row sessionLine
			if err := json.Unmarshal(line, &row); err != nil {
				return fmt.Errorf("line %d: invalid meta row: %w", lineNo, err)
			}
			if row.Meta == nil || row.Meta.ID == "" {
				return fmt.Errorf("line %d: meta row has no session id", lineNo)
			}
			history.Meta = *row.Meta
			sawMeta = true

		case "message":
			message, err := hydrateMessage(line)
			if err != nil {
				return fmt.Errorf("line %d: invalid message row: %w", lineNo, err)
			}
			current.messages = append(current.messages, message)

		case "compaction":
			compacted, err := hydrateCompaction(line)
			if err != nil {
				return fmt.Errorf("line %d: invalid compaction row: %w", lineNo, err)
			}
			appendCurrent()
			current = rawSegment{compacted: true, messages: compacted}
			cumulative, err := hydrateCompactionUsage(line)
			if err != nil {
				return fmt.Errorf("line %d: invalid compaction usage: %w", lineNo, err)
			}
			if cumulative != nil {
				current.checkpoints = append(current.checkpoints, rawCheckpoint{
					messageCount: len(current.messages),
					cumulative:   *cumulative,
				})
			}

		case "usage":
			var row struct {
				Cumulative *provider.Usage `json:"cumulative"`
			}
			if err := json.Unmarshal(line, &row); err != nil {
				return fmt.Errorf("line %d: invalid usage row: %w", lineNo, err)
			}
			if row.Cumulative == nil {
				return fmt.Errorf("line %d: usage row has no cumulative usage", lineNo)
			}
			current.checkpoints = append(current.checkpoints, rawCheckpoint{
				messageCount: len(current.messages),
				cumulative:   *row.Cumulative,
			})

		case "extension_state":
			// Extension state is session metadata rather than transcript history.
			// The snapshot reader restores it for resume; tree history only needs
			// provider messages and usage checkpoints.

		case "rename":
			var row struct {
				Title *string `json:"title"`
			}
			if err := json.Unmarshal(line, &row); err != nil {
				return fmt.Errorf("line %d: invalid rename row: %w", lineNo, err)
			}
			if row.Title == nil {
				return fmt.Errorf("line %d: rename row has no title", lineNo)
			}

		default:
			return fmt.Errorf("line %d: unknown row type %q", lineNo, head.Type)
		}
		return nil
	})
	if err != nil {
		return SessionHistory{}, fmt.Errorf("session history %q: %w", path, err)
	}
	if !sawMeta {
		return SessionHistory{}, fmt.Errorf("session history %q: file is empty", path)
	}
	appendCurrent()
	return history, nil
}

// forEachStrictJSONLLine is the complete-read counterpart to
// forEachJSONLLine. It rejects malformed and blank rows and reports the
// line number so callers cannot accidentally proceed with a partial file.
func forEachStrictJSONLLine(r io.Reader, fn func([]byte, int) error) error {
	return forEachStrictJSONLLineContext(context.Background(), r, fn)
}

func forEachStrictJSONLLineContext(ctx context.Context, r io.Reader, fn func([]byte, int) error) error {
	br := bufio.NewReader(r)
	lineNo := 0
	for {
		if err := contextErr(ctx); err != nil {
			return err
		}
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			line = bytes.TrimRight(line, "\r\n")
			if len(bytes.TrimSpace(line)) == 0 {
				return fmt.Errorf("line %d: blank row", lineNo)
			}
			if ferr := fn(line, lineNo); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// repairedSessionMessage keeps the raw message index that produced an
// effective message. Synthetic tool results use the assistant's raw index;
// this lets usage checkpoints continue to refer to the same effective
// message prefix after repair removes or inserts rows.
type repairedSessionMessage struct {
	message  provider.Message
	rawIndex int
}

type repairedSessionMessages struct {
	messages []provider.Message
	counts   []int
}

func (r repairedSessionMessages) prefixCount(rawCount int) int {
	if rawCount < 0 {
		return 0
	}
	if rawCount >= len(r.counts) {
		return r.counts[len(r.counts)-1]
	}
	return r.counts[rawCount]
}

// repairSessionMessages repairs tool-call pairs and removes orphaned result
// blocks while retaining a mapping from raw effective prefixes to repaired
// prefixes. A tool result is valid only in the immediately following tool
// message for the assistant tool calls; keeping an unrelated result there
// would still make a resumed or branched transcript provider-invalid.
func repairSessionMessages(msgs []provider.Message) repairedSessionMessages {
	if len(msgs) == 0 {
		return repairedSessionMessages{counts: []int{0}}
	}

	repaired := make([]repairedSessionMessage, 0, len(msgs)+2)
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role == provider.RoleAssistant && messageHasToolCalls(m) {
			ids := make(map[string]bool)
			for _, c := range m.Content {
				if tc, ok := c.(provider.ToolCallBlock); ok {
					ids[tc.ID] = true
				}
			}
			repaired = append(repaired, repairedSessionMessage{message: m, rawIndex: i})

			if i+1 < len(msgs) && msgs[i+1].Role == provider.RoleTool {
				tool := msgs[i+1]
				seen := make(map[string]bool)
				filtered := make([]provider.Content, 0, len(tool.Content)+len(ids))
				for _, c := range tool.Content {
					tr, ok := c.(provider.ToolResultBlock)
					if !ok {
						filtered = append(filtered, c)
						continue
					}
					if !ids[tr.CallID] || seen[tr.CallID] {
						continue
					}
					seen[tr.CallID] = true
					filtered = append(filtered, tr)
				}
				for _, c := range m.Content {
					if tc, ok := c.(provider.ToolCallBlock); ok && !seen[tc.ID] {
						filtered = append(filtered, provider.ToolResultBlock{
							CallID:  tc.ID,
							Content: []provider.Content{provider.TextBlock{Text: "tool call was aborted; no result recorded."}},
							IsError: true,
						})
					}
				}
				if len(filtered) > 0 {
					tool.Content = filtered
					repaired = append(repaired, repairedSessionMessage{message: tool, rawIndex: i + 1})
				}
				i++
				continue
			}

			stubs := make([]provider.Content, 0, len(ids))
			for _, c := range m.Content {
				if tc, ok := c.(provider.ToolCallBlock); ok {
					stubs = append(stubs, provider.ToolResultBlock{
						CallID:  tc.ID,
						Content: []provider.Content{provider.TextBlock{Text: "tool call was aborted; no result recorded."}},
						IsError: true,
					})
				}
			}
			repaired = append(repaired, repairedSessionMessage{message: provider.Message{
				Role:    provider.RoleTool,
				Content: stubs,
				Time:    m.Time,
			}, rawIndex: i})
			continue
		}

		if m.Role == provider.RoleTool {
			// Deferred-tool activation rows intentionally carry a result
			// without a preceding assistant call. Preserve those rows and
			// their AddedToolNames marker; they are consumed as local
			// transcript state rather than sent as a provider tool pair.
			if len(m.AddedToolNames) > 0 {
				repaired = append(repaired, repairedSessionMessage{message: m, rawIndex: i})
				continue
			}
			// This tool row was not immediately paired with an assistant
			// tool-call row. Drop its result blocks; a bare tool row is an
			// orphan and is not safe to send to a provider.
			filtered := make([]provider.Content, 0, len(m.Content))
			for _, c := range m.Content {
				if _, ok := c.(provider.ToolResultBlock); !ok {
					filtered = append(filtered, c)
				}
			}
			if len(filtered) > 0 {
				m.Content = filtered
				repaired = append(repaired, repairedSessionMessage{message: m, rawIndex: i})
			}
			continue
		}
		repaired = append(repaired, repairedSessionMessage{message: m, rawIndex: i})
	}

	out := repairedSessionMessages{
		messages: make([]provider.Message, 0, len(repaired)),
		counts:   make([]int, len(msgs)+1),
	}
	for _, item := range repaired {
		out.messages = append(out.messages, item.message)
	}
	// rawIndex values are produced in source order. Accumulate the number of
	// repaired messages before each raw prefix in one forward pass instead of
	// revisiting every remaining prefix for every repaired message.
	repairedIndex := 0
	for rawCount := range out.counts {
		for repairedIndex < len(repaired) && repaired[repairedIndex].rawIndex < rawCount {
			repairedIndex++
		}
		out.counts[rawCount] = repairedIndex
	}
	return out
}

func messageHasToolCalls(msg provider.Message) bool {
	for _, content := range msg.Content {
		if _, ok := content.(provider.ToolCallBlock); ok {
			return true
		}
	}
	return false
}

// OpenSession opens an existing session for appending.
func OpenSession(path string) (*Session, []provider.Message, error) {
	snapshot, err := ReadSessionSnapshot(path)
	if err != nil {
		return nil, nil, err
	}
	out, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	s := &Session{
		ID:             snapshot.Meta.ID,
		Path:           path,
		Meta:           snapshot.Meta,
		Title:          NormalizeSessionTitle(snapshot.Title),
		ExtensionState: snapshot.ExtensionState,
		writer:         out,
		buf:            bufio.NewWriter(out),
	}
	return s, snapshot.Messages, nil
}

// repairToolUseResultPairs walks a restored transcript and
// synthesises stub tool_result blocks for any assistant
// tool_use blocks that aren't paired with a matching result in
// the next message. Anthropic (and OpenAI via the responses API)
// reject any request whose transcript leaves a tool_use without
// its matching tool_result immediately after, with errors like:
//
//	messages.8: `tool_use` ids were found without `tool_result`
//	blocks immediately after
//
// Corruption gets into the transcript two ways we know of:
//
//   - Older zut builds that persisted the assistant tool_use row
//     before the tool_result row, then crashed between the two.
//   - Abort paths in older builds that didn't drop the mid-turn
//     assistant message cleanly.
//
// Rather than change runtime semantics (which would risk hiding a
// real bug), we scrub on load: any unmatched tool_use gets a stub
// tool_result injected as a RoleTool message so the next
// outbound request passes the provider's validity check. The stub
// reads "tool call was aborted; no result recorded." so the
// model can see what happened and decide whether to retry.
//
// Runs once per OpenSession call. No cost on the hot path.
func repairToolUseResultPairs(msgs []provider.Message) []provider.Message {
	return repairSessionMessages(msgs).messages
}

// LatestSession returns the most recent session file for cwd, or "".
func LatestSession(root, cwd string) string {
	paths := ListSessions(root, cwd)
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

// SessionSummary describes one on-disk session at a glance for UI pickers.
type SessionSummary struct {
	Path          string
	Started       time.Time
	Model         string
	Provider      string
	MessageCount  int
	FirstUserText string
	TotalCost     float64
	Title         string
	BranchDepth   int
	// HideFromSessions marks internal branches omitted from the flat picker.
	HideFromSessions bool
}

// RenameSession appends a sanitized rename line to the session file. This is
// safe even for the currently active session because it opens the file
// independently and appends (doesn't rewrite).
func RenameSession(path, title string) error {
	title = NormalizeSessionTitle(title)
	if title == "" {
		return errors.New("session title is empty")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, _ := json.Marshal(map[string]string{"type": "rename", "title": title})
	line = append(line, '\n')
	_, err = f.Write(line)
	return err
}

// DeleteSession removes a session file. It refuses an empty path so
// callers do not accidentally delete their cwd or another implicit target.
func DeleteSession(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("session path is empty")
	}
	return os.Remove(path)
}

// DescribeSession returns a lightweight summary for one on-disk session.
// It uses the effective snapshot so message counts, metadata, and usage agree
// with resume and branching behavior. The summary retains HideFromSessions so
// callers that load entries independently can apply the same picker filter as
// DescribeSessions.
func DescribeSession(path string) SessionSummary {
	return DescribeSessionContext(context.Background(), path)
}

// DescribeSessionContext is the cancellation-aware form of DescribeSession.
// Cancellation is checked between JSONL records so a closed picker does not
// keep parsing a large transcript unnecessarily.
func DescribeSessionContext(ctx context.Context, path string) SessionSummary {
	return describeSessionContext(ctx, path)
}

// DescribeSessions returns lightweight summaries for every session in
// cwd, newest first. It uses the effective snapshot so message counts,
// metadata, and usage agree with resume and branching behavior.
func DescribeSessions(root, cwd string) []SessionSummary {
	paths := ListSessions(root, cwd)
	summaries := make([]SessionSummary, 0, len(paths))
	metas := make(map[string]SessionMeta, len(paths))
	idToPath := make(map[string]string, len(paths))
	for _, p := range paths {
		if meta, err := readSessionMeta(p); err == nil && meta.ID != "" {
			metas[p] = meta
			idToPath[meta.ID] = p
		}
	}
	for _, p := range paths {
		if meta := metas[p]; meta.HideFromSessions {
			continue
		}
		summaries = append(summaries, describeSession(p))
	}
	for idx := range summaries {
		summaries[idx].BranchDepth = sessionBranchDepth(summaries[idx].Path, metas, idToPath)
	}
	return summaries
}

func sessionBranchDepth(path string, metas map[string]SessionMeta, idToPath map[string]string) int {
	depth := 0
	seen := map[string]bool{}
	for {
		meta, ok := metas[path]
		if !ok || meta.Parent == "" || seen[meta.ID] {
			return depth
		}
		seen[meta.ID] = true
		parentPath := idToPath[meta.Parent]
		if parentPath == "" {
			return depth
		}
		depth++
		path = parentPath
	}
}

func describeSession(path string) SessionSummary {
	return describeSessionContext(context.Background(), path)
}

func describeSessionContext(ctx context.Context, path string) SessionSummary {
	s := SessionSummary{Path: path}
	snapshot, err := readSessionSnapshot(ctx, path)
	if err != nil {
		return s
	}
	s.Started = snapshot.Meta.Started
	s.Model = snapshot.Meta.Model
	s.Provider = snapshot.Meta.Provider
	s.HideFromSessions = snapshot.Meta.HideFromSessions
	s.Title = NormalizeSessionTitle(snapshot.Title)
	s.MessageCount = len(snapshot.Messages)
	s.FirstUserText = firstUserTextFromMessages(snapshot.Messages)
	lastUser := lastUserText(snapshot.Messages)
	isBranch := snapshot.Meta.Parent != ""
	forkPoint := snapshot.Meta.ForkPoint
	branchPrompt := firstUserTextFromMessagesAfter(snapshot.Messages, forkPoint)
	if len(snapshot.UsageCheckpoints) > 0 {
		s.TotalCost = snapshot.UsageCheckpoints[len(snapshot.UsageCheckpoints)-1].Cumulative.CostUSD
	}

	// Rename rows are append-only and are intentionally kept out of the
	// effective transcript. Scan only their placement so generated branch
	// titles can still be replaced by the first post-fork prompt while an
	// explicit rename after divergence keeps priority.
	messageIdx := 0
	renameMessageIdx := -1
	renameGenerated := false
	_ = func() error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return forEachJSONLLineContext(ctx, f, func(line []byte) error {
			var head sessionLineHead
			if err := json.Unmarshal(line, &head); err != nil {
				return nil
			}
			switch head.Type {
			case "message":
				messageIdx++
			case "compaction":
				if compacted, err := hydrateCompaction(line); err == nil {
					messageIdx = len(compacted)
				}
			case "rename":
				var row struct {
					Title     string `json:"title"`
					Generated bool   `json:"generated"`
				}
				if err := json.Unmarshal(line, &row); err == nil && row.Title != "" {
					s.Title = NormalizeSessionTitle(row.Title)
					renameMessageIdx = messageIdx
					renameGenerated = row.Generated
				}
			}
			return nil
		})
	}()
	if isBranch {
		// A rename written after the branch has diverged is user intent
		// and must win. Older generated branch titles were written at
		// fork creation time, before any post-fork prompt, so those are
		// allowed to be replaced by the branch prompt fallback. Current
		// generated titles are marked so a fast hidden request that writes
		// before the first post-fork message still wins.
		explicitRename := !renameGenerated && renameMessageIdx > forkPoint
		if !renameGenerated && renameMessageIdx == forkPoint && s.Title != lastUser {
			explicitRename = true
		}
		if !renameGenerated && !explicitRename && branchPrompt != "" {
			s.Title = branchPrompt
		} else if s.Title == "" && lastUser != "" {
			s.Title = lastUser
		}
	}
	return s
}

func firstUserTextFromMessages(msgs []provider.Message) string {
	return firstUserTextFromMessagesAfter(msgs, 0)
}

func firstUserTextFromMessagesAfter(msgs []provider.Message, start int) string {
	if start < 0 {
		start = 0
	}
	for idx, msg := range msgs {
		if idx < start || msg.Role != provider.RoleUser {
			continue
		}
		if text := firstTextFromMessage(msg); text != "" {
			return text
		}
	}
	return ""
}

func firstTextFromMessage(msg provider.Message) string {
	for _, c := range msg.Content {
		if tb, ok := c.(provider.TextBlock); ok && tb.Text != "" {
			return tb.Text
		}
	}
	return ""
}

// PruneEmptySessions deletes session files in cwd's session directory
// that contain only a meta line (no messages were ever appended).
// Cleans up the backlog of empty stubs created by old zut versions
// that wrote a meta line at NewSession time and never followed up.
// Errors are swallowed; the caller treats this as best-effort.
func PruneEmptySessions(root, cwd string) {
	dir := SessionsDir(root, cwd)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if sessionHasNoMessages(p) {
			_ = os.Remove(p)
		}
	}
}

// sessionHasNoMessages returns true when the file at path contains
// no lines of type "message". Meta-only / usage-only files count as
// empty. Used by PruneEmptySessions and the Describe path.
func sessionHasNoMessages(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	hasMessage := false
	_ = forEachJSONLLine(f, func(line []byte) error {
		var head sessionLineHead
		if err := json.Unmarshal(line, &head); err != nil {
			return nil
		}
		if head.Type == "message" {
			hasMessage = true
			return io.EOF
		}
		return nil
	})
	return !hasMessage
}

// ListSessions returns session file paths for cwd, most-recently-
// modified first. Sorting on filesystem ModTime instead of the
// timestamp embedded in the filename means a long-running session
// the user actually returned to recently floats to the top of
// /sessions, /continue, and the resume picker, even when it was
// originally created days earlier than newer but idle sessions.
// Files with identical ModTime fall back to filename desc so the
// order stays stable across calls.
func ListSessions(root, cwd string) []string {
	return ListSessionsContext(context.Background(), root, cwd)
}

// ListSessionsContext is the cancellation-aware form of ListSessions. It
// checks cancellation while walking directory entries so a closed picker does
// not continue preparing a large result set.
func ListSessionsContext(ctx context.Context, root, cwd string) []string {
	if err := contextErr(ctx); err != nil {
		return nil
	}
	dir := SessionsDir(root, cwd)
	dirFile, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer dirFile.Close()
	type rec struct {
		path string
		mod  time.Time
	}
	var files []rec
	for {
		if err := contextErr(ctx); err != nil {
			return nil
		}
		entries, readErr := dirFile.ReadDir(128)
		if readErr != nil && readErr != io.EOF {
			return nil
		}
		for _, e := range entries {
			if err := contextErr(ctx); err != nil {
				return nil
			}
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			p := filepath.Join(dir, e.Name())
			info, err := e.Info()
			if err != nil {
				continue
			}
			files = append(files, rec{path: p, mod: info.ModTime()})
		}
		if readErr == io.EOF {
			break
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].mod.Equal(files[j].mod) {
			return files[i].mod.After(files[j].mod)
		}
		return files[i].path > files[j].path
	})
	out := make([]string, 0, len(files))
	for _, r := range files {
		if err := contextErr(ctx); err != nil {
			return nil
		}
		out = append(out, r.path)
	}
	return out
}

// AppendMessage writes a message to the session.
func (s *Session) AppendMessage(m provider.Message) error {
	if s == nil {
		return nil
	}
	if len(m.Content) == 0 {
		return errors.New("message has no content")
	}
	if err := s.writeLine(sessionLine{Type: "message", Message: &m}); err != nil {
		return err
	}
	s.messagesAppended++
	return nil
}

// Sync flushes the session file's bytes through the filesystem. Interactive
// hosts use this at acknowledgement boundaries where a crash must not make a
// prompt look accepted before its user message is recoverable.
func (s *Session) Sync() error {
	if s == nil || s.writer == nil {
		return nil
	}
	if err := s.buf.Flush(); err != nil {
		return err
	}
	return s.writer.Sync()
}

// AppendCompaction writes a checkpoint that replaces all earlier
// transcript rows when the session is resumed. The old rows remain in
// the JSONL file for audit/export, while loaders use the latest
// compaction row as the effective transcript.
func (s *Session) AppendCompaction(messages []provider.Message) error {
	if s == nil {
		return nil
	}
	compactionMessages := messages
	if err := s.writeLine(sessionLine{Type: "compaction", Messages: &compactionMessages}); err != nil {
		return err
	}
	// The compaction row itself is meaningful even when it replaces the
	// transcript with an empty or nil slice. Count the append operation so a
	// fresh session containing an empty checkpoint is not deleted on Close.
	s.messagesAppended++
	return nil
}

// AppendCompactionWithUsage atomically records a replacement transcript and
// its cumulative usage in one JSONL row. Workers use this after compaction so
// a persistence failure cannot leave a resumable session with only part of the
// continued turn or without its matching usage checkpoint.
func (s *Session) AppendCompactionWithUsage(messages []provider.Message, cumulative provider.Usage) error {
	if s == nil {
		return nil
	}
	compactionMessages := messages
	if err := s.writeUsageCheckpoint(sessionLine{
		Type:       "compaction",
		Messages:   &compactionMessages,
		Usage:      &cumulative,
		Cumulative: &cumulative,
	}); err != nil {
		return err
	}
	s.messagesAppended++
	return nil
}

// UpdateModel records a provider/model switch in the session file.
// The reader keeps the most recent meta entry, so the session resumes
// with the updated model.
func (s *Session) UpdateModel(providerName, model string) error {
	if s == nil {
		return nil
	}
	oldProvider, oldModel := s.Meta.Provider, s.Meta.Model
	s.Meta.Provider = providerName
	s.Meta.Model = model
	if err := s.writeLine(sessionLine{Type: "meta", Meta: &s.Meta}); err != nil {
		s.Meta.Provider = oldProvider
		s.Meta.Model = oldModel
		return err
	}
	return nil
}

// UpdateCompactHandoff records opaque host-owned compaction-handoff state in
// a new metadata row. A nil or JSON null value clears the state. The core
// session layer deliberately does not interpret the payload.
func (s *Session) UpdateCompactHandoff(state json.RawMessage) error {
	if s == nil {
		return nil
	}
	if len(state) > 0 && !json.Valid(state) {
		return errors.New("compact handoff state is not valid JSON")
	}
	if strings.TrimSpace(string(state)) == "null" {
		state = nil
	}
	previous := append(json.RawMessage(nil), s.Meta.CompactHandoff...)
	s.Meta.CompactHandoff = append(json.RawMessage(nil), state...)
	if err := s.writeLine(sessionLine{Type: "meta", Meta: &s.Meta}); err != nil {
		s.Meta.CompactHandoff = previous
		return fmt.Errorf("update compact handoff: %w", err)
	}
	return nil
}

// UpdateGoal records or clears the current autonomous session goal.
const maxMissionGoalTransitions = 16

// EnsureMission creates the durable user-intent boundary when this session has
// none. It never replaces an existing mission; explicit user controls own
// replacement and cancellation semantics.
func (s *Session) EnsureMission(objective string, source MissionSource) error {
	if s == nil || strings.TrimSpace(objective) == "" || s.Meta.Mission != nil {
		return nil
	}
	previous := s.Meta.Mission
	s.Meta.Mission = &SessionMission{
		ID:        uuid.NewString(),
		Objective: strings.TrimSpace(objective),
		Status:    MissionActive,
		Source:    source,
	}
	if err := s.writeLine(sessionLine{Type: "meta", Meta: &s.Meta}); err != nil {
		s.Meta.Mission = previous
		return fmt.Errorf("ensure mission: %w", err)
	}
	return nil
}

// UpdateGoal persists one linear goal transition. It lazily migrates legacy
// goal-only sessions into a mission and rejects unbounded manager progression.
func (s *Session) UpdateGoal(goal *SessionGoal) error {
	if s == nil {
		return nil
	}
	previous := cloneSessionGoal(s.Meta.Goal)
	previousMission := cloneSessionMission(s.Meta.Mission)
	previousHistory := append([]SessionGoal(nil), s.Meta.GoalHistory...)
	if previous != nil {
		s.Meta.GoalHistory = append(s.Meta.GoalHistory, *previous)
	}
	if goal == nil {
		s.Meta.Goal = nil
		s.Meta.Mission = nil
	} else {
		copyGoal := *goal
		if err := s.assignGoalToMission(&copyGoal, previous); err != nil {
			s.Meta.Mission = previousMission
			s.Meta.GoalHistory = previousHistory
			return err
		}
		s.Meta.Goal = &copyGoal
	}
	if err := s.writeLine(sessionLine{Type: "meta", Meta: &s.Meta}); err != nil {
		s.Meta.Goal = previous
		s.Meta.Mission = previousMission
		s.Meta.GoalHistory = previousHistory
		return fmt.Errorf("update goal: %w", err)
	}
	return nil
}

// UpdateGoalRuntime persists mutable execution state without treating each
// continuation as a mission transition. The goal ID must match the current
// goal so stale workers cannot overwrite a newer user or manager goal.
func (s *Session) UpdateGoalRuntime(goal *SessionGoal) error {
	if s == nil {
		return nil
	}
	if goal == nil || s.Meta.Goal == nil || goal.ID == "" || goal.ID != s.Meta.Goal.ID {
		return errors.New("goal runtime update does not match the current goal")
	}
	previous := cloneSessionGoal(s.Meta.Goal)
	copyGoal := *s.Meta.Goal
	copyGoal.TokensUsed = goal.TokensUsed
	copyGoal.ContinuationID = goal.ContinuationID
	copyGoal.ConsecutiveNoProgressTurns = goal.ConsecutiveNoProgressTurns
	s.Meta.Goal = &copyGoal
	if err := s.writeLine(sessionLine{Type: "meta", Meta: &s.Meta}); err != nil {
		s.Meta.Goal = previous
		return fmt.Errorf("update goal runtime: %w", err)
	}
	return nil
}

func (s *Session) assignGoalToMission(goal, previous *SessionGoal) error {
	if s.Meta.Mission == nil {
		source := MissionSourceUser
		if goal.Owner == GoalOwnerManager {
			source = MissionSourceManager
		}
		s.Meta.Mission = &SessionMission{
			ID:        uuid.NewString(),
			Objective: goal.Objective,
			Status:    MissionActive,
			Source:    source,
		}
	}
	mission := s.Meta.Mission
	if goal.ID == "" {
		goal.ID = uuid.NewString()
	}
	isNewManagerGoal := goal.Status == GoalActive && goal.Owner == GoalOwnerManager && (previous == nil || goal.ID != previous.ID)
	if isNewManagerGoal {
		mission.TransitionCount++
		if mission.TransitionCount > maxMissionGoalTransitions {
			return fmt.Errorf("mission goal transition limit reached")
		}
	}
	goal.MissionID = mission.ID
	if goal.Ordinal == 0 {
		goal.Ordinal = len(s.Meta.GoalHistory) + 1
	}
	if goal.Owner == "" {
		goal.Owner = GoalOwnerUser
	}
	if goal.Status == GoalActive {
		mission.ActiveGoalID = goal.ID
		mission.Status = MissionActive
		mission.Reason = ""
	} else if mission.ActiveGoalID == goal.ID {
		mission.ActiveGoalID = ""
		switch goal.Status {
		case GoalDone:
			mission.Status = MissionCompleted
			mission.Reason = ""
		case GoalPaused:
			mission.Status = MissionPaused
			mission.Reason = ""
		case GoalBlocked, GoalBudgetLimited, GoalStalled:
			mission.Status = MissionBlocked
			mission.Reason = goal.Reason
		}
	}
	return nil
}

func normalizeSessionGoalMeta(meta *SessionMeta) {
	if meta == nil || meta.Goal == nil {
		return
	}
	goal := meta.Goal
	if goal.Owner == "" {
		goal.Owner = GoalOwnerUser
	}
	if meta.Mission == nil {
		meta.Mission = &SessionMission{
			ID:        "legacy-" + meta.ID,
			Objective: goal.Objective,
			Status:    MissionActive,
			Source:    MissionSourceUser,
		}
	}
	if goal.ID == "" {
		goal.ID = "legacy-goal-" + meta.ID
	}
	if goal.MissionID == "" {
		goal.MissionID = meta.Mission.ID
	}
	if goal.Ordinal == 0 {
		goal.Ordinal = len(meta.GoalHistory) + 1
	}
	if goal.Status == GoalActive {
		meta.Mission.ActiveGoalID = goal.ID
	}
}

func cloneSessionMission(mission *SessionMission) *SessionMission {
	if mission == nil {
		return nil
	}
	clone := *mission
	return &clone
}

// UpdateTitle records a session title without adding anything to the
// conversation transcript. The title is kept in memory as well so the live
// session can restore it when the user switches sessions in the TUI.
func (s *Session) UpdateTitle(title string) error {
	if s == nil {
		return nil
	}
	title = NormalizeSessionTitle(title)
	if title == "" {
		return nil
	}
	if err := s.writeLine(sessionLine{Type: "rename", Title: title, Generated: true}); err != nil {
		return fmt.Errorf("update session title: %w", err)
	}
	s.Title = title
	return nil
}

// RecordRetryLifecycle buffers a sanitized retry record for the next usage
// checkpoint. Invalid enum values are discarded or reduced to "unknown" so
// callers cannot use this metadata path to persist provider text.
func (s *Session) RecordRetryLifecycle(record RetryLifecycleRecord) {
	if s == nil {
		return
	}
	record, ok := sanitizeRetryLifecycleRecord(record)
	if !ok {
		return
	}
	s.retryMu.Lock()
	s.pendingRetryLifecycle = append(s.pendingRetryLifecycle, record)
	s.retryMu.Unlock()
}

// AppendUsage writes a usage row to the session and includes any retry
// lifecycle records accumulated since the previous usage checkpoint.
func (s *Session) AppendUsage(u, cum provider.Usage) error {
	if s == nil {
		return nil
	}
	return s.writeUsageCheckpoint(sessionLine{Type: "usage", Usage: &u, Cumulative: &cum})
}

// FlushRetryLifecycle writes a usage checkpoint only when retry records are
// pending. Long-running hosts use it during shutdown so a terminal failure
// without a usage event is still diagnosable.
func (s *Session) FlushRetryLifecycle(u, cum provider.Usage) error {
	if s == nil {
		return nil
	}
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	if len(s.pendingRetryLifecycle) == 0 {
		return nil
	}
	return s.writeUsageCheckpointLocked(sessionLine{Type: "usage", Usage: &u, Cumulative: &cum})
}

func (s *Session) writeUsageCheckpoint(row sessionLine) error {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	return s.writeUsageCheckpointLocked(row)
}

func (s *Session) writeUsageCheckpointLocked(row sessionLine) error {
	if len(s.pendingRetryLifecycle) > 0 {
		row.RetryLifecycle = append([]RetryLifecycleRecord(nil), s.pendingRetryLifecycle...)
	}
	if err := s.writeLine(row); err != nil {
		return err
	}
	s.pendingRetryLifecycle = nil
	return nil
}

func sanitizeRetryLifecycleRecord(record RetryLifecycleRecord) (RetryLifecycleRecord, bool) {
	switch record.Event {
	case RetryLifecycleRequestFailed:
		record.DelayMS = 0
	case RetryLifecycleRetryScheduled:
		record.Terminal = false
		if record.DelayMS < 0 {
			record.DelayMS = 0
		}
	default:
		return RetryLifecycleRecord{}, false
	}
	switch record.Scope {
	case RetryScopeProvider, RetryScopeAgent:
	default:
		return RetryLifecycleRecord{}, false
	}
	if record.Attempt < 1 || record.MaxAttempts < record.Attempt {
		return RetryLifecycleRecord{}, false
	}
	switch record.Reason {
	case RetryReasonOverload, RetryReasonRateLimit, RetryReasonQuota,
		RetryReasonServer, RetryReasonNetwork, RetryReasonTimeout,
		RetryReasonAuth, RetryReasonContextWindow, RetryReasonClient,
		RetryReasonUnknown:
	default:
		record.Reason = RetryReasonUnknown
	}
	return record, true
}

const maxExtensionStateBytes = 256 * 1024

// AppendExtensionState records the latest opaque extension snapshot for the
// active session/branch. State is persisted in the session file but is never
// included in provider requests. A nil or JSON null state clears the key.
func (s *Session) AppendExtensionState(extension string, state json.RawMessage) error {
	if s == nil {
		return nil
	}
	extension = strings.TrimSpace(extension)
	if extension == "" {
		return fmt.Errorf("extension state: extension name is empty")
	}
	if len(state) > maxExtensionStateBytes {
		return fmt.Errorf("extension state %q exceeds %d bytes", extension, maxExtensionStateBytes)
	}
	if len(state) > 0 && !json.Valid(state) {
		return fmt.Errorf("extension state %q is not valid JSON", extension)
	}
	copyState := append(json.RawMessage(nil), state...)
	if err := s.writeLine(sessionLine{Type: "extension_state", Extension: extension, State: copyState}); err != nil {
		return fmt.Errorf("append extension state: %w", err)
	}
	if s.ExtensionState == nil {
		s.ExtensionState = map[string]json.RawMessage{}
	}
	if len(copyState) == 0 || strings.TrimSpace(string(copyState)) == "null" {
		delete(s.ExtensionState, extension)
	} else {
		s.ExtensionState[extension] = copyState
	}
	return nil
}

// Flush writes buffered session data to the append handle.
func (s *Session) Flush() error {
	if s == nil {
		return nil
	}
	return s.buf.Flush()
}

// Close flushes and closes the session file. If the session was
// freshly created in this process and never had any messages
// appended (the user opened zut, looked around, and exited without
// prompting), the file is deleted on close so the sessions list
// doesn't fill up with empty meta-only stubs.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	flushErr := s.Flush()
	closeErr := s.writer.Close()
	if s.freshFile && s.messagesAppended == 0 && len(s.ExtensionState) == 0 && len(s.Meta.CompactHandoff) == 0 && s.Meta.Mission == nil && s.Meta.Goal == nil && len(s.Meta.GoalHistory) == 0 {
		// Best-effort cleanup. We deliberately don't propagate the
		// remove error: if it fails (file already gone, perms changed)
		// the worst case is one stale empty file in the listing.
		_ = os.Remove(s.Path)
	}
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

func (s *Session) writeLine(row sessionLine) error {
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if _, err := s.buf.Write(b); err != nil {
		return err
	}
	if err := s.buf.WriteByte('\n'); err != nil {
		return err
	}
	return s.buf.Flush()
}

// ---- content (de)serialization ----
//
// provider.Content is an interface; encoding/json drops type information.
// We persist messages by reading the raw "message" object back and
// rebuilding Content from discriminated fields.

func hydrateCompactionUsage(lineBytes []byte) (*provider.Usage, error) {
	var row struct {
		Cumulative *provider.Usage `json:"cumulative"`
	}
	if err := json.Unmarshal(lineBytes, &row); err != nil {
		return nil, err
	}
	return row.Cumulative, nil
}

func hydrateCompaction(lineBytes []byte) ([]provider.Message, error) {
	var row struct {
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(lineBytes, &row); err != nil {
		return nil, err
	}
	if len(row.Messages) == 0 || bytes.Equal(bytes.TrimSpace(row.Messages), []byte("null")) {
		// Older writers omitted the field for an empty compaction and some
		// wrote it as null. Both represent a valid empty replacement.
		return []provider.Message{}, nil
	}
	var rawMessages []json.RawMessage
	if err := json.Unmarshal(row.Messages, &rawMessages); err != nil {
		return nil, fmt.Errorf("invalid messages: %w", err)
	}
	messages := make([]provider.Message, 0, len(rawMessages))
	for idx, raw := range rawMessages {
		msg, err := hydrateMessageObject(raw)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", idx, err)
		}
		messages = append(messages, msg)
	}
	return messages, nil
}

func hydrateMessage(lineBytes []byte) (provider.Message, error) {
	var row struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(lineBytes, &row); err != nil {
		return provider.Message{}, err
	}
	if len(row.Message) == 0 || bytes.Equal(bytes.TrimSpace(row.Message), []byte("null")) {
		return provider.Message{}, fmt.Errorf("message row has no message")
	}
	return hydrateMessageObject(row.Message)
}

func hydrateMessageObject(rawMessage []byte) (provider.Message, error) {
	var row struct {
		Role           provider.Role     `json:"role"`
		Content        []json.RawMessage `json:"content"`
		Time           time.Time         `json:"time"`
		Meta           map[string]string `json:"meta,omitempty"`
		AddedToolNames []string          `json:"added_tool_names,omitempty"`
	}
	if err := json.Unmarshal(rawMessage, &row); err != nil {
		return provider.Message{}, err
	}
	if row.Role != provider.RoleUser && row.Role != provider.RoleAssistant && row.Role != provider.RoleTool {
		return provider.Message{}, fmt.Errorf("message has invalid role %q", row.Role)
	}
	if len(row.Content) == 0 {
		return provider.Message{}, fmt.Errorf("message has no content")
	}
	msg := provider.Message{Role: row.Role, Time: row.Time, Meta: row.Meta, AddedToolNames: row.AddedToolNames}
	for idx, raw := range row.Content {
		if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return provider.Message{}, fmt.Errorf("content block %d is empty", idx)
		}
		var head struct {
			Text             string `json:"text"`
			MimeType         string `json:"mime_type"`
			Data             []byte `json:"data"`
			ID               string `json:"id"`
			Name             string `json:"name"`
			CallID           string `json:"call_id"`
			ReasoningID      string `json:"reasoning_id"`
			Summary          string `json:"summary"`
			Encrypted        string `json:"encrypted_content"`
			ThoughtSignature string `json:"thought_signature"`
			// ToolCallBlock also has Arguments, ToolResultBlock has Content + IsError
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return provider.Message{}, fmt.Errorf("content block %d: %w", idx, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			if err == nil {
				err = fmt.Errorf("content block is not an object")
			}
			return provider.Message{}, fmt.Errorf("content block %d: %w", idx, err)
		}
		// Discriminate by presence of fields.
		switch {
		case head.ReasoningID != "" || head.Summary != "" || head.Encrypted != "":
			msg.Content = append(msg.Content, provider.ReasoningBlock{
				ID:        head.ReasoningID,
				Summary:   head.Summary,
				Encrypted: head.Encrypted,
			})
		case head.Name != "" || head.ID != "" || fields["arguments"] != nil:
			if head.Name == "" || head.ID == "" {
				return provider.Message{}, fmt.Errorf("content block %d: incomplete tool call", idx)
			}
			var tc struct {
				ID               string          `json:"id"`
				Name             string          `json:"name"`
				Arguments        json.RawMessage `json:"arguments"`
				ThoughtSignature string          `json:"thought_signature"`
			}
			_ = json.Unmarshal(raw, &tc)
			msg.Content = append(msg.Content, provider.ToolCallBlock{
				ID:               tc.ID,
				Name:             tc.Name,
				Arguments:        tc.Arguments,
				ThoughtSignature: tc.ThoughtSignature,
			})
		case head.CallID != "":
			var tr struct {
				CallID  string               `json:"call_id"`
				Content []json.RawMessage    `json:"content"`
				IsError bool                 `json:"is_error"`
				Timing  *provider.ToolTiming `json:"timing,omitempty"`
			}
			if err := json.Unmarshal(raw, &tr); err != nil || tr.Content == nil {
				if err == nil {
					err = fmt.Errorf("tool result has no content")
				}
				return provider.Message{}, fmt.Errorf("content block %d: %w", idx, err)
			}
			block := provider.ToolResultBlock{CallID: tr.CallID, IsError: tr.IsError, Timing: tr.Timing}
			for innerIdx, c := range tr.Content {
				var inner struct {
					Text     string `json:"text"`
					MimeType string `json:"mime_type"`
					Data     []byte `json:"data"`
				}
				if err := json.Unmarshal(c, &inner); err != nil {
					return provider.Message{}, fmt.Errorf("content block %d result %d: %w", idx, innerIdx, err)
				}
				var innerFields map[string]json.RawMessage
				if err := json.Unmarshal(c, &innerFields); err != nil || innerFields == nil {
					if err == nil {
						err = fmt.Errorf("result content is not an object")
					}
					return provider.Message{}, fmt.Errorf("content block %d result %d: %w", idx, innerIdx, err)
				}
				if inner.MimeType != "" {
					block.Content = append(block.Content, provider.ImageBlock{MimeType: inner.MimeType, Data: inner.Data})
				} else if _, ok := innerFields["text"]; ok {
					block.Content = append(block.Content, provider.TextBlock{Text: inner.Text})
				} else {
					return provider.Message{}, fmt.Errorf("content block %d result %d: unknown content", idx, innerIdx)
				}
			}
			if len(block.Content) == 0 {
				return provider.Message{}, fmt.Errorf("content block %d tool result has no content", idx)
			}
			msg.Content = append(msg.Content, block)
		case head.MimeType != "":
			msg.Content = append(msg.Content, provider.ImageBlock{
				MimeType:         head.MimeType,
				Data:             head.Data,
				ThoughtSignature: head.ThoughtSignature,
			})
		default:
			if _, ok := fields["text"]; !ok {
				return provider.Message{}, fmt.Errorf("content block %d: unknown content", idx)
			}
			msg.Content = append(msg.Content, provider.TextBlock{
				Text:             head.Text,
				ThoughtSignature: head.ThoughtSignature,
			})
		}
	}
	return msg, nil
}

// DecodeMessageJSON restores a provider-neutral message whose Content blocks
// were encoded through json.RawMessage. It is shared by durable session-like
// stores; callers must treat malformed content as persistence corruption.
func DecodeMessageJSON(rawMessage []byte) (provider.Message, error) {
	return hydrateMessageObject(rawMessage)
}
