package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bnema/zut/packages/agent/scheduler"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

const scheduleToolSchema = `{"type":"object","properties":{"action":{"type":"string","enum":["add","list","cancel"],"description":"Scheduling operation."},"cron":{"type":"string","description":"Five-field cron expression, for example '0 9 * * 1-5'. Required for add."},"message":{"type":"string","description":"Prompt to run in this session when due. Required for add."},"id":{"type":"string","description":"Task ID. Required for cancel."}},"required":["action"],"additionalProperties":false}`

// ScheduleToolName is the stable built-in scheduler tool name.
const ScheduleToolName = "schedule"

// ScheduleTool manages process-lifetime scheduled follow-up prompts. Its
// host-provided session callback prevents an agent from selecting another
// session as a task target.
type ScheduleTool struct {
	Engine    *scheduler.Engine
	SessionID func() string
	Location  func() *time.Location
}

type scheduleToolArgs struct {
	Action  string `json:"action"`
	Cron    string `json:"cron,omitempty"`
	Message string `json:"message,omitempty"`
	ID      string `json:"id,omitempty"`
}

func (t *ScheduleTool) Name() string { return ScheduleToolName }

func (t *ScheduleTool) Description() string {
	return "Create, list, or cancel in-process cron follow-ups for the current session. Scheduled work runs only while this zut process remains open."
}

func (t *ScheduleTool) Schema() json.RawMessage { return json.RawMessage(scheduleToolSchema) }

func (t *ScheduleTool) Execute(_ context.Context, raw json.RawMessage, _ func(string)) (core.ToolResult, error) {
	if t == nil || t.Engine == nil || t.SessionID == nil {
		return scheduleToolError("scheduler is unavailable"), nil
	}
	var args scheduleToolArgs
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return scheduleToolError("invalid arguments"), nil
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return scheduleToolError("invalid arguments"), nil
	}
	args.Action = strings.TrimSpace(args.Action)
	args.Cron = strings.TrimSpace(args.Cron)
	args.Message = strings.TrimSpace(args.Message)
	args.ID = strings.TrimSpace(args.ID)

	switch args.Action {
	case "add":
		if args.Cron == "" || args.Message == "" {
			return scheduleToolError("cron and message are required when adding a task"), nil
		}
		location := time.Local
		if t.Location != nil && t.Location() != nil {
			location = t.Location()
		}
		task, err := t.Engine.Add(scheduler.NewTaskInput{
			SessionID: t.SessionID(),
			Cron:      args.Cron,
			Message:   args.Message,
			Location:  location,
		})
		if err != nil {
			return scheduleToolError(err.Error()), nil
		}
		return scheduleToolResult(fmt.Sprintf("scheduled %s for %s (%s)", task.ID, task.NextRun.Format(time.RFC3339), task.Timezone))
	case "list":
		tasks := t.Engine.List()
		currentSession := t.SessionID()
		lines := make([]string, 0, len(tasks))
		for _, task := range tasks {
			if task.SessionID != currentSession {
				continue
			}
			line := fmt.Sprintf("%s  %s  next %s", task.ID, task.Cron, task.NextRun.Format(time.RFC3339))
			if task.LastError != "" {
				line += "  last error: " + task.LastError
			}
			lines = append(lines, line)
		}
		if len(lines) == 0 {
			return scheduleToolResult("no scheduled tasks for this session")
		}
		return scheduleToolResult(strings.Join(lines, "\n"))
	case "cancel":
		if args.ID == "" {
			return scheduleToolError("id is required when cancelling a task"), nil
		}
		task, ok := t.Engine.Get(args.ID)
		if !ok || task.SessionID != t.SessionID() {
			return scheduleToolError("scheduled task not found in this session"), nil
		}
		t.Engine.Cancel(args.ID)
		return scheduleToolResult("cancelled scheduled task " + args.ID)
	default:
		return scheduleToolError("action must be add, list, or cancel"), nil
	}
}

func scheduleToolResult(text string) (core.ToolResult, error) {
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: text}}}, nil
}

func scheduleToolError(text string) core.ToolResult {
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: text}}, IsError: true}
}
