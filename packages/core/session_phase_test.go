package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func phaseOf(block provider.Content) string {
	tb, ok := block.(provider.TextBlock)
	if !ok {
		return "<non-text>"
	}
	return tb.Phase
}

func appendPhasedAssistant(t *testing.T, s *Session) {
	t.Helper()
	if err := s.AppendMessage(provider.Message{
		Role: provider.RoleAssistant,
		Content: []provider.Content{
			provider.TextBlock{Text: "working on it", Phase: provider.TextPhaseCommentary},
			provider.TextBlock{Text: "done", Phase: provider.TextPhaseFinal},
			provider.TextBlock{Text: "plain"},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionPhaseOldUnphasedFixture(t *testing.T) {
	raw := `{"role":"assistant","content":[{"text":"hello"}],"time":"2026-01-01T00:00:00Z"}`
	msg, err := DecodeMessageJSON([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeMessageJSON: %v", err)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content = %#v, want 1 block", msg.Content)
	}
	if got := phaseOf(msg.Content[0]); got != "" {
		t.Fatalf("phase = %q, want empty", got)
	}
}

func TestSessionPhaseMixedRoundTrip(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	s, err := NewSession(root, cwd, "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	appendPhasedAssistant(t, s)
	path := s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, msgs, err := OpenSession(path)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer reopened.Close()
	if len(msgs) != 1 || len(msgs[0].Content) != 3 {
		t.Fatalf("msgs = %#v, want 1 message with 3 blocks", msgs)
	}
	want := []string{provider.TextPhaseCommentary, provider.TextPhaseFinal, ""}
	for i, w := range want {
		if got := phaseOf(msgs[0].Content[i]); got != w {
			t.Fatalf("block %d phase = %q, want %q", i, got, w)
		}
	}
}

func TestSessionPhaseResumeFork(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	s, err := NewSession(root, cwd, "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Content{provider.TextBlock{Text: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	appendPhasedAssistant(t, s)
	parentPath := s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Resume: reopening the same file preserves phases.
	reopened, msgs, err := OpenSession(parentPath)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	reopened.Close()
	if len(msgs) != 2 || len(msgs[1].Content) != 3 {
		t.Fatalf("resume msgs = %#v, want 2 messages", msgs)
	}
	if got := phaseOf(msgs[1].Content[0]); got != provider.TextPhaseCommentary {
		t.Fatalf("resume phase = %q, want commentary", got)
	}

	// Fork: branching preserves phases.
	branchPath, err := BranchSession(parentPath, root, cwd, "test", 2)
	if err != nil {
		t.Fatalf("BranchSession: %v", err)
	}
	branch, branchMsgs, err := OpenSession(branchPath)
	if err != nil {
		t.Fatalf("OpenSession(branch): %v", err)
	}
	defer branch.Close()
	if len(branchMsgs) != 2 || len(branchMsgs[1].Content) != 3 {
		t.Fatalf("fork msgs = %#v, want 2 messages", branchMsgs)
	}
	want := []string{provider.TextPhaseCommentary, provider.TextPhaseFinal, ""}
	for i, w := range want {
		if got := phaseOf(branchMsgs[1].Content[i]); got != w {
			t.Fatalf("fork block %d phase = %q, want %q", i, got, w)
		}
	}
}

func TestSessionPhasePortableImportExport(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	s, err := NewSession(root, cwd, "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	appendPhasedAssistant(t, s)
	srcPath := s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	exportPath := filepath.Join(t.TempDir(), "export.json")
	out, err := ExportSession(srcPath, exportPath)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	importedPath, err := ImportSession(out, root, cwd, "test")
	if err != nil {
		t.Fatalf("ImportSession: %v", err)
	}
	imported, msgs, err := OpenSession(importedPath)
	if err != nil {
		t.Fatalf("OpenSession(imported): %v", err)
	}
	defer imported.Close()
	if len(msgs) != 1 || len(msgs[0].Content) != 3 {
		t.Fatalf("imported msgs = %#v, want 1 message with 3 blocks", msgs)
	}
	want := []string{provider.TextPhaseCommentary, provider.TextPhaseFinal, ""}
	for i, w := range want {
		if got := phaseOf(msgs[0].Content[i]); got != w {
			t.Fatalf("imported block %d phase = %q, want %q", i, got, w)
		}
	}
}

func TestSessionPhaseRetainedProjection(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Content{
			provider.TextBlock{Text: "progress", Phase: provider.TextPhaseCommentary},
			provider.TextBlock{Text: "answer", Phase: provider.TextPhaseFinal},
		}},
	}
	projected := projectProviderMessages(msgs)
	if len(projected) != 1 || len(projected[0].Content) != 2 {
		t.Fatalf("projected = %#v, want 2 retained blocks", projected)
	}
	if got := phaseOf(projected[0].Content[0]); got != provider.TextPhaseCommentary {
		t.Fatalf("projected[0] phase = %q, want commentary", got)
	}
	if got := phaseOf(projected[0].Content[1]); got != provider.TextPhaseFinal {
		t.Fatalf("projected[1] phase = %q, want final_answer", got)
	}
	// Original must not be mutated by projection.
	if got := phaseOf(msgs[0].Content[0]); got != provider.TextPhaseCommentary {
		t.Fatalf("original mutated: phase = %q", got)
	}
}

func TestSessionPhaseUnknownPreserved(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	s, err := NewSession(root, cwd, "provider", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMessage(provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.Content{provider.TextBlock{Text: "odd", Phase: "mystery"}},
	}); err != nil {
		t.Fatal(err)
	}
	path := s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Raw file must carry the unrecognized phase verbatim.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := forEachJSONLLine(f, func(line []byte) error {
		var row struct {
			Message *json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(line, &row); err != nil || row.Message == nil {
			return nil
		}
		var msg struct {
			Content []map[string]any `json:"content"`
		}
		if err := json.Unmarshal(*row.Message, &msg); err != nil {
			return nil
		}
		for _, c := range msg.Content {
			if c["phase"] == "mystery" {
				found = true
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("unrecognized phase not persisted in %s", data)
	}
	reopened, msgs, err := OpenSession(path)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer reopened.Close()
	if len(msgs) != 1 || phaseOf(msgs[0].Content[0]) != "mystery" {
		t.Fatalf("msgs = %#v, want preserved mystery phase", msgs)
	}
}
