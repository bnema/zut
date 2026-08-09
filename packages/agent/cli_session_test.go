package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bnema/zut/packages/agent/modes"
	"github.com/bnema/zut/packages/core"
	"github.com/bnema/zut/packages/provider"
	"github.com/google/uuid"
)

type failingStdinReader struct{}

func (failingStdinReader) Read([]byte) (int, error) { return 0, errors.New("stdin failed") }

func TestReadAllStdinFromPropagatesReadErrors(t *testing.T) {
	if _, err := readAllStdinFrom(failingStdinReader{}); err == nil || !strings.Contains(err.Error(), "stdin failed") {
		t.Fatalf("readAllStdinFrom error = %v, want stdin failure", err)
	}
}

func TestOpenOrCreateSessionResumesByUUIDAcrossCWDAndAgentStores(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	otherCWD := t.TempDir()
	t.Setenv("ZUT_HOME", root)

	session, err := core.NewSession(root, cwd, "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "resume me"}},
	}); err != nil {
		t.Fatal(err)
	}
	id := session.ID
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	ag := core.NewAgent(nil, "model", "", nil)
	got, err := openOrCreateSession(context.Background(), Args{
		CWD:             otherCWD,
		Resume:          true,
		ResumeSessionID: id,
	}, Resolved{Provider: "provider", Model: "model"}, ag, "test")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != id || got.Meta.CWD != cwd {
		t.Fatalf("resumed session = %#v, want id=%q cwd=%q", got, id, cwd)
	}
	if got := firstMessageText(ag.Messages()); got != "resume me" {
		t.Fatalf("resumed message = %q, want resume me", got)
	}
	if err := got.Close(); err != nil {
		t.Fatal(err)
	}

	namedRoot := filepath.Join(root, "sessions", "agents", "demo")
	named, err := core.NewSession(namedRoot, cwd, "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := named.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "named resume me"}},
	}); err != nil {
		t.Fatal(err)
	}
	namedID := named.ID
	if err := named.Close(); err != nil {
		t.Fatal(err)
	}

	namedAgent := core.NewAgent(nil, "model", "", nil)
	namedGot, err := openOrCreateSession(context.Background(), Args{
		CWD:             otherCWD,
		Resume:          true,
		ResumeSessionID: namedID,
	}, Resolved{Provider: "provider", Model: "model"}, namedAgent, "test")
	if err != nil {
		t.Fatal(err)
	}
	if namedGot == nil || namedGot.ID != namedID || namedGot.Path != named.Path {
		t.Fatalf("named resumed session = %#v, want path %q", namedGot, named.Path)
	}
	if got := firstMessageText(namedAgent.Messages()); got != "named resume me" {
		t.Fatalf("named resumed message = %q, want named resume me", got)
	}
	if err := namedGot.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResumeHintSuppressesEmptyAndArbitrarySessions(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	empty, err := core.NewSession(root, cwd, "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(empty.Path); !os.IsNotExist(err) {
		t.Fatalf("empty session stat error = %v, want deleted", err)
	}
	if got := resumableSessionID(root, empty); got != "" {
		t.Fatalf("empty session hint ID = %q, want empty", got)
	}

	arbitraryPath := filepath.Join(t.TempDir(), "session.jsonl")
	arbitrary, err := core.NewSessionAtPath(arbitraryPath, cwd, "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := arbitrary.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "arbitrary"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := arbitrary.Close(); err != nil {
		t.Fatal(err)
	}
	if got := resumableSessionID(root, arbitrary); got != "" {
		t.Fatalf("arbitrary session hint ID = %q, want empty", got)
	}
}

func TestResumeHintSuppressesPersistedEmptyExplicitUUIDSession(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	otherCWD := t.TempDir()
	t.Setenv("ZUT_HOME", root)

	dir := core.SessionsDir(root, cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	metaLine := struct {
		Type string           `json:"type"`
		Meta core.SessionMeta `json:"meta"`
	}{
		Type: "meta",
		Meta: core.SessionMeta{
			ID:       id,
			CWD:      cwd,
			Provider: "provider",
			Model:    "model",
			Started:  time.Now().UTC(),
			Version:  "test",
		},
	}
	data, err := json.Marshal(metaLine)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "persisted-empty.jsonl")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	sess, err := openOrCreateSession(context.Background(), Args{
		CWD:             otherCWD,
		Resume:          true,
		ResumeSessionID: id,
	}, Resolved{Provider: "provider", Model: "model"}, core.NewAgent(nil, "model", "", nil), "test")
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil || sess.Path != path {
		t.Fatalf("resumed persisted empty session = %#v, want path %q", sess, path)
	}
	if got := resumableSessionID(root, sess); got != "" {
		t.Fatalf("persisted empty session hint ID = %q, want empty", got)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenOrCreateSessionUnknownUUIDDoesNotCreateSession(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ZUT_HOME", root)
	id := uuid.NewString()

	_, err := openOrCreateSession(context.Background(), Args{
		CWD:             cwd,
		Resume:          true,
		ResumeSessionID: id,
	}, Resolved{Provider: "provider", Model: "model"}, core.NewAgent(nil, "model", "", nil), "test")
	if err == nil {
		t.Fatal("unknown UUID resume unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), id) {
		t.Fatalf("unknown UUID error = %v, want session ID", err)
	}
	if sessions := core.ListSessions(root, cwd); len(sessions) != 0 {
		t.Fatalf("unknown UUID created sessions: %v", sessions)
	}
}

func TestOpenOrCreateSessionUUIDLookupReturnsMetadataErrors(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("ZUT_HOME", root)
	if err := os.MkdirAll(core.SessionsDir(root, cwd), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(core.SessionsDir(root, cwd), "bad.jsonl"), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := openOrCreateSession(context.Background(), Args{
		CWD:             cwd,
		Resume:          true,
		ResumeSessionID: uuid.NewString(),
	}, Resolved{Provider: "provider", Model: "model"}, core.NewAgent(nil, "model", "", nil), "test")
	if err == nil {
		t.Fatal("malformed UUID lookup unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "metadata") || strings.Contains(err.Error(), "not found") {
		t.Fatalf("malformed UUID lookup error = %v, want strict metadata error", err)
	}
}

func TestResumeSessionHint(t *testing.T) {
	id := uuid.NewString()
	want := "Resume this session with: zut --resume " + id + "\n"
	if got := resumeSessionHint(id); got != want {
		t.Fatalf("resume hint = %q, want %q", got, want)
	}
}

func TestTrimMessagesForResumeCarriesDeferredToolActivation(t *testing.T) {
	msgs := make([]provider.Message, 0, 101)
	msgs = append(msgs, provider.Message{
		Role:           provider.RoleTool,
		AddedToolNames: []string{"lookup_weather"},
		Content:        []provider.Content{provider.ToolResultBlock{CallID: "old-call"}},
	})
	for idx := 1; idx < 101; idx++ {
		msgs = append(msgs, provider.Message{
			Role:    provider.RoleUser,
			Content: []provider.Content{provider.TextBlock{Text: "message"}},
		})
	}
	trimmed := trimMessagesForResume(msgs, 100)
	if len(trimmed) != 100 {
		t.Fatalf("trimmed message count = %d, want 100", len(trimmed))
	}
	if len(trimmed[0].AddedToolNames) != 1 || trimmed[0].AddedToolNames[0] != "lookup_weather" {
		t.Fatalf("trimmed activation markers = %v, want lookup_weather", trimmed[0].AddedToolNames)
	}
}

func TestTrimMessagesForResumeKeepsCompactionSummaryAtTailBoundary(t *testing.T) {
	for _, total := range []int{100, 101, 102} {
		t.Run(fmt.Sprintf("%d messages", total), func(t *testing.T) {
			msgs := make([]provider.Message, 0, total)
			msgs = append(msgs, provider.Message{
				Role: provider.RoleUser,
				Meta: map[string]string{"compaction": "true"},
				Content: []provider.Content{
					provider.TextBlock{Text: "## Context Summary (compacted)"},
				},
			})
			for idx := 1; idx < total; idx++ {
				msgs = append(msgs, provider.Message{
					Role:    provider.RoleUser,
					Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("message-%d", idx)}},
				})
			}

			trimmed := trimMessagesForResume(msgs, 100)
			if len(trimmed) > 100 {
				t.Fatalf("trimmed message count = %d, want at most 100", len(trimmed))
			}
			if len(trimmed) == 0 || trimmed[0].Meta["compaction"] != "true" {
				t.Fatalf("trimmed transcript lost compaction summary: %+v", trimmed)
			}
		})
	}
}

func TestLiveInteractiveAgentUsesReplacementAgentForSessionResume(t *testing.T) {
	startup := core.NewAgent(nil, "startup-model", "", nil)
	startup.SetMessages([]provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "startup transcript"}},
	}})

	replacement := core.NewAgent(nil, "replacement-model", "", nil)
	iv := modes.NewInteractive(modes.InteractiveConfig{Agent: replacement})

	resumed := []provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "resumed transcript"}},
	}}
	liveInteractiveAgent(iv, startup).SetMessages(resumed)

	if got := firstMessageText(replacement.Messages()); got != "resumed transcript" {
		t.Fatalf("replacement agent transcript = %q, want resumed transcript", got)
	}
	if got := firstMessageText(startup.Messages()); got != "startup transcript" {
		t.Fatalf("startup agent transcript changed to %q", got)
	}
}

func TestLiveInteractiveAgentFallsBackBeforeInteractiveConstruction(t *testing.T) {
	startup := core.NewAgent(nil, "startup-model", "", nil)
	if got := liveInteractiveAgent(nil, startup); got != startup {
		t.Fatalf("fallback agent = %p, want %p", got, startup)
	}
}

func TestPersistModelCallbackDoesNotReenterSessionTransition(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "old-provider", "old-model", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	var persistMu sync.Mutex
	activeProvider, activeModel := "old-provider", "old-model"
	persistModel := newPersistModelCallback(&persistMu, &sess, &activeProvider, &activeModel, nil)

	var transitionMu sync.RWMutex
	sessionTransition := newSessionTransition(&transitionMu)
	done := make(chan struct{})
	go func() {
		sessionTransition(func() {
			persistModel("new-provider", "new-model")
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("model persistence deadlocked inside the session transition")
	}
	if sess.Meta.Provider != "new-provider" || sess.Meta.Model != "new-model" {
		t.Fatalf("session model = %q/%q, want new-provider/new-model", sess.Meta.Provider, sess.Meta.Model)
	}
	if activeProvider != "new-provider" || activeModel != "new-model" {
		t.Fatalf("active model = %q/%q, want new-provider/new-model", activeProvider, activeModel)
	}
}

func firstMessageText(messages []provider.Message) string {
	if len(messages) == 0 || len(messages[0].Content) == 0 {
		return ""
	}
	text, _ := messages[0].Content[0].(provider.TextBlock)
	return text.Text
}

func syntheticSession(t *testing.T, providerName, model string, usage provider.Usage) string {
	t.Helper()
	root := t.TempDir()
	cwd := t.TempDir()
	sess, err := core.NewSession(root, cwd, providerName, model, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "resumed transcript"}},
	}); err != nil {
		t.Fatal(err)
	}
	if usage != (provider.Usage{}) {
		if err := sess.AppendUsage(usage, usage); err != nil {
			t.Fatal(err)
		}
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	return sess.Path
}

func TestApplyInitialSessionResumeKeepsFreshEmptySessionOwned(t *testing.T) {
	root := t.TempDir()
	sess, err := core.NewSession(root, "/workspace", "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	ag := core.NewAgent(nil, "model", "", nil)
	gotSess, gotAgent, providerName, model, err := applyInitialSessionResume(context.Background(), Args{}, Resolved{Provider: "provider", Model: "model"}, nil, sess, ag)
	if err != nil {
		t.Fatal(err)
	}
	if gotSess != sess || gotAgent != ag || providerName != "provider" || model != "model" {
		t.Fatalf("empty startup resume = session %p/%p agent %p/%p provider/model %q/%q", gotSess, sess, gotAgent, ag, providerName, model)
	}
	if _, err := os.Stat(sess.Path); err != nil {
		t.Fatalf("fresh session disappeared before close: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sess.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh empty session remained after close: %v", err)
	}
}

func TestApplyInitialSessionResumeWithRuntimePreservesInheritedModelDefaults(t *testing.T) {
	t.Setenv("ZUT_HOME", t.TempDir())
	if err := SaveConfig(Config{Provider: "ollama", Model: "current-local-model"}); err != nil {
		t.Fatal(err)
	}
	path := syntheticSession(t, "ollama", "stored-local-model", provider.Usage{})
	args := Args{Mode: ModePrint, Orchestrate: true, CWD: t.TempDir()}
	base, err := Resolve(args, true)
	if err != nil {
		t.Fatal(err)
	}
	current := base.NewAgent()
	runtime := newSubagentRuntime(subagentRuntimeConfig{
		Context:  context.Background(),
		Args:     args,
		Root:     t.TempDir(),
		RepoRoot: args.CWD,
		Provider: base.Provider,
		Model:    base.Model,
	})
	defer func() { _ = runtime.Close(context.Background()) }()

	sess, _, err := core.OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	gotSess, gotAgent, providerName, model, err := applyInitialSessionResumeWithRuntime(context.Background(), args, base, nil, sess, current, runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer gotSess.Close()
	if gotAgent == current || providerName != "ollama" || model != "stored-local-model" {
		t.Fatalf("resume candidate = rebuilt=%v provider/model=%q/%q", gotAgent == current, providerName, model)
	}
	if got := runtime.currentModel(); got != "stored-local-model" {
		t.Fatalf("runtime inherited model = %q, want stored-local-model", got)
	}
	if _, ok := gotAgent.ToolsSnapshot()["subagent_spawn"]; !ok {
		t.Fatal("resumed parent lost canonical subagent_spawn manager tool")
	}
}

func TestPrepareSessionResumeRebuildsForStoredProviderAndModel(t *testing.T) {
	old := core.NewAgent(nil, "old-model", "", nil)
	old.SetMessages([]provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "current transcript"}},
	}})
	path := syntheticSession(t, "stored-provider", "stored-model", provider.Usage{
		InputTokens:  12,
		OutputTokens: 7,
	})

	var gotProvider, gotModel string
	candidate, err := prepareSessionResume(path, old, "current-provider", "old-model", func(providerName, model string) (*core.Agent, string, string, error) {
		gotProvider, gotModel = providerName, model
		return core.NewAgent(nil, model, "", nil), providerName, model, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.session.Close()

	if gotProvider != "stored-provider" || gotModel != "stored-model" {
		t.Fatalf("builder selection = %q/%q, want stored-provider/stored-model", gotProvider, gotModel)
	}
	if !candidate.rebuilt || candidate.agent == old {
		t.Fatalf("candidate rebuilt = %v, agent=%p, old=%p", candidate.rebuilt, candidate.agent, old)
	}
	if got := firstMessageText(candidate.agent.Messages()); got != "resumed transcript" {
		t.Fatalf("candidate transcript = %q, want resumed transcript", got)
	}
	if got := candidate.agent.Cost(); got.InputTokens != 12 || got.OutputTokens != 7 {
		t.Fatalf("candidate usage = %+v, want input=12 output=7", got)
	}
	if got := old.Messages(); len(got) != 1 || firstMessageText(got) != "current transcript" {
		t.Fatalf("current transcript changed during preparation: %+v", got)
	}
}

func TestPrepareSessionResumePreservesLegacyMissingMetadata(t *testing.T) {
	old := core.NewAgent(nil, "old-model", "", nil)
	old.SetMessages([]provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "current transcript"}},
	}})
	path := syntheticSession(t, "", "", provider.Usage{})

	candidate, err := prepareSessionResume(path, old, "current-provider", "old-model", func(string, string) (*core.Agent, string, string, error) {
		t.Fatal("legacy session unexpectedly requested a rebuild")
		return nil, "", "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.session.Close()

	if candidate.rebuilt || candidate.agent != old {
		t.Fatalf("legacy candidate = rebuilt %v, agent=%p; want current agent without rebuild", candidate.rebuilt, candidate.agent)
	}
	if got := firstMessageText(old.Messages()); got != "current transcript" {
		t.Fatalf("current transcript changed before commit: %q", got)
	}
	if got := firstMessageText(candidate.messages); got != "resumed transcript" {
		t.Fatalf("legacy candidate transcript = %q, want resumed transcript", got)
	}
}

func TestPrepareSessionResumePreservesCompactHandoffMetadata(t *testing.T) {
	path := syntheticSession(t, "stored-provider", "stored-model", provider.Usage{})
	session, _, err := core.OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	handoff := json.RawMessage(`{"version":1,"reason":"status_rescue","rescue_attempts":1}`)
	if err := session.UpdateCompactHandoff(handoff); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	current := core.NewAgent(nil, "current-model", "", nil)
	candidate, err := prepareSessionResume(path, current, "stored-provider", "stored-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.session.Close()
	if got := string(candidate.session.Meta.CompactHandoff); got != string(handoff) {
		t.Fatalf("candidate compact handoff = %q, want %q", got, handoff)
	}
}

func TestPrepareSessionResumeFailureLeavesCurrentAndCandidateFileUsable(t *testing.T) {
	old := core.NewAgent(nil, "old-model", "", nil)
	old.SetMessages([]provider.Message{{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "current transcript"}},
	}})
	path := syntheticSession(t, "stored-provider", "stored-model", provider.Usage{})
	wantErr := errors.New("synthetic builder failure")

	if _, err := prepareSessionResume(path, old, "current-provider", "old-model", func(string, string) (*core.Agent, string, string, error) {
		return nil, "", "", wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("prepare error = %v, want %v", err, wantErr)
	}
	if got := firstMessageText(old.Messages()); got != "current transcript" {
		t.Fatalf("current transcript changed after failed preparation: %q", got)
	}

	// The failed candidate must have released its append handle, leaving
	// the selected file readable and reopenable for a later retry.
	reopened, _, err := core.OpenSession(path)
	if err != nil {
		t.Fatalf("reopen candidate after failed preparation: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("candidate session disappeared after failed preparation: %v", err)
	}
}

func TestPrepareSessionResumeHonorsExplicitProviderModelFields(t *testing.T) {
	for _, tc := range []struct {
		name             string
		explicitProvider bool
		explicitModel    bool
		wantProvider     string
		wantModel        string
		wantBuild        bool
	}{
		{name: "both", explicitProvider: true, explicitModel: true, wantProvider: "current-provider", wantModel: "current-model"},
		{name: "provider-only", explicitProvider: true, wantProvider: "current-provider", wantModel: "stored-model", wantBuild: true},
		{name: "model-only", explicitModel: true, wantProvider: "stored-provider", wantModel: "current-model", wantBuild: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := core.NewAgent(nil, "old-model", "", nil)
			path := syntheticSession(t, "stored-provider", "stored-model", provider.Usage{})
			var gotProvider, gotModel string
			candidate, err := prepareSessionResumeWithOptions(path, old, "current-provider", "current-model", tc.explicitProvider, tc.explicitModel, func(providerName, model string) (*core.Agent, string, string, error) {
				gotProvider, gotModel = providerName, model
				return core.NewAgent(nil, model, "", nil), providerName, model, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer candidate.session.Close()
			if tc.wantBuild {
				if gotProvider != tc.wantProvider || gotModel != tc.wantModel {
					t.Fatalf("builder selection = %q/%q, want %q/%q", gotProvider, gotModel, tc.wantProvider, tc.wantModel)
				}
			} else if candidate.provider != tc.wantProvider || candidate.model != tc.wantModel || candidate.rebuilt {
				t.Fatalf("candidate selection = %q/%q rebuilt=%v, want %q/%q without rebuild", candidate.provider, candidate.model, candidate.rebuilt, tc.wantProvider, tc.wantModel)
			}
		})
	}
}

func TestWriteNewTranscriptTracksPartialAppend(t *testing.T) {
	sess, err := core.NewSession(t.TempDir(), "cwd", "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	first := provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "first"}},
	}
	invalid := provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.Content{provider.ToolCallBlock{
			ID:        "call-1",
			Name:      "tool",
			Arguments: json.RawMessage("{"),
		}},
	}
	ag := core.NewAgent(nil, "model", "", nil)
	ag.SetMessages([]provider.Message{first, invalid})
	next, err := writeNewTranscriptLocked(ag, sess, 0)
	if err == nil {
		t.Fatal("writeNewTranscriptLocked succeeded with invalid message")
	}
	if next != 1 {
		t.Fatalf("next persisted message index = %d, want 1", next)
	}

	second := provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.ToolCallBlock{ID: "call-1", Name: "tool", Arguments: json.RawMessage(`{}`)}},
	}
	third := provider.Message{
		Role:    provider.RoleTool,
		Content: []provider.Content{provider.ToolResultBlock{CallID: "call-1", Content: []provider.Content{provider.TextBlock{Text: "done"}}}},
	}
	ag.SetMessages([]provider.Message{first, second, third})
	next, err = writeNewTranscriptLocked(ag, sess, next)
	if err != nil {
		t.Fatalf("retry transcript: %v", err)
	}
	if next != 3 {
		t.Fatalf("next persisted message index after retry = %d, want 3", next)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, messages, err := core.OpenSession(sess.Path)
	if err != nil {
		t.Fatalf("reopen session: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if len(messages) != 3 {
		t.Fatalf("replayed message count = %d, want 3", len(messages))
	}
	if repaired := provider.RepairOrphanedToolResults(messages); len(repaired) != len(messages) {
		t.Fatalf("replayed transcript lost tool pairing: %#v", messages)
	}
}
