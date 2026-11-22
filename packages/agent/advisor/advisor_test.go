package advisor

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/bnema/zut/packages/provider/providermocks"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func reply(text string) <-chan provider.Event {
	ch := make(chan provider.Event, 3)
	ch <- provider.EventTextDelta{Delta: text}
	ch <- provider.EventUsage{Usage: provider.Usage{InputTokens: 10, OutputTokens: 2}}
	ch <- provider.EventDone{Stop: provider.StopEnd}
	close(ch)
	return ch
}

func text(role provider.Role, s string, at time.Time) provider.Message {
	return provider.Message{Role: role, Content: []provider.Content{provider.TextBlock{Text: s}}, Time: at}
}

// transcript is a mutable executor transcript shared with a reviewer.
type transcript struct {
	mu   sync.Mutex
	msgs []provider.Message
}

func (tr *transcript) set(msgs ...provider.Message) {
	tr.mu.Lock()
	tr.msgs = append([]provider.Message(nil), msgs...)
	tr.mu.Unlock()
}

func (tr *transcript) add(msgs ...provider.Message) {
	tr.mu.Lock()
	tr.msgs = append(tr.msgs, msgs...)
	tr.mu.Unlock()
}

func (tr *transcript) get() []provider.Message {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]provider.Message(nil), tr.msgs...)
}

func newTestReviewer(client provider.Client, tr *transcript) *Reviewer {
	return NewReviewer(func(context.Context) (Target, error) {
		return Target{Client: client, Model: provider.Model{ID: "adv", MaxOutput: 1000}}, nil
	}, func() Snapshot {
		return Snapshot{System: "exec system", Messages: tr.get()}
	})
}

func lastUserText(req provider.Request) string {
	return req.Messages[len(req.Messages)-1].Content[0].(provider.TextBlock).Text
}

func TestReviewerSendsContextOnceThenOnlyDelta(t *testing.T) {
	client := providermocks.NewMockClient(t)
	var reqs []provider.Request
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		reqs = append(reqs, req)
		return reply("SILENT"), nil
	})
	t0 := time.Unix(100, 0)
	tr := &transcript{}
	tr.set(text(provider.RoleUser, "fix the parser", t0))
	r := newTestReviewer(client, tr)

	v, err := r.Review(context.Background(), KindStep, nil, nil)
	require.NoError(t, err)
	require.Equal(t, Silent, v.Decision)

	tr.add(text(provider.RoleAssistant, "reading parser.go", t0.Add(time.Second)))
	_, err = r.Review(context.Background(), KindStep, nil, nil)
	require.NoError(t, err)

	require.Len(t, reqs, 2)
	first := lastUserText(reqs[0])
	require.Contains(t, first, "exec system")
	require.Contains(t, first, "fix the parser")
	second := reqs[1]
	require.Len(t, second.Messages, 3, "previous user/assistant turn is replayed, then the delta")
	delta := lastUserText(second)
	require.NotContains(t, delta, "exec system")
	require.NotContains(t, delta, "fix the parser")
	require.Contains(t, delta, "reading parser.go")

	u, ok := r.TakeUsage()
	require.True(t, ok)
	require.Equal(t, 20, u.InputTokens)
	_, ok = r.TakeUsage()
	require.False(t, ok)
}

func TestReviewerResetsWhenTranscriptIsRewritten(t *testing.T) {
	client := providermocks.NewMockClient(t)
	var reqs []provider.Request
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		reqs = append(reqs, req)
		return reply("SILENT"), nil
	})
	t0 := time.Unix(100, 0)
	tr := &transcript{}
	tr.set(text(provider.RoleUser, "a", t0), text(provider.RoleAssistant, "b", t0.Add(time.Second)))
	r := newTestReviewer(client, tr)
	_, err := r.Review(context.Background(), KindStep, nil, nil)
	require.NoError(t, err)

	// Compaction replaces the transcript with a summary.
	tr.set(text(provider.RoleUser, "summary", t0.Add(time.Minute)))
	_, err = r.Review(context.Background(), KindStep, nil, nil)
	require.NoError(t, err)
	require.Len(t, reqs[1].Messages, 1, "a rewritten transcript starts a fresh conversation")
	require.Contains(t, lastUserText(reqs[1]), "exec system")
	require.Contains(t, lastUserText(reqs[1]), "summary")
}

func TestReviewerSkipsItsOwnNotesAndConsultResults(t *testing.T) {
	client := providermocks.NewMockClient(t)
	var reqs []provider.Request
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		reqs = append(reqs, req)
		return reply("check the caller"), nil
	})
	t0 := time.Unix(100, 0)
	tr := &transcript{}
	tr.set(text(provider.RoleUser, "task", t0), provider.Message{Role: provider.RoleAssistant, Time: t0.Add(time.Second), Content: []provider.Content{
		provider.ToolCallBlock{ID: "adv1", Name: "advisor", Arguments: json.RawMessage(`{}`)},
	}})
	r := newTestReviewer(client, tr)
	advice, _, err := r.Consult(context.Background(), "adv1", nil)
	require.NoError(t, err)
	require.Equal(t, "check the caller", advice)

	note := text(provider.RoleUser, "[advisor note] earlier note", t0.Add(2*time.Second))
	note.Meta = map[string]string{core.WatcherNoteMetaKey: "true"}
	tr.add(provider.Message{Role: provider.RoleTool, Time: t0.Add(2 * time.Second), Content: []provider.Content{
		provider.ToolResultBlock{CallID: "adv1", Content: []provider.Content{provider.TextBlock{Text: "check the caller"}}},
	}}, note, text(provider.RoleAssistant, "fresh work", t0.Add(3*time.Second)))
	_, err = r.Review(context.Background(), KindStep, nil, nil)
	require.NoError(t, err)
	delta := lastUserText(reqs[1])
	require.Contains(t, delta, "fresh work")
	require.NotContains(t, delta, "earlier note")
	require.NotContains(t, delta, "tool_result id=adv1")
}

func TestReviewerFailureKeepsDeltaForRetry(t *testing.T) {
	client := providermocks.NewMockClient(t)
	calls := 0
	var last provider.Request
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
		calls++
		last = req
		if calls == 1 {
			ch := make(chan provider.Event, 1)
			ch <- provider.EventDone{Err: context.DeadlineExceeded}
			close(ch)
			return ch, nil
		}
		return reply("SILENT"), nil
	})
	tr := &transcript{}
	tr.set(text(provider.RoleUser, "task", time.Unix(1, 0)))
	r := newTestReviewer(client, tr)
	_, err := r.Review(context.Background(), KindStep, nil, nil)
	require.Error(t, err)
	_, err = r.Review(context.Background(), KindStep, nil, nil)
	require.NoError(t, err)
	require.Len(t, last.Messages, 1)
	require.Contains(t, lastUserText(last), "task")
}

func TestParseVerdict(t *testing.T) {
	for in, want := range map[string]Verdict{
		"":                         {Decision: Silent},
		"SILENT":                   {Decision: Silent},
		"silent.":                  {Decision: Silent},
		"NOTE: run the tests":      {Decision: Note, Text: "run the tests"},
		"stop:  wrong file":        {Decision: Stop, Text: "wrong file"},
		"Consider the cache first": {Decision: Note, Text: "Consider the cache first"},
	} {
		require.Equal(t, want, parseVerdict(in), in)
	}
}

func bashCall(cmd string) provider.ToolCallBlock {
	args, _ := json.Marshal(map[string]string{"command": cmd})
	return provider.ToolCallBlock{ID: "b", Name: "bash", Arguments: args}
}

func TestIsChange(t *testing.T) {
	changes := []string{
		"rm -rf build", "git commit -m x", "sed -i 's/a/b/' f.go", "echo hi > out.txt",
		"cat a >> b", "go mod tidy", "npm install", "mkdir -p x && touch x/y", "gofmt -w .",
		"git branch -D old",
	}
	for _, cmd := range changes {
		require.True(t, IsChange(bashCall(cmd)), cmd)
	}
	reads := []string{
		"ls -la", "git status --short", "git diff", "grep -rn foo .", "go test ./... 2>&1 | tail",
		"cat f > /dev/null", "go vet ./...", "sed -n 1,20p f.go", "git log --oneline -5",
		`grep -rn "=>" .`, `rg 'foo->bar' src`, `grep -n "rm -rf" docs/`,
		"git log --grep rm", "a->b", "time go test ./...",
	}
	for _, cmd := range reads {
		require.False(t, IsChange(bashCall(cmd)), cmd)
	}
	for _, cmd := range []string{
		`git commit -m "use => arrows"`, `bash -c 'rm -rf build'`, "sed -i.bak s/a/b/ f", "sed -Ei s/a/b/ f",
		"sed --in-place s/a/b/ f", "gofmt -l -w .", "go test ./... 2>err.log", "npm ci",
		"cd x && git commit -m y", "find . -name '*.o' -exec rm {} +", "sudo rm f",
	} {
		require.True(t, IsChange(bashCall(cmd)), cmd)
	}
	require.True(t, IsChange(provider.ToolCallBlock{Name: "python"}))
	require.True(t, IsChange(provider.ToolCallBlock{Name: "edit"}))
	require.True(t, IsChange(provider.ToolCallBlock{Name: "write"}))
	require.False(t, IsChange(provider.ToolCallBlock{Name: "read"}))
	require.True(t, IsChange(provider.ToolCallBlock{Name: "ast", Arguments: json.RawMessage(`{"pattern":"x","rewrite":"y"}`)}))
	require.False(t, IsChange(provider.ToolCallBlock{Name: "ast", Arguments: json.RawMessage(`{"pattern":"x"}`)}))
}

func TestReviewInstructionNamesPendingCall(t *testing.T) {
	call := provider.ToolCallBlock{ID: "e2", Name: "edit", Arguments: json.RawMessage(`{"path":"lexer.go"}`)}
	got := reviewInstruction(KindChange, &call)
	require.Contains(t, got, `[tool_call id=e2 edit {"path":"lexer.go"}]`)
}

func TestReviewerTrimsOversizedTranscript(t *testing.T) {
	client := providermocks.NewMockClient(t)
	var req provider.Request
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, r provider.Request) (<-chan provider.Event, error) {
		req = r
		return reply("SILENT"), nil
	})
	tr := &transcript{}
	var msgs []provider.Message
	for i := 0; i < 200; i++ {
		msgs = append(msgs, text(provider.RoleAssistant, "step "+strings.Repeat("x", 100), time.Unix(int64(i), 0)))
	}
	msgs = append(msgs, text(provider.RoleAssistant, "latest step", time.Unix(500, 0)))
	tr.set(msgs...)
	r := NewReviewer(func(context.Context) (Target, error) {
		return Target{Client: client, Model: provider.Model{ID: "adv", ContextWindow: 4000}}, nil
	}, func() Snapshot { return Snapshot{System: "exec system", Messages: tr.get()} })
	_, err := r.Review(context.Background(), KindStep, nil, nil)
	require.NoError(t, err)
	body := lastUserText(req)
	require.Contains(t, body, "(earlier transcript omitted)")
	require.Contains(t, body, "latest step")
	require.Less(t, len(body), 6000)
	require.NotEmpty(t, req.Context.CacheSessionID)
}

func TestReviewerResetsBeforeOversizedDelta(t *testing.T) {
	client := providermocks.NewMockClient(t)
	var reqs []provider.Request
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, r provider.Request) (<-chan provider.Event, error) {
		reqs = append(reqs, r)
		return reply("NOTE: watch the lexer"), nil
	})
	tr := &transcript{}
	tr.set(text(provider.RoleUser, "task", time.Unix(1, 0)))
	r := NewReviewer(func(context.Context) (Target, error) {
		return Target{Client: client, Model: provider.Model{ID: "adv", ContextWindow: 4000}}, nil
	}, func() Snapshot { return Snapshot{System: "exec system", Messages: tr.get()} })
	_, err := r.Review(context.Background(), KindStep, nil, nil)
	require.NoError(t, err)

	tr.add(text(provider.RoleAssistant, strings.Repeat("y", 20000)+"\nlatest step", time.Unix(2, 0)))
	_, err = r.Review(context.Background(), KindStep, nil, nil)
	require.NoError(t, err)
	second := reqs[1]
	require.Len(t, second.Messages, 1, "an oversized delta starts a fresh, trimmed conversation")
	body := lastUserText(second)
	require.Contains(t, body, "(earlier transcript omitted)")
	require.Contains(t, body, "latest step")
	require.Contains(t, body, "Advice you already gave in this session", "earlier advice survives the reset")
	require.Contains(t, body, "watch the lexer")
	require.NotEqual(t, reqs[0].Context.CacheSessionID, second.Context.CacheSessionID)
}

func TestReviewerWaitHonorsContext(t *testing.T) {
	r := NewReviewer(func(context.Context) (Target, error) { return Target{}, ErrNoModel }, func() Snapshot { return Snapshot{} })
	require.NoError(t, r.acquire(context.Background()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Review(ctx, KindStep, nil, nil)
	require.ErrorIs(t, err, context.Canceled)
	r.release()
}

func TestReviewerStartedOnlyAfterTargetResolves(t *testing.T) {
	r := NewReviewer(func(context.Context) (Target, error) { return Target{}, ErrNoModel }, func() Snapshot { return Snapshot{} })
	called := false
	_, err := r.Review(context.Background(), KindChange, nil, func() { called = true })
	require.ErrorIs(t, err, ErrNoModel)
	require.False(t, called, "no review status when the advisor cannot run")
}

func TestTailFromLineKeepsRunes(t *testing.T) {
	require.Equal(t, "é", tailFromLine("aé", 2))
	require.Equal(t, "", tailFromLine("aé", 1), "never starts inside a rune")
	require.Equal(t, "line2", tailFromLine("line1\nline2", 8))
}

func TestReviewerTrimKeepsRoomBesideLargeContext(t *testing.T) {
	client := providermocks.NewMockClient(t)
	var req provider.Request
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, r provider.Request) (<-chan provider.Event, error) {
		req = r
		return reply("SILENT"), nil
	})
	tr := &transcript{}
	tr.set(text(provider.RoleAssistant, strings.Repeat("z", 20000)+"\nlatest step", time.Unix(1, 0)))
	// The executor system prompt alone exceeds half of the advisor budget.
	r := NewReviewer(func(context.Context) (Target, error) {
		return Target{Client: client, Model: provider.Model{ID: "adv", ContextWindow: 4000}}, nil
	}, func() Snapshot { return Snapshot{System: strings.Repeat("s", 4000), Messages: tr.get()} })
	_, err := r.Review(context.Background(), KindStep, nil, nil)
	require.NoError(t, err)
	body := lastUserText(req)
	require.Contains(t, body, "(earlier transcript omitted)")
	require.Contains(t, body, "latest step")
	require.Less(t, len(body), 4000+6000/8+2000)
}

func TestReviewerUndeliveredNoteIsForgottenAndAnnounced(t *testing.T) {
	client := providermocks.NewMockClient(t)
	var reqs []provider.Request
	client.EXPECT().Stream(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, r provider.Request) (<-chan provider.Event, error) {
		reqs = append(reqs, r)
		return reply("NOTE: check lexer.go"), nil
	})
	tr := &transcript{}
	tr.set(text(provider.RoleUser, "task", time.Unix(1, 0)))
	r := newTestReviewer(client, tr)
	v, err := r.Review(context.Background(), KindStep, nil, nil)
	require.NoError(t, err)
	r.Undelivered(v.Text)
	require.Empty(t, r.notes)
	_, err = r.Review(context.Background(), KindStep, nil, nil)
	require.NoError(t, err)
	require.Contains(t, lastUserText(reqs[1]), "Your previous note was not delivered")
}
