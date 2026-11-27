package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bnema/zut/packages/agent/subagents"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

// SubagentStatusTool reports live state for background sub-agents without
// waiting for a child to finish. An omitted agent_id lists resident children
// in the current session lineage or spawned by this manager; an agent_id
// queries one child in that scope.
// By default the result contains metadata only. Explicit include_result reads
// the bounded durable outcome/handoff for one child without model execution.
type SubagentStatusTool struct {
	ResidentManager *subagents.ResidentManager
	Enabled         func() bool
	SessionScope    func() []string
}

type subagentStatusArgs struct {
	AgentID       string `json:"agent_id,omitempty"`
	IncludeResult bool   `json:"include_result,omitempty"`
	Watch         int    `json:"watch,omitempty"`
}

const (
	subagentStatusListLimit = 20
	subagentWatchMaxSeconds = 60
	subagentWatchInterval   = 250 * time.Millisecond
	subagentWatchMaxEntries = 40
	subagentActivityText    = 300
	subagentActivityArgs    = 200
)

type subagentStatusResponse struct {
	Agent     *subagentStatusEntry      `json:"agent,omitempty"`
	Agents    []subagentStatusEntry     `json:"agents"`
	Total     *int                      `json:"total,omitempty"`
	Truncated bool                      `json:"truncated,omitempty"`
	Activity  *subagentActivity         `json:"activity,omitempty"`
	Watch     *subagentWatchReport      `json:"watch,omitempty"`
	Result    *subagents.ResidentResult `json:"result,omitempty"`
}

// subagentActivity is a bounded view of the child's unfinished turn: what it
// is doing now, the tail of visible text it is writing, and active tools.
// Hidden reasoning is never exposed.
type subagentActivity struct {
	Phase string                 `json:"phase"`
	Text  string                 `json:"text,omitempty"`
	Tools []subagentActivityTool `json:"tools,omitempty"`
}

type subagentActivityTool struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Args  string `json:"args,omitempty"`
}

type subagentWatchReport struct {
	Seconds   float64                 `json:"observed_seconds"`
	EndState  subagents.ResidentState `json:"end_state"`
	Timeline  []subagentWatchEntry    `json:"timeline"`
	Truncated bool                    `json:"truncated,omitempty"`
}

type subagentWatchEntry struct {
	At       string                  `json:"at"`
	State    subagents.ResidentState `json:"state"`
	Activity *subagentActivity       `json:"activity,omitempty"`
}

type subagentStatusEntry struct {
	ID             string                  `json:"agent_id"`
	State          subagents.ResidentState `json:"state"`
	Profile        string                  `json:"profile,omitempty"`
	Provider       string                  `json:"provider"`
	Model          string                  `json:"model"`
	Workspace      subagents.WorkspaceMode `json:"workspace_mode,omitempty"`
	Required       bool                    `json:"required,omitempty"`
	OwnedElsewhere bool                    `json:"owned_elsewhere,omitempty"`
	// PendingFollowUps counts accepted follow-ups the child has not read yet.
	PendingFollowUps int `json:"pending_followups,omitempty"`
}

// Name returns the shared facade name: this type is an internal
// implementation and is never registered on its own.
func (t *SubagentStatusTool) Name() string { return SubagentToolName }

func (t *SubagentStatusTool) Execute(ctx context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return core.ToolResult{}, ctx.Err()
		default:
		}
	}
	prefix := t.Name()
	if t.ResidentManager == nil {
		return protocolToolError(prefix + ": subagent runtime not available in this mode")
	}
	if t.Enabled == nil || !t.Enabled() {
		return protocolToolError(prefix + ": subagent status is unavailable in this mode")
	}

	var args subagentStatusArgs
	if err := decodeSubagentArgs(raw, &args); err != nil {
		return core.ToolResult{}, err
	}
	id := strings.TrimSpace(args.AgentID)
	if id == "" {
		if args.IncludeResult {
			return protocolToolError(prefix + ": include_result requires agent_id")
		}
		if args.Watch != 0 {
			return protocolToolError(prefix + ": watch requires agent_id")
		}
		return renderSubagentStatus(listResidentStatus(t.ResidentManager.ScopedSnapshots(residentScope(t.SessionScope))))
	}
	snapshot, ok := t.ResidentManager.ResolveScopedSnapshot(residentScope(t.SessionScope), id)
	if !ok {
		return protocolToolError(fmt.Sprintf("%s: no such agent %q", prefix, id))
	}
	if args.Watch < 0 || args.Watch > subagentWatchMaxSeconds {
		return protocolToolError(fmt.Sprintf("%s: watch must be between 1 and %d seconds", prefix, subagentWatchMaxSeconds))
	}
	if args.Watch > 0 && snapshot.OwnedElsewhere {
		return protocolToolError(prefix + ": watch is unavailable: child is owned by another zut process")
	}
	entry := publicResidentStatus(snapshot)
	response := subagentStatusResponse{Agent: &entry}
	if args.Watch > 0 {
		report, err := t.watch(ctx, snapshot.ID, time.Duration(args.Watch)*time.Second)
		if err != nil {
			return core.ToolResult{}, err
		}
		if report.EndState == "" {
			report.EndState = entry.State
		}
		response.Watch = &report
		entry.State = report.EndState
	}
	if live, ok := t.ResidentManager.Live(snapshot.ID); ok {
		response.Activity = residentActivity(entry.State, live)
	}
	if args.IncludeResult {
		result, err := t.ResidentManager.Result(snapshot.ID)
		if err != nil {
			message := "saved result is unavailable"
			switch {
			case snapshot.OwnedElsewhere:
				message += ": child is owned by another zut process"
			case os.IsNotExist(err):
				message += ": no saved result exists yet"
			case os.IsPermission(err):
				message += ": permission denied"
			default:
				message += ": could not read or decode the saved result"
			}
			return protocolToolError(prefix + ": " + message)
		}
		response.Result = &result
	}
	return renderSubagentStatus(response)
}

// watch samples the child's live projection until the duration elapses, the
// child leaves the running/queued states, or ctx is canceled. It records an
// entry only when the observable activity changes.
func (t *SubagentStatusTool) watch(ctx context.Context, id string, duration time.Duration) (subagentWatchReport, error) {
	start := time.Now()
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	ticker := time.NewTicker(subagentWatchInterval)
	defer ticker.Stop()
	report := subagentWatchReport{Timeline: []subagentWatchEntry{}}
	last := ""
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		state, ok := t.ResidentManager.State(id)
		if !ok {
			break
		}
		report.EndState = state
		var activity *subagentActivity
		if live, ok := t.ResidentManager.Live(id); ok {
			activity = residentActivity(state, live)
		}
		entry := subagentWatchEntry{State: state, Activity: activity}
		if key := watchChangeKey(entry); key != last {
			last = key
			if len(report.Timeline) < subagentWatchMaxEntries {
				entry.At = fmt.Sprintf("+%.1fs", time.Since(start).Seconds())
				report.Timeline = append(report.Timeline, entry)
			} else {
				report.Truncated = true
			}
		}
		if state != subagents.ResidentRunning && state != subagents.ResidentQueued {
			break
		}
		select {
		case <-ctx.Done():
			return subagentWatchReport{}, ctx.Err()
		case <-deadline.C:
			report.Seconds = roundSeconds(time.Since(start))
			return report, nil
		case <-ticker.C:
		}
	}
	report.Seconds = roundSeconds(time.Since(start))
	return report, nil
}

// watchChangeKey ignores streamed text growth so the timeline records phase
// and tool transitions rather than every delta.
func watchChangeKey(entry subagentWatchEntry) string {
	var key strings.Builder
	key.WriteString(string(entry.State))
	if entry.Activity != nil {
		key.WriteString("|" + entry.Activity.Phase)
		for _, tool := range entry.Activity.Tools {
			key.WriteString("|" + tool.Name + ":" + tool.State)
		}
	}
	return key.String()
}

func roundSeconds(d time.Duration) float64 {
	return float64(d.Round(100*time.Millisecond)) / float64(time.Second)
}

// residentActivity summarizes a live snapshot. It returns nil when the child
// is not running a turn.
func residentActivity(state subagents.ResidentState, live subagents.ResidentLiveSnapshot) *subagentActivity {
	if state != subagents.ResidentRunning {
		return nil
	}
	activity := &subagentActivity{Text: tailRunes(live.AssistantText, subagentActivityText)}
	for _, tool := range live.Tools {
		activity.Tools = append(activity.Tools, subagentActivityTool{Name: tool.Name, State: string(tool.State), Args: headRunes(string(tool.Args), subagentActivityArgs)})
	}
	switch {
	case len(activity.Tools) > 0:
		activity.Phase = "tools"
	case live.WaitingForModel:
		activity.Phase = "waiting_for_model"
	case activity.Text != "":
		activity.Phase = "writing"
	default:
		activity.Phase = "working"
	}
	return activity
}

func headRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit]) + "…"
}

func tailRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return "…" + string(runes[len(runes)-limit:])
}

// listResidentStatus sorts the scoped snapshots in place before projecting the page.
func listResidentStatus(snapshots []subagents.ResidentSnapshot) subagentStatusResponse {
	active := func(state subagents.ResidentState) bool {
		return state == subagents.ResidentQueued || state == subagents.ResidentRunning
	}
	slices.SortFunc(snapshots, func(a, b subagents.ResidentSnapshot) int {
		if aActive, bActive := active(a.State), active(b.State); aActive != bActive {
			if aActive {
				return -1
			}
			return 1
		}
		if order := b.UpdatedAt.Compare(a.UpdatedAt); order != 0 {
			return order
		}
		return cmp.Compare(a.ID, b.ID)
	})
	total := len(snapshots)
	page := snapshots[:min(total, subagentStatusListLimit)]
	entries := make([]subagentStatusEntry, 0, len(page))
	for _, snapshot := range page {
		entries = append(entries, publicResidentStatus(snapshot))
	}
	return subagentStatusResponse{Agents: entries, Total: &total, Truncated: total > len(page)}
}

func residentScope(scope func() []string) []string {
	if scope == nil {
		return nil
	}
	return scope()
}

func publicResidentStatus(snapshot subagents.ResidentSnapshot) subagentStatusEntry {
	return subagentStatusEntry{ID: snapshot.ID, State: snapshot.State, Profile: snapshot.Profile, Provider: snapshot.Provider, Model: snapshot.Model, Workspace: snapshot.WorkspaceMode, Required: snapshot.Required, OwnedElsewhere: snapshot.OwnedElsewhere, PendingFollowUps: snapshot.PendingFollowUps}
}

func renderSubagentStatus(response subagentStatusResponse) (core.ToolResult, error) {
	data, err := json.Marshal(response)
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("%s: encode status: %w", SubagentToolName, err)
	}
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: string(data)}},
		Details: response,
	}, nil
}
