package modes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// scriptedTurnsClient replays one scripted event list per provider request.
type scriptedTurnsClient struct {
	turns [][]provider.Event
	call  int
}

func (c *scriptedTurnsClient) Name() string { return "scripted" }

func (c *scriptedTurnsClient) Stream(ctx context.Context, _ provider.Request) (<-chan provider.Event, error) {
	events := c.turns[min(c.call, len(c.turns)-1)]
	c.call++
	out := make(chan provider.Event, len(events))
	for _, ev := range events {
		out <- ev
	}
	close(out)
	return out, nil
}

type echoTool struct{}

func (echoTool) Name() string        { return "echo" }
func (echoTool) Description() string { return "echo" }
func (echoTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (echoTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "tool output"}}}, nil
}

func textTurn(text string, stop provider.StopReason, extra ...provider.Content) []provider.Event {
	content := append([]provider.Content{provider.TextBlock{Text: text}}, extra...)
	var events []provider.Event
	// Fat chunks like the Anthropic OAuth path: the provider is far ahead of
	// the pacer when the message completes.
	for chunk := range strings.SplitSeq(text, " ") {
		events = append(events, provider.EventTextDelta{Delta: chunk + " "})
	}
	return append(events, provider.EventDone{Stop: stop, Message: provider.Message{Role: provider.RoleAssistant, Content: content}})
}

// countVisible reports how many times text appears in the visible chat.
func countVisible(i *Interactive, text string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return strings.Count(stripANSIBytes(strings.Join(i.buildChatLocked(120), "\n")), text)
}

// newStreamRaceInteractive wires an agent to an interactive view without
// the pacer goroutine, so the test controls exactly when text is painted.
func newStreamRaceInteractive(t *testing.T, client provider.Client, tools core.Registry) (*Interactive, *core.Agent) {
	t.Helper()
	ag := core.NewAgent(client, "test-model", "", tools)
	i := &Interactive{
		agent:     ag,
		view:      &tui.View{Theme: tui.Dark},
		toolCalls: map[string]*tui.ToolCallView{},
		dirty:     make(chan struct{}, 1),
		busy:      true,
	}
	return i, ag
}

// Race 1: the agent appends the finished reply and persists it before the UI
// receives EvAssistantMessage. A frame drawn in that window must not show the
// reply both as a transcript message and as partial live text.
func TestStreamReplyNotDuplicatedWhileMessageIsPersisted(t *testing.T) {
	const reply = "alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu"
	client := &scriptedTurnsClient{turns: [][]provider.Event{textTurn(reply, provider.StopEnd)}}
	i, ag := newStreamRaceInteractive(t, client, nil)

	var seen []int
	ag.OnMessageAppended = func(m provider.Message) {
		if m.Role != provider.RoleAssistant {
			return
		}
		// Paint part of the reply, as the pacer would have by now.
		i.mu.Lock()
		drainTicks(&i.stream, 3)
		i.mu.Unlock()
		seen = append(seen, countVisible(i, "alpha"))
	}
	if err := ag.Prompt(context.Background(), "question", nil, i.handleEvent); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != 1 {
		t.Fatalf("reply painted %v times while the message was being persisted, want exactly once", seen)
	}
}

// Race 2: after a text+tool reply, the tool result lands in the transcript
// while the pacer is still painting the text. The reply must stay single.
func TestStreamReplyNotDuplicatedWhenToolResultArrivesEarly(t *testing.T) {
	const reply = "let me check that file before answering the question properly"
	toolCall := provider.ToolCallBlock{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}
	client := &scriptedTurnsClient{turns: [][]provider.Event{
		textTurn(reply, provider.StopToolUse, toolCall),
		textTurn("done", provider.StopEnd),
	}}
	i, ag := newStreamRaceInteractive(t, client, core.Registry{"echo": echoTool{}})

	var seen []int
	sawLive := false
	ag.OnMessageAppended = func(m provider.Message) {
		if m.Role != provider.RoleTool {
			return
		}
		// Tool result appended; the pacer has painted only part of the reply.
		i.mu.Lock()
		drainTicks(&i.stream, 3)
		sawLive = sawLive || i.stream.Active()
		i.mu.Unlock()
		seen = append(seen, countVisible(i, "let me"))
	}
	if err := ag.Prompt(context.Background(), "question", nil, i.handleEvent); err != nil {
		t.Fatal(err)
	}
	if !sawLive {
		t.Fatal("test setup: pacer finished before the tool result was appended")
	}
	if len(seen) != 1 || seen[0] != 1 {
		t.Fatalf("reply painted %v times after the tool result arrived, want exactly once", seen)
	}
}

// Esc usually surfaces as a context error, not StopAborted. Buffered text
// must stop at once instead of typing on and then vanishing.
func TestStreamStopsImmediatelyOnCancel(t *testing.T) {
	i := &Interactive{view: &tui.View{Theme: tui.Dark}, toolCalls: map[string]*tui.ToolCallView{}}
	i.handleEvent(core.EvAssistantStart{})
	i.handleEvent(core.EvTextDelta{Delta: strings.Repeat("x", 500)})
	i.handleEvent(core.EvTurnEnd{Stop: provider.StopError, Err: context.Canceled})
	if i.stream.Active() || i.stream.Text() != "" {
		t.Fatalf("stream still active after cancel: active=%t text=%d", i.stream.Active(), len(i.stream.Text()))
	}
}

// Replacing the agent (/model, /login) while text drains must drop the old
// agent's transcript anchor.
func TestStreamResetOnAgentReplacement(t *testing.T) {
	i := &Interactive{view: &tui.View{Theme: tui.Dark}, toolCalls: map[string]*tui.ToolCallView{}}
	i.stream.Start(3, 7)
	i.stream.Push("draining")
	i.stream.Finish()
	i.prepareReplacementAgentLocked(nil)
	if i.stream.Active() {
		t.Fatal("stream anchor survived an agent replacement")
	}
}

// After the pacer drains, the transcript shows the reply exactly once and
// the live block is gone.
func TestStreamRevealsTranscriptWhenPacerDrains(t *testing.T) {
	const reply = "one two three four five six seven eight nine ten"
	client := &scriptedTurnsClient{turns: [][]provider.Event{textTurn(reply, provider.StopEnd)}}
	i, ag := newStreamRaceInteractive(t, client, nil)
	if err := ag.Prompt(context.Background(), "question", nil, i.handleEvent); err != nil {
		t.Fatal(err)
	}

	i.mu.Lock()
	drainTicks(&i.stream, 3)
	active := i.stream.Active()
	i.mu.Unlock()
	if !active {
		t.Fatal("test setup: stream drained after one tick")
	}
	if got := countVisible(i, "one two"); got != 1 {
		t.Fatalf("reply painted %d times before draining, want 1", got)
	}

	i.mu.Lock()
	for i.stream.Active() {
		i.stream.Tick()
	}
	i.mu.Unlock()
	if got := countVisible(i, reply); got != 1 {
		t.Fatalf("reply painted %d times after draining, want 1", got)
	}
}

// A backlog far larger than the minimum rate is painted within a bounded
// number of ticks, so a fast provider never leaves the UI trailing far behind.
func TestStreamPacerCatchesUpWithLargeBacklog(t *testing.T) {
	var s streamPresenter
	s.Start(-1, 0)
	s.Push(strings.Repeat("x", 20000))
	s.Finish()
	ticks := 0
	for s.Active() {
		s.Tick()
		ticks++
	}
	if limit := paintCatchUpTicks * 8; ticks > limit {
		t.Fatalf("drained a 20k-rune backlog in %d ticks, want <= %d", ticks, limit)
	}
}

// A transcript replaced by a shorter one (compaction, /clear) must never be
// clipped by an anchor that described the old transcript.
func TestStreamAnchorIgnoredAfterTranscriptShrinks(t *testing.T) {
	var s streamPresenter
	s.Start(10, 42)
	s.Push("text")
	if _, _, ok := s.transcriptLimit(3); ok {
		t.Fatal("anchor clipped a transcript shorter than itself")
	}
	if limit, rev, ok := s.transcriptLimit(12); !ok || limit != 10 || rev != 42 {
		t.Fatalf("transcriptLimit(12) = %d, %d, %t; want 10, 42, true", limit, rev, ok)
	}
}
