package modes

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/skills"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/tui"
)

// pinRecordingClient answers every request with "ok" and records the request.
type pinRecordingClient struct {
	requests chan provider.Request
}

func (c *pinRecordingClient) Name() string { return "pin-test" }

func (c *pinRecordingClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.requests <- req
	out := make(chan provider.Event, 2)
	out <- provider.EventTextDelta{Delta: "ok"}
	out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: "ok"}},
	}}
	close(out)
	return out, nil
}

func lastUserText(req provider.Request) string {
	for n := len(req.Messages) - 1; n >= 0; n-- {
		if req.Messages[n].Role != provider.RoleUser {
			continue
		}
		return stripMessageTime(userMessageText(req.Messages[n]))
	}
	return ""
}

// stripMessageTime drops the request-time "[message time: ...]" line the
// context builder prefixes to user messages.
func stripMessageTime(text string) string {
	if strings.HasPrefix(text, "[message time:") {
		if _, rest, ok := strings.Cut(text, "\n"); ok {
			return rest
		}
	}
	return text
}

type pinTurnHarness struct {
	i         *Interactive
	client    *pinRecordingClient
	mu        sync.Mutex
	persisted []provider.Message
}

func newPinTurnHarness(t *testing.T, pinned bool) *pinTurnHarness {
	t.Helper()
	h := &pinTurnHarness{client: &pinRecordingClient{requests: make(chan provider.Request, 8)}}
	ag := core.NewAgent(h.client, "test-model", "", nil)
	ag.OnMessageAppended = func(m provider.Message) {
		h.mu.Lock()
		h.persisted = append(h.persisted, m)
		h.mu.Unlock()
	}
	h.i = NewInteractive(InteractiveConfig{
		Agent:    ag,
		Provider: "anthropic",
		Model:    "claude-sonnet-4-5-20250929",
		CWD:      "/work",
		SkillSnapshot: func() []*skills.Skill {
			return []*skills.Skill{{Name: "review", Body: "Inspect carefully."}}
		},
		LoadSkillPins: func(string) (skills.Pins, error) {
			return skills.Pins{Project: []string{"review"}}, nil
		},
		PreloadPinnedSkills: pinned,
	})
	h.i.runCtx = context.Background()
	return h
}

// prompt submits text as genuine user input, the way the host paths do.
func (h *pinTurnHarness) prompt(t *testing.T, text string) provider.Request {
	t.Helper()
	h.i.submitOrQueueMessage(core.QueuedMessage{Text: text}, true)
	req := receiveRequest(t, h.client.requests)
	waitInteractiveIdle(t, h.i)
	return req
}

func (h *pinTurnHarness) persistedUserTexts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, m := range h.persisted {
		if m.Role == provider.RoleUser {
			out = append(out, userMessageText(m))
		}
	}
	return out
}

func TestPinnedSkillsJoinFirstPromptOnceAndPersist(t *testing.T) {
	h := newPinTurnHarness(t, true)
	first := lastUserText(h.prompt(t, "first request"))
	if strings.Count(first, "Inspect carefully.") != 1 || !strings.HasSuffix(first, "first request") {
		t.Fatalf("first prompt = %q", first)
	}
	if second := lastUserText(h.prompt(t, "second request")); second != "second request" {
		t.Fatalf("second prompt carried pins: %q", second)
	}
	texts := h.persistedUserTexts()
	if len(texts) != 2 || strings.Count(strings.Join(texts, "\n"), "Inspect carefully.") != 1 || !strings.Contains(texts[0], "Inspect carefully.") {
		t.Fatalf("persisted transcript = %q", texts)
	}
	// The notice disappears once consumed.
	h.i.mu.Lock()
	notice := h.i.skillPins.notice(h.i.cfg.Theme, 80)
	h.i.mu.Unlock()
	if len(notice) != 0 {
		t.Fatal("notice survived consumption")
	}
}

func TestPinnedSkillsAbsentWithoutArming(t *testing.T) {
	for name, arm := range map[string]func(*pinTurnHarness){
		"not configured": func(*pinTurnHarness) {},
		"resumed": func(h *pinTurnHarness) {
			h.i.SetPinnedSkillsPending(true)
			h.i.SetPinnedSkillsPending(false)
		},
		"session applied": func(h *pinTurnHarness) {
			h.i.SetPinnedSkillsPending(true)
			h.i.ApplySessionAgent(core.NewAgent(h.client, "test-model", "", nil), "anthropic", "claude-sonnet-4-5-20250929")
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newPinTurnHarness(t, false)
			arm(h)
			if got := lastUserText(h.prompt(t, "hello")); got != "hello" {
				t.Fatalf("prompt = %q", got)
			}
		})
	}
}

const pinBody = "Inspect carefully."

func (h *pinTurnHarness) pinsPending() bool {
	h.i.mu.Lock()
	defer h.i.mu.Unlock()
	return h.i.skillPins.pending
}

func TestPinnedSkillsIgnoreNonUserStarts(t *testing.T) {
	h := newPinTurnHarness(t, true)

	// Scheduler follow-up, extension prompt and host evidence are not user
	// input: they must neither carry the pins nor consume them.
	if err := h.i.SubmitFollowUp(context.Background(), "scheduled tick"); err != nil {
		t.Fatal(err)
	}
	if got := lastUserText(receiveRequest(t, h.client.requests)); got != "scheduled tick" {
		t.Fatalf("scheduled prompt = %q", got)
	}
	waitInteractiveIdle(t, h.i)

	h.i.startTurn(context.Background(), "extension prompt") // invokeExtensionCommand "prompt" action
	if got := lastUserText(receiveRequest(t, h.client.requests)); got != "extension prompt" {
		t.Fatalf("extension prompt = %q", got)
	}
	waitInteractiveIdle(t, h.i)

	h.i.submitOrQueueMessage(core.QueuedMessage{Text: "worker report", HostEvent: true}, false)
	if got := lastUserText(receiveRequest(t, h.client.requests)); got != "worker report" {
		t.Fatalf("host event = %q", got)
	}
	waitInteractiveIdle(t, h.i)

	if !h.pinsPending() {
		t.Fatal("non-user starts consumed the pins")
	}
	if first := lastUserText(h.prompt(t, "real prompt")); strings.Count(first, pinBody) != 1 {
		t.Fatalf("first genuine user prompt = %q", first)
	}
}

func TestPinnedSkillsKeepContinuationsAndRetriesPending(t *testing.T) {
	h := newPinTurnHarness(t, true)
	// Real continuation and post-compaction/overflow retry entry points.
	h.i.agent.SetMessages([]provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "earlier"}}}})
	h.i.startTurnRequest(context.Background(), "", nil, true, false)
	receiveRequest(t, h.client.requests)
	waitInteractiveIdle(t, h.i)
	h.i.startTurnRequest(context.Background(), "", nil, true, true)
	receiveRequest(t, h.client.requests)
	waitInteractiveIdle(t, h.i)
	if !h.pinsPending() {
		t.Fatal("continuation or retry consumed the pins")
	}
}

func TestPinnedSkillsModelSwapAndClear(t *testing.T) {
	h := newPinTurnHarness(t, true)
	h.i.swapModel("anthropic", "claude-opus-4-1-20250805", nil, false)
	if !h.pinsPending() {
		t.Fatal("model swap consumed the pins")
	}
	if first := lastUserText(h.prompt(t, "real prompt")); strings.Count(first, pinBody) != 1 {
		t.Fatalf("pins lost across model swap: %q", first)
	}
	h.i.swapModel("anthropic", "claude-sonnet-4-5-20250929", nil, false)
	if later := lastUserText(h.prompt(t, "later")); later != "later" {
		t.Fatalf("model swap re-armed pins: %q", later)
	}
	h.i.runSlash(context.Background(), "/clear")
	if again := lastUserText(h.prompt(t, "after clear")); strings.Count(again, pinBody) != 1 {
		t.Fatalf("/clear did not re-arm pins: %q", again)
	}
}

// A prompt typed while a startup command still owns the agent is queued, not
// started. It is the first genuine user prompt and must carry the pins once.
func TestPinnedSkillsQueuedDuringStartupPreReachProvider(t *testing.T) {
	h := newPinTurnHarness(t, true)
	h.i.mu.Lock()
	h.i.busy = true
	h.i.awaitingStartupPre = true
	h.i.mu.Unlock()

	h.i.ed.SetValue("typed while pre runs")
	h.i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})
	h.i.ed.SetValue("second typed")
	h.i.handleKey(context.Background(), tui.Key{Kind: tui.KeyEnter})

	queued := h.i.agent.PendingQueuedMessages()
	if len(queued) != 2 || strings.Count(queued[0].Text, pinBody) != 1 || !strings.HasSuffix(queued[0].Text, "typed while pre runs") || queued[1].Text != "second typed" {
		t.Fatalf("queued = %+v", queued)
	}

	// The startup turn ends; the agent loop delivers the queued prompts.
	h.i.mu.Lock()
	h.i.busy = false
	h.i.awaitingStartupPre = false
	h.i.mu.Unlock()
	h.i.startTurnRequest(context.Background(), "", nil, true, false)
	req := receiveRequest(t, h.client.requests)
	waitInteractiveIdle(t, h.i)
	if strings.Count(userMessagesText(req), pinBody) != 1 {
		t.Fatalf("provider request = %q", userMessagesText(req))
	}
	if strings.Count(strings.Join(h.persistedUserTexts(), "\n"), pinBody) != 1 {
		t.Fatalf("persisted = %q", h.persistedUserTexts())
	}
}

func TestPinnedSkillsCarryOncePastPreTurnCompaction(t *testing.T) {
	h := newPinTurnHarness(t, true)
	h.i.agent.SetMessages([]provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "earlier one"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "reply one"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "earlier two"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "reply two"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "earlier three"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "reply three"}}},
	})
	threshold := 70
	h.i.cfg.AutoCompactThreshold = &threshold
	h.i.mu.Lock()
	h.i.lastCtxInput = 10_000_000
	h.i.mu.Unlock()

	h.i.submitOrQueueMessage(core.QueuedMessage{Text: "big request"}, true)
	compaction := receiveRequest(t, h.client.requests)
	if strings.Contains(userMessagesText(compaction), pinBody) {
		t.Fatal("compaction request carried pinned skills")
	}
	h.i.mu.Lock()
	h.i.lastCtxInput = 0
	h.i.mu.Unlock()
	followUp := receiveRequest(t, h.client.requests)
	waitInteractiveIdle(t, h.i)
	if strings.Count(userMessagesText(followUp), pinBody) != 1 {
		t.Fatalf("follow-up did not carry pins exactly once: %q", userMessagesText(followUp))
	}
}

func TestPinnedSkillsClearResetsCompactionEstimate(t *testing.T) {
	h := newPinTurnHarness(t, true)
	h.prompt(t, "before clear")
	h.i.mu.Lock()
	h.i.lastCtxInput = 10_000_000
	h.i.mu.Unlock()
	h.i.runSlash(context.Background(), "/clear")
	if got := lastUserText(h.prompt(t, "after clear")); strings.Count(got, pinBody) != 1 || !strings.HasSuffix(got, "after clear") {
		t.Fatalf("first request after clear = %q", got)
	}
}

func TestPinnedSkillsRearmAfterFailedPreTurnCompaction(t *testing.T) {
	h := newPinTurnHarness(t, true)
	threshold := 70
	h.i.cfg.AutoCompactThreshold = &threshold
	h.i.mu.Lock()
	h.i.lastCtxInput = 10_000_000
	h.i.mu.Unlock()
	// An empty transcript cannot compact: the pending user prompt never reaches
	// the provider, and its pinned selection must be released for a retry.
	h.i.submitOrQueueMessage(core.QueuedMessage{Text: "undelivered request"}, true)
	waitInteractiveIdle(t, h.i)
	h.i.mu.Lock()
	errText := h.i.statusErr
	h.i.lastCtxInput = 0
	h.i.mu.Unlock()
	if !strings.Contains(errText, "nothing to compact") || !h.pinsPending() {
		t.Fatalf("failed compaction: status=%q pending=%v", errText, h.pinsPending())
	}
	select {
	case req := <-h.client.requests:
		t.Fatalf("failed compaction sent unexpected request: %q", userMessagesText(req))
	default:
	}
	if got := lastUserText(h.prompt(t, "retried request")); strings.Count(got, pinBody) != 1 || !strings.HasSuffix(got, "retried request") {
		t.Fatalf("retry after failed compaction = %q", got)
	}
}

func TestPinnedSkillsRearmAfterCancelledPreTurnCompaction(t *testing.T) {
	client := &compactQueueClient{
		compactionStarted: make(chan struct{}), releaseCompaction: make(chan struct{}),
		followUpRequest: make(chan provider.Request, 1),
	}
	ag := core.NewAgent(client, "test-model", "", nil)
	ag.SetMessages([]provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "earlier context"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "earlier answer"}}},
	})
	threshold := 70
	i := NewInteractive(InteractiveConfig{
		Agent: ag, Provider: "anthropic", Model: "claude-sonnet-4-5-20250929", CWD: t.TempDir(),
		AutoCompactThreshold: &threshold, PreloadPinnedSkills: true,
		LoadSkillPins: func(string) (skills.Pins, error) { return skills.Pins{Global: []string{"review"}}, nil },
		SkillSnapshot: reviewSnapshot,
	})
	i.runCtx = context.Background()
	t.Cleanup(i.CancelTurn)
	i.mu.Lock()
	i.lastCtxInput = 150000
	i.mu.Unlock()
	i.submitOrQueueMessage(core.QueuedMessage{Text: "undelivered request"}, true)
	select {
	case <-client.compactionStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("compaction did not start")
	}
	i.mu.Lock()
	cancel := i.cancelTurn
	i.mu.Unlock()
	if cancel == nil {
		t.Fatal("compaction has no cancellation")
	}
	cancel()
	waitInteractiveIdle(t, i)
	i.mu.Lock()
	pending := i.skillPins.pending
	i.lastCtxInput = 0
	i.mu.Unlock()
	if !pending {
		t.Fatal("cancelled compaction lost the pinned selection")
	}
	i.submitOrQueueMessage(core.QueuedMessage{Text: "retried request"}, true)
	select {
	case req := <-client.followUpRequest:
		if got := lastUserText(req); strings.Count(got, "Inspect carefully.") != 1 || !strings.HasSuffix(got, "retried request") {
			t.Fatalf("retry after cancelled compaction = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry did not reach provider")
	}
	waitInteractiveIdle(t, i)
}

func userMessagesText(req provider.Request) string {
	var parts []string
	for _, m := range req.Messages {
		if m.Role == provider.RoleUser {
			parts = append(parts, userMessageText(m))
		}
	}
	return strings.Join(parts, "\n")
}

func TestSkillsPickerPinKeysUseHostCallbacksAndDedupeWarnings(t *testing.T) {
	var toggled []string
	i := NewInteractive(InteractiveConfig{
		CWD:           "/work",
		SkillSnapshot: reviewSnapshot,
		LoadSkillPins: func(string) (skills.Pins, error) {
			return skills.Pins{Global: []string{"gone"}}, nil
		},
		ToggleSkillPin: func(cwd, name string, global bool) error {
			toggled = append(toggled, cwd+":"+name)
			return nil
		},
		PreloadPinnedSkills: true,
	})
	i.openSkillsDialog()
	i.openSkillsDialog()
	i.handleKey(context.Background(), pinKey('p'))
	if len(toggled) != 1 || toggled[0] != "/work:review" {
		t.Fatalf("toggle calls = %v", toggled)
	}
	i.mu.Lock()
	warnings := 0
	for _, e := range i.reloadErrors {
		if strings.Contains(e, "gone") {
			warnings++
		}
	}
	notice := i.statusOK
	i.mu.Unlock()
	if warnings != 1 || notice == "" {
		t.Fatalf("warnings=%d status=%q reloadErrors=%q", warnings, notice, i.reloadErrors)
	}
}
