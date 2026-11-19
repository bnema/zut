package modes

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/bnema/zut/packages/agent/session/sessionmocks"
	"github.com/bnema/zut/packages/provider"
)

func TestExtractCopyText(t *testing.T) {
	for _, tt := range []struct {
		name, markdown string
		all            bool
		text, kind     string
	}{
		{"bare separator", "> first\n>\n> second", false, "first\n\nsecond", "quote block"},
		{"space separator", "> first\n> \n> second", false, "first\n\nsecond", "quote block"},
		{"code", "Intro\n```go\n  x\n> literal\n```\nEnd", false, "  x\n> literal", "code block"},
		{"last code", "> quote\n\n```\ncode\n```", false, "code", "code block"},
		{"last quote", "```\ncode\n```\n\n> quote", false, "quote", "quote block"},
		{"last of quotes", "> old\n\ntext\n> new", false, "new", "quote block"},
		{"plain", "hello\nworld\n", false, "hello\nworld\n", "message"},
		{"all", "> quote\n```\ncode\n```", true, "> quote\n```\ncode\n```", "message"},
		{"long fence", "````md\n```\n````", false, "```", "code block"},
		{"tilde fence", "~~~\ncode\n~~~", false, "code", "code block"},
		{"unfinished fence", "```\ncode", false, "code", "code block"},
		{"indented fence is code", "```\n    ```\n> literal\n```", false, "    ```\n> literal", "code block"},
		{"crlf quote", "> first\r\n> second\r\nend", false, "first\nsecond", "quote block"},
		{"crlf code", "```\r\ncode\r\n```\r\n", false, "code", "code block"},
		{"crlf all keeps bytes", "> a\r\n", true, "> a\r\n", "message"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			text, kind := extractCopyText(tt.markdown, tt.all)
			if text != tt.text || kind != tt.kind {
				t.Fatalf("got (%q, %q), want (%q, %q)", text, kind, tt.text, tt.kind)
			}
		})
	}
}

func TestLastAssistantCopyText(t *testing.T) {
	messages := []provider.Message{
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "old"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "> raw"}, provider.TextBlock{Text: " markdown"}}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "ignore"}}},
	}
	if text, ok := lastAssistantCopyText(messages); !ok || text != "> raw markdown" {
		t.Fatalf("got %q, %v", text, ok)
	}
	withToolCall := append(messages, provider.Message{Role: provider.RoleAssistant})
	if text, ok := lastAssistantCopyText(withToolCall); !ok || text != "> raw markdown" {
		t.Fatalf("text-less assistant turn: got %q, %v", text, ok)
	}
	if _, ok := lastAssistantCopyText(messages[2:]); ok {
		t.Fatal("found an assistant in user-only transcript")
	}
}

func TestCopyCommandErrors(t *testing.T) {
	for _, tt := range []struct{ cmd, want string }{
		{"/copy", "nothing to copy"},
		{"/copy all", "nothing to copy"},
		{"/copy nope", "usage: /copy [all]"},
		{"/copy all extra", "usage: /copy [all]"},
	} {
		t.Run(tt.cmd, func(t *testing.T) {
			i := &Interactive{statusOK: "stale"}
			i.runSlash(context.Background(), tt.cmd)
			if i.statusErr != tt.want || i.statusOK != "" {
				t.Fatalf("status = %q, %q", i.statusErr, i.statusOK)
			}
		})
	}
}

func TestCopyCommandDaemonTranscript(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "vev")
	t.Setenv("PATH", t.TempDir())
	term := &alertTestTerminal{}
	i := NewInteractive(InteractiveConfig{Terminal: term, Port: sessionmocks.NewMockPort(t)})
	i.portMsgs = []provider.Message{{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "Intro\n> first\n>\n> second"}}}}
	for _, tt := range []struct{ cmd, text, status string }{
		{"/copy", "first\n\nsecond", "copied quote block (3 lines)"},
		{"/copy all", "Intro\n> first\n>\n> second", "copied message"},
	} {
		before := term.String()
		i.runSlash(context.Background(), tt.cmd)
		want := before + "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(tt.text)) + "\a"
		if term.String() != want {
			t.Fatalf("clipboard output = %q, want %q", term.String(), want)
		}
		if i.statusOK != tt.status || i.statusErr != "" {
			t.Fatalf("status = %q, %q", i.statusOK, i.statusErr)
		}
	}
}
