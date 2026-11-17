package core

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func serverToolSessionMessages() []provider.Message {
	use := provider.ServerToolBlock{Provider: "anthropic", Kind: provider.ServerToolKindUse, ID: "srvtoolu_test1", Name: "advisor",
		Raw: json.RawMessage(`{"type":"server_tool_use","id":"srvtoolu_test1","name":"advisor","input":{}}`)}
	result := provider.ServerToolBlock{Provider: "anthropic", Kind: provider.ServerToolKindAdvisorResult, ID: "srvtoolu_test1", Name: "advisor", Redacted: true,
		Raw: json.RawMessage(`{"type":"advisor_tool_result","tool_use_id":"srvtoolu_test1","content":{"type":"advisor_redacted_result","encrypted_content":"ZW5jcnlwdGVk"}}`)}
	return []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "hi"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{use, result, provider.TextBlock{Text: "done"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "next"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "ok"}}},
	}
}

func assertServerToolBlocks(t *testing.T, label string, got []provider.Message) {
	t.Helper()
	want := serverToolSessionMessages()
	if len(got) < 2 {
		t.Fatalf("%s: messages = %d", label, len(got))
	}
	for i, wantBlock := range want[1].Content[:2] {
		gotBlock, ok := got[1].Content[i].(provider.ServerToolBlock)
		if !ok {
			t.Fatalf("%s: block %d decoded as %T, want ServerToolBlock", label, i, got[1].Content[i])
		}
		w := wantBlock.(provider.ServerToolBlock)
		if gotBlock.Kind != w.Kind || gotBlock.ID != w.ID || gotBlock.Provider != w.Provider || gotBlock.Redacted != w.Redacted {
			t.Fatalf("%s: block %d = %+v, want %+v", label, i, gotBlock, w)
		}
		var a, b any
		_ = json.Unmarshal(gotBlock.Raw, &a)
		_ = json.Unmarshal(w.Raw, &b)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s: raw = %s", label, gotBlock.Raw)
		}
	}
	for _, m := range got {
		for _, c := range m.Content {
			if _, ok := c.(provider.ToolCallBlock); ok {
				t.Fatalf("%s: server block became a tool call", label)
			}
		}
	}
}

func TestSessionServerToolBlockRoundTrip(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	s, err := NewSession(root, cwd, "anthropic", "claude-sonnet-5-5", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range serverToolSessionMessages() {
		if err := s.AppendMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Resume.
	reopened, msgs, err := OpenSession(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	_ = reopened.Close()
	assertServerToolBlocks(t, "resume", msgs)

	// Fork.
	forked, err := BranchSession(s.Path, root, cwd, "test", 3)
	if err != nil {
		t.Fatal(err)
	}
	fs, fmsgs, err := OpenSession(forked)
	if err != nil {
		t.Fatal(err)
	}
	_ = fs.Close()
	assertServerToolBlocks(t, "fork", fmsgs)

	// Portable export and import.
	exported, err := ExportSession(s.Path, filepath.Join(t.TempDir(), "export.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	imported, err := ImportSession(exported, t.TempDir(), cwd, "test")
	if err != nil {
		t.Fatal(err)
	}
	is, imsgs, err := OpenSession(imported)
	if err != nil {
		t.Fatal(err)
	}
	_ = is.Close()
	assertServerToolBlocks(t, "import", imsgs)
}

func TestDecodeMessageLegacyToolCallUnchanged(t *testing.T) {
	msg, err := DecodeMessageJSON([]byte(`{"role":"assistant","content":[{"id":"call1","name":"read","arguments":{}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := msg.Content[0].(provider.ToolCallBlock); !ok {
		t.Fatalf("legacy tool call decoded as %T", msg.Content[0])
	}
}

func TestServerToolTranscriptsOmitRaw(t *testing.T) {
	msgs := serverToolSessionMessages()
	plain := provider.ServerToolBlock{Provider: "anthropic", Kind: provider.ServerToolKindAdvisorResult, ID: "srvtoolu_p", AdvisorText: "Check the cache.", Raw: json.RawMessage(`{"secret":"blob"}`)}
	msgs[3].Content = append(msgs[3].Content, plain)
	for name, out := range map[string]string{
		"compaction": serializeTranscript(msgs),
		"advisor":    SerializeAdvisorTranscript("", nil, msgs, ""),
	} {
		if strings.Contains(out, "ZW5jcnlwdGVk") || strings.Contains(out, "secret") {
			t.Errorf("%s leaked raw: %s", name, out)
		}
		if !strings.Contains(out, "[advisor consulted] (advice encrypted)") || !strings.Contains(out, "[advisor consulted] Check the cache.") {
			t.Errorf("%s missing advisor lines: %s", name, out)
		}
	}
}

func TestDecodeMessageSkipsIncompleteServerBlock(t *testing.T) {
	msg, err := DecodeMessageJSON([]byte(`{"role":"assistant","content":[{"provider":"anthropic","kind":"server_tool_use","id":"","raw":{}},{"text":"ok"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content = %+v", msg.Content)
	}
	if _, ok := msg.Content[0].(provider.TextBlock); !ok {
		t.Fatalf("content = %+v", msg.Content)
	}
}
