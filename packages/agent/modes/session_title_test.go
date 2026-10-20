package modes

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
)

type interactiveTitleClient struct {
	mu       sync.Mutex
	requests []provider.Request
	title    string
	titleErr error
}

func (c *interactiveTitleClient) Name() string { return "interactive-title-test" }

func (c *interactiveTitleClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()
	if req.System != "" && c.titleErr != nil {
		return nil, c.titleErr
	}
	out := make(chan provider.Event, 2)
	if req.System != "" {
		out <- provider.EventTextDelta{Delta: c.title}
		out <- provider.EventDone{Stop: provider.StopEnd}
	} else {
		out <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{provider.TextBlock{Text: "main response"}},
		}}
	}
	close(out)
	return out, nil
}

func (c *interactiveTitleClient) requestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

func waitInteractiveIdle(t *testing.T, i *Interactive) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		i.mu.Lock()
		busy := i.busy
		i.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("interactive turn did not finish")
}

func waitInteractiveTitle(t *testing.T, i *Interactive, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		i.mu.Lock()
		got := i.sessionTitle
		i.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	i.mu.Lock()
	got := i.sessionTitle
	i.mu.Unlock()
	t.Fatalf("session title = %q, want %q", got, want)
}

func waitTerminalOutput(t *testing.T, term *alertTestTerminal, want string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := term.String(); got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("terminal title output = %q, want %q", term.String(), want)
}

func TestFirstRealPromptSetsAndPersistsTerminalTitle(t *testing.T) {
	term := &alertTestTerminal{}
	client := &interactiveTitleClient{title: "Fix login retries"}
	ag := core.NewAgent(client, "test-model", "", nil)
	persisted := make(chan string, 1)
	i := NewInteractive(InteractiveConfig{
		Agent:    ag,
		Terminal: term,
		PersistTitle: func(title string) error {
			persisted <- title
			return nil
		},
	})
	i.markInteractiveStarted()

	i.Submit("Please fix login retries")
	waitInteractiveIdle(t, i)

	select {
	case got := <-persisted:
		if got != "Fix login retries" {
			t.Fatalf("persisted title = %q, want %q", got, "Fix login retries")
		}
	case <-time.After(time.Second):
		t.Fatal("title was not persisted")
	}
	waitTerminalOutput(t, term, "\x1b]0;zut: Fix login retries\x07")
	if got := len(ag.Messages()); got != 2 {
		t.Fatalf("transcript message count = %d, want user + assistant only", got)
	}
	if got := client.requestCount(); got != 2 {
		t.Fatalf("provider request count = %d, want main + hidden title", got)
	}
}

func TestClearResetsTerminalTitleAndRegeneratesOnNextPrompt(t *testing.T) {
	term := &alertTestTerminal{}
	client := &interactiveTitleClient{title: "Fix login retries"}
	ag := core.NewAgent(client, "test-model", "", nil)
	i := NewInteractive(InteractiveConfig{Agent: ag, Terminal: term})
	i.markInteractiveStarted()

	i.Submit("Please fix login retries")
	waitInteractiveIdle(t, i)
	waitInteractiveTitle(t, i, "Fix login retries")

	i.runSlash(context.Background(), "/clear")
	if got := term.String(); !strings.HasSuffix(got, "\x1b]0;zut\x07") {
		t.Fatalf("terminal title output after /clear = %q, want neutral title last", got)
	}

	client.mu.Lock()
	client.title = "Add dark theme"
	client.mu.Unlock()
	i.Submit("Please add a dark theme")
	waitInteractiveIdle(t, i)
	waitInteractiveTitle(t, i, "Add dark theme")
	if got := term.String(); !strings.HasSuffix(got, "\x1b]0;zut: Add dark theme\x07") {
		t.Fatalf("terminal title output = %q, want new title last", got)
	}
}

func TestTitleFailureFallsBackWithoutFailingMainTurn(t *testing.T) {
	term := &alertTestTerminal{}
	client := &interactiveTitleClient{titleErr: errors.New("title provider unavailable")}
	ag := core.NewAgent(client, "test-model", "", nil)
	persisted := make(chan string, 1)
	i := NewInteractive(InteractiveConfig{
		Agent:    ag,
		Terminal: term,
		PersistTitle: func(title string) error {
			persisted <- title
			return nil
		},
	})
	i.markInteractiveStarted()

	prompt := "Please fix the login retry behavior"
	i.Submit(prompt)
	waitInteractiveIdle(t, i)

	select {
	case got := <-persisted:
		if got != "Please fix the login retry behavior" {
			t.Fatalf("fallback title = %q, want prompt text", got)
		}
	case <-time.After(time.Second):
		t.Fatal("fallback title was not persisted")
	}
	if got := len(ag.Messages()); got != 2 {
		t.Fatalf("transcript message count = %d, want main turn only", got)
	}
}

func TestSkillSlashPromptDoesNotConsumeTheFirstRealPromptTitle(t *testing.T) {
	term := &alertTestTerminal{}
	client := &interactiveTitleClient{title: "should not be used"}
	ag := core.NewAgent(client, "test-model", "", nil)
	i := NewInteractive(InteractiveConfig{Agent: ag, Terminal: term})
	i.markInteractiveStarted()

	i.submitOrQueuePrompt(context.Background(), "expanded skill instructions")
	waitInteractiveIdle(t, i)

	if got := client.requestCount(); got != 1 {
		t.Fatalf("provider request count = %d, want only the slash-expanded main request", got)
	}
	i.mu.Lock()
	seen := i.titleRealPromptSeen
	i.mu.Unlock()
	if seen {
		t.Fatal("slash-expanded prompt consumed the first real prompt title slot")
	}
}

func TestStartupPreDoesNotConsumeTheFirstRealPromptTitle(t *testing.T) {
	term := &alertTestTerminal{}
	client := &interactiveTitleClient{title: "real prompt title"}
	ag := core.NewAgent(client, "test-model", "", nil)
	i := NewInteractive(InteractiveConfig{Agent: ag, Terminal: term})
	i.markInteractiveStarted()
	i.awaitingStartupPre = true

	i.Submit("load startup resources")
	waitInteractiveIdle(t, i)

	if got := client.requestCount(); got != 1 {
		t.Fatalf("startup pre request count = %d, want only the startup request", got)
	}
	i.mu.Lock()
	seen := i.titleRealPromptSeen
	i.mu.Unlock()
	if seen {
		t.Fatal("startup pre consumed the first real prompt title slot")
	}
}

func TestDisabledTerminalTitlesSkipHiddenRequest(t *testing.T) {
	term := &alertTestTerminal{}
	disabled := false
	client := &interactiveTitleClient{title: "should not be used"}
	ag := core.NewAgent(client, "test-model", "", nil)
	i := NewInteractive(InteractiveConfig{Agent: ag, Terminal: term, TerminalTitleEnabled: &disabled})
	i.markInteractiveStarted()

	i.startTurn(context.Background(), "do the work")
	waitInteractiveIdle(t, i)

	if got := client.requestCount(); got != 1 {
		t.Fatalf("provider request count = %d, want only the main request", got)
	}
	if got := term.String(); got != "" {
		t.Fatalf("disabled title output = %q, want empty", got)
	}
}

func TestFreshBranchTitleIgnoresCopiedPrefix(t *testing.T) {
	term := &alertTestTerminal{}
	client := &interactiveTitleClient{title: "new branch title"}
	ag := core.NewAgent(client, "test-model", "", nil)
	ag.SetMessages([]provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "copied parent prompt"}},
	}})
	i := NewInteractive(InteractiveConfig{
		Agent:                      ag,
		Terminal:                   term,
		InitialSessionTitlePending: true,
	})
	i.markInteractiveStarted()

	i.Submit("new branch work")
	waitInteractiveIdle(t, i)
	// The hidden title request is deliberately asynchronous, so wait for its
	// visible result rather than assuming it finishes before the main turn.
	waitInteractiveTitle(t, i, "new branch title")
	if got := client.requestCount(); got != 2 {
		t.Fatalf("provider request count = %d, want main + hidden title", got)
	}
	i.mu.Lock()
	seen := i.titleRealPromptSeen
	i.mu.Unlock()
	if !seen {
		t.Fatal("fresh branch did not record its first real prompt")
	}
}

func TestDisablingTerminalTitlesRestoresNeutralTitle(t *testing.T) {
	term := &alertTestTerminal{}
	enabled := true
	i := NewInteractive(InteractiveConfig{
		Terminal:             term,
		TerminalTitleEnabled: &enabled,
		InitialSessionTitle:  "Existing work",
	})
	i.markInteractiveStarted()
	i.applySettingToggle("terminal_title_enabled", false)

	got := term.String()
	if !strings.Contains(got, "\x1b]0;zut: Existing work\x07") || !strings.Contains(got, "\x1b]0;zut\x07") {
		t.Fatalf("terminal title output = %q, want existing and neutral title sequences", got)
	}
}

func TestLoadedSessionTitleIsRestoredWithoutGeneration(t *testing.T) {
	term := &alertTestTerminal{}
	ag := core.NewAgent(nil, "test-model", "", nil)
	ag.SetMessages([]provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "existing prompt"}},
	}})
	i := NewInteractive(InteractiveConfig{
		Agent:               ag,
		Terminal:            term,
		InitialSessionTitle: "Existing work",
	})
	i.markInteractiveStarted()

	if got, want := term.String(), "\x1b]0;zut: Existing work\x07"; got != want {
		t.Fatalf("restored terminal title = %q, want %q", got, want)
	}
	i.mu.Lock()
	seen := i.titleRealPromptSeen
	started := i.titleGenerationStarted
	i.mu.Unlock()
	if !seen || !started {
		t.Fatal("loaded session was eligible for a new title")
	}
}
