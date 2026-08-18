package core

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bnema/zut/packages/provider"
)

func TestProjectProviderMessagesAnnotatesUserTimeWithoutMutatingTranscript(t *testing.T) {
	zone := time.FixedZone("PDT", -7*60*60)
	accepted := time.Date(2026, 8, 8, 12, 34, 56, 0, zone)
	messages := []provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "hello"}},
		Time:    accepted,
	}}

	projected := projectProviderMessages(messages)
	if got := messages[0].Content[0].(provider.TextBlock).Text; got != "hello" {
		t.Fatalf("raw user text changed to %q", got)
	}
	if len(projected[0].Content) != 2 {
		t.Fatalf("projected content = %d blocks, want timestamp and original", len(projected[0].Content))
	}
	annotation := projected[0].Content[0].(provider.TextBlock).Text
	if annotation != "[message time: 2026-08-08T12:34:56-07:00]" {
		t.Fatalf("timestamp annotation = %q", annotation)
	}
	if got := projected[0].Content[1].(provider.TextBlock).Text; got != "hello" {
		t.Fatalf("projected user text = %q", got)
	}
}

func TestAgentProviderTimeContextIsStableAndLocal(t *testing.T) {
	a := NewAgent(nil, "model", "system", Registry{})
	started := time.Date(2026, 8, 8, 9, 10, 11, 0, time.UTC)
	a.SetSessionTimeContext(started, "America/Los_Angeles", "-07:00")

	first := a.providerTimeContext().systemText()
	second := a.providerTimeContext().systemText()
	if first != second {
		t.Fatalf("time context changed between requests: %q != %q", first, second)
	}
	for _, want := range []string{"session_started: 2026-08-08 02:10", "local_timezone: America/Los_Angeles (UTC-07:00)"} {
		if !strings.Contains(first, want) {
			t.Fatalf("time context %q missing %q", first, want)
		}
	}
}

func TestSessionTimeContextUsesNamedTimezoneForProjectedWinterMessages(t *testing.T) {
	a := NewAgent(nil, "model", "", Registry{})
	started := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	a.SetSessionTimeContext(started, "America/Los_Angeles", "-07:00")

	context := a.providerTimeContext()
	december := time.Date(2026, 12, 8, 12, 0, 0, 0, time.UTC).In(context.location)
	projected := projectProviderMessages([]provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "winter message"}},
		Time:    december,
	}})
	annotation := projected[0].Content[0].(provider.TextBlock).Text
	if annotation != "[message time: 2026-12-08T04:00:00-08:00]" {
		t.Fatalf("winter message annotation = %q, want Pacific Standard Time", annotation)
	}
}

func TestProjectToolTimingIsModelOnly(t *testing.T) {
	started := time.Date(2026, 8, 8, 12, 0, 0, 0, time.FixedZone("UTC-4", -4*60*60))
	timing := &provider.ToolTiming{StartedAt: started, CompletedAt: started.Add(1500 * time.Millisecond), Duration: 1500 * time.Millisecond}
	messages := []provider.Message{{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
		CallID: "call-1", Content: []provider.Content{provider.TextBlock{Text: "output"}}, Timing: timing,
	}}}}

	projected := projectProviderMessages(messages)
	original := messages[0].Content[0].(provider.ToolResultBlock)
	if len(original.Content) != 1 {
		t.Fatalf("raw result content was changed: %#v", original.Content)
	}
	result := projected[0].Content[0].(provider.ToolResultBlock)
	if len(result.Content) != 2 {
		t.Fatalf("projected result content = %d blocks, want output and timing", len(result.Content))
	}
	timingText := result.Content[1].(provider.TextBlock).Text
	if !strings.Contains(timingText, "started=2026-08-08T12:00:00-04:00") || !strings.Contains(timingText, "duration=1.5s") {
		t.Fatalf("timing projection = %q", timingText)
	}
}

func TestProjectToolResultMessagesKeepsHistoricalPrefixStable(t *testing.T) {
	messages := make([]provider.Message, 0, 4)
	for i := 0; i < 4; i++ {
		messages = append(messages, provider.Message{Role: provider.RoleTool, Content: []provider.Content{
			provider.ToolResultBlock{
				CallID:  fmt.Sprintf("call-%d", i),
				Content: []provider.Content{provider.TextBlock{Text: strings.Repeat(string(rune('a'+i)), maxToolResultTextBytes)}},
			},
		}})
	}

	before := projectToolResultMessages(messages)
	messages = append(messages, provider.Message{Role: provider.RoleTool, Content: []provider.Content{
		provider.ToolResultBlock{
			CallID:  "new-call",
			Content: []provider.Content{provider.TextBlock{Text: strings.Repeat("new", maxToolResultTextBytes)}},
		},
	}})
	after := projectToolResultMessages(messages)

	if !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatal("appending a tool result changed the provider-visible historical prefix")
	}
}

func TestProjectToolResultMessagesBoundsAndUTF8(t *testing.T) {
	msgs := make([]provider.Message, 0, 10)
	for i := 0; i < 5; i++ {
		id := "call-" + string(rune('1'+i))
		msgs = append(msgs,
			provider.Message{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.ToolCallBlock{ID: id, Name: "read"}},
			},
			provider.Message{
				Role: provider.RoleTool,
				Content: []provider.Content{provider.ToolResultBlock{
					CallID:  id,
					Content: []provider.Content{provider.TextBlock{Text: strings.Repeat("界", maxToolResultTextBytes+100)}},
				}},
			},
		)
	}

	projected := projectToolResultMessages(msgs)
	for _, message := range projected {
		for _, content := range message.Content {
			result, ok := content.(provider.ToolResultBlock)
			if !ok {
				continue
			}
			retainedBytes := retainedToolResultTextBytes(result.Content)
			if retainedBytes > maxToolResultTextBytes {
				t.Fatalf("result %q retains %d text bytes, want <= %d", result.CallID, retainedBytes, maxToolResultTextBytes)
			}
			if !hasToolResultOmissionMarker(result.Content) {
				t.Fatalf("truncated result %q is missing the omission marker", result.CallID)
			}
			for _, inner := range result.Content {
				if text, ok := inner.(provider.TextBlock); ok && !utf8.ValidString(text.Text) {
					t.Fatalf("result %q contains invalid UTF-8", result.CallID)
				}
			}
		}
	}
}

func TestProjectToolResultMessagesBoundsEveryResultIndependently(t *testing.T) {
	msgs := make([]provider.Message, 0, 10)
	for i := 1; i <= 5; i++ {
		id := "call-" + string(rune('0'+i))
		text := strings.Repeat("result-"+id+" ", maxToolResultTextBytes/4)
		msgs = append(msgs,
			provider.Message{
				Role:    provider.RoleAssistant,
				Content: []provider.Content{provider.ToolCallBlock{ID: id, Name: "read"}},
			},
			provider.Message{
				Role:    provider.RoleTool,
				Content: []provider.Content{provider.ToolResultBlock{CallID: id, Content: []provider.Content{provider.TextBlock{Text: text}}}},
			},
		)
	}

	projected := projectToolResultMessages(msgs)
	for i := 1; i <= 5; i++ {
		id := "call-" + string(rune('0'+i))
		got := toolResultTextForCall(projected, id)
		if !strings.HasPrefix(got, "result-"+id+" ") || !strings.Contains(got, toolResultOmissionMarker) {
			t.Fatalf("result %s was not independently retained and truncated: %q", id, got[:min(len(got), 80)])
		}
	}
}

func TestProjectToolResultMessagesDoesNotExhaustAnAggregateBudget(t *testing.T) {
	const resultCount = 5
	msgs := make([]provider.Message, 0, resultCount)
	for i := 0; i < resultCount; i++ {
		id := "call-" + string(rune('1'+i))
		msgs = append(msgs, provider.Message{
			Role: provider.RoleTool,
			Content: []provider.Content{provider.ToolResultBlock{
				CallID:  id,
				Content: []provider.Content{provider.TextBlock{Text: strings.Repeat("x", maxToolResultTextBytes+1)}},
			}},
		})
	}

	projected := projectToolResultMessages(msgs)
	retainedTotal := 0
	for _, message := range projected {
		for _, content := range message.Content {
			result, ok := content.(provider.ToolResultBlock)
			if !ok {
				continue
			}
			if !hasToolResultOmissionMarker(result.Content) {
				t.Fatalf("result %q is missing the omission marker", result.CallID)
			}
			retainedTotal += retainedToolResultTextBytes(result.Content)
		}
	}
	if retainedTotal != resultCount*maxToolResultTextBytes {
		t.Fatalf("retained tool-result text = %d bytes, want %d independent bytes", retainedTotal, resultCount*maxToolResultTextBytes)
	}
	if got := toolResultTextForCall(projected, "call-1"); got == toolResultOmissionMarker {
		t.Fatalf("old result was discarded after later results: %q", got)
	}
}

func TestProjectToolResultMessagesPreservesToolResultBlocksAndRawInput(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.ToolCallBlock{ID: "call-1", Name: "read"},
			provider.ToolCallBlock{ID: "call-2", Name: "bash"},
		}},
		{Role: provider.RoleTool, Content: []provider.Content{
			provider.ToolResultBlock{CallID: "call-1", Content: []provider.Content{provider.TextBlock{Text: strings.Repeat("one", maxToolResultTextBytes)}}},
			provider.ToolResultBlock{CallID: "call-2", Content: []provider.Content{provider.TextBlock{Text: strings.Repeat("two", maxToolResultTextBytes)}}},
		}},
	}
	wantRaw := append([]provider.Message(nil), msgs...)

	projected := projectToolResultMessages(msgs)
	if !reflect.DeepEqual(msgs, wantRaw) {
		t.Fatal("projection mutated the raw input transcript")
	}
	var resultIDs []string
	for _, content := range projected[1].Content {
		result, ok := content.(provider.ToolResultBlock)
		if !ok {
			t.Fatalf("projected content = %T, want ToolResultBlock", content)
		}
		resultIDs = append(resultIDs, result.CallID)
	}
	if !reflect.DeepEqual(resultIDs, []string{"call-1", "call-2"}) {
		t.Fatalf("projected result IDs = %v, want both tool results in order", resultIDs)
	}
	if got := toolResultTextForCall(msgs, "call-1"); got != strings.Repeat("one", maxToolResultTextBytes) {
		t.Fatal("raw tool result text changed after projection")
	}
}

func TestAgentNormalRequestProjectsToolResultsAfterPairRepair(t *testing.T) {
	client := &captureClient{}
	agent := NewAgent(client, "model", "system", Registry{})
	full := strings.Repeat("full-normal-result ", maxToolResultTextBytes)
	timing := &provider.ToolTiming{StartedAt: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC), CompletedAt: time.Date(2026, 8, 8, 12, 0, 1, 0, time.UTC), Duration: time.Second}
	agent.SetMessages([]provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "continue"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "call-1", Name: "read"}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "call-1",
			Content: []provider.Content{provider.TextBlock{Text: full}},
			Timing:  timing,
		}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "call-2", Name: "read"}}},
	})

	if err := agent.Continue(context.Background(), nil); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	projectedText := toolResultTextForCall(client.lastReq.Messages, "call-1")
	if !strings.Contains(projectedText, toolResultOmissionMarker) {
		t.Fatalf("normal request result was not bounded: %d bytes, %q", len(projectedText), projectedText[:min(len(projectedText), 80)])
	}
	if !strings.Contains(projectedText, formatToolTiming(timing)) {
		t.Fatal("normal request is missing the tool timing annotation")
	}
	if !hasToolResultCall(client.lastReq.Messages, "call-2") {
		t.Fatal("pair repair result was not preserved in normal request")
	}
	if got := toolResultTextForCall(agent.Messages(), "call-1"); got != full {
		t.Fatalf("raw Agent.Messages tool result changed: got %d bytes, want %d", len(got), len(full))
	}
	if hasToolResultCall(agent.Messages(), "call-2") {
		t.Fatal("pair-repair stub leaked into raw Agent.Messages")
	}
}

func TestCompactionProjectsSummaryInputWithoutChangingPersistedTail(t *testing.T) {
	client := &compactLifecycleClient{}
	agent := NewAgent(client, "model", "system", Registry{})
	oldText := strings.Repeat("full-compaction-result ", maxToolResultTextBytes)
	timing := &provider.ToolTiming{StartedAt: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC), CompletedAt: time.Date(2026, 8, 8, 12, 0, 1, 0, time.UTC), Duration: time.Second}
	tailText := strings.Repeat("full-tail-result ", maxToolResultTextBytes)
	agent.SetMessages([]provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "summarize this"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "old-call", Name: "read"}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "old-call",
			Content: []provider.Content{provider.TextBlock{Text: oldText}},
			Timing:  timing,
		}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "tail-call", Name: "read"}}},
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{
			CallID:  "tail-call",
			Content: []provider.Content{provider.TextBlock{Text: tailText}},
		}}},
	})
	var persisted []provider.Message
	agent.OnTranscriptCompacted = func(messages []provider.Message) {
		persisted = messages
	}

	if _, err := agent.Compact(context.Background(), 2, nil); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	requestText := client.req.Messages[0].Content[0].(provider.TextBlock).Text
	if !strings.Contains(requestText, toolResultOmissionMarker) {
		t.Fatal("compaction request is missing the tool-result omission marker")
	}
	if !strings.Contains(requestText, formatToolTiming(timing)) {
		t.Fatal("compaction request is missing the tool timing annotation")
	}
	if got := toolResultTextForCall(agent.Messages(), "tail-call"); got != tailText {
		t.Fatalf("compacted Agent.Messages tail changed: got %d bytes, want %d", len(got), len(tailText))
	}
	if got := toolResultTextForCall(persisted, "tail-call"); got != tailText {
		t.Fatalf("persisted compacted tail changed: got %d bytes, want %d", len(got), len(tailText))
	}
	if hasToolResultCall(agent.Messages(), "old-call") {
		t.Fatal("summarized tool result remained in the compacted transcript")
	}
}

func retainedToolResultTextBytes(content []provider.Content) int {
	total := 0
	for _, block := range content {
		if text, ok := block.(provider.TextBlock); ok && text.Text != toolResultOmissionMarker {
			total += len(text.Text)
		}
	}
	return total
}

func hasToolResultOmissionMarker(content []provider.Content) bool {
	for _, block := range content {
		if text, ok := block.(provider.TextBlock); ok && text.Text == toolResultOmissionMarker {
			return true
		}
	}
	return false
}

func retainedToolResultTextBytesForCall(messages []provider.Message, callID string) int {
	for _, message := range messages {
		for _, content := range message.Content {
			result, ok := content.(provider.ToolResultBlock)
			if ok && result.CallID == callID {
				return retainedToolResultTextBytes(result.Content)
			}
		}
	}
	return 0
}

func toolResultTextForCall(messages []provider.Message, callID string) string {
	var text strings.Builder
	for _, message := range messages {
		for _, content := range message.Content {
			result, ok := content.(provider.ToolResultBlock)
			if !ok || result.CallID != callID {
				continue
			}
			for _, inner := range result.Content {
				if block, ok := inner.(provider.TextBlock); ok {
					text.WriteString(block.Text)
				}
			}
		}
	}
	return text.String()
}

func hasToolResultCall(messages []provider.Message, callID string) bool {
	for _, message := range messages {
		for _, content := range message.Content {
			if result, ok := content.(provider.ToolResultBlock); ok && result.CallID == callID {
				return true
			}
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
