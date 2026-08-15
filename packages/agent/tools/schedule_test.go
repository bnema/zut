package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/scheduler"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

func TestScheduleToolScopesTasksToCurrentSession(t *testing.T) {
	now := time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC)
	engine := scheduler.NewEngine(func() time.Time { return now })
	tool := &ScheduleTool{
		Engine:    engine,
		SessionID: func() string { return "session-a" },
		Location:  func() *time.Location { return time.UTC },
	}

	ctx := context.Background()
	result, err := tool.Execute(ctx, json.RawMessage(`{"action":"add","cron":"@hourly","message":"check build"}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("Execute(add) = %#v, %v", result, err)
	}
	if got := resultText(result); got == "" {
		t.Fatal("Execute(add) returned no confirmation")
	}

	otherSession, err := engine.Add(scheduler.NewTaskInput{SessionID: "session-b", Cron: "@hourly", Message: "hidden", Location: time.UTC})
	if err != nil {
		t.Fatalf("Engine.Add() error = %v", err)
	}
	result, err = tool.Execute(ctx, json.RawMessage(`{"action":"cancel","id":"`+otherSession.ID+`"}`), nil)
	if err != nil || !result.IsError {
		t.Fatalf("Execute(cancel other session) = %#v, %v; want scoped error", result, err)
	}
}

func resultText(result core.ToolResult) string {
	for _, content := range result.Content {
		if text, ok := content.(provider.TextBlock); ok {
			return text.Text
		}
	}
	return ""
}
