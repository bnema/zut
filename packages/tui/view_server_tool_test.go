package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bnema/zut/packages/provider"
)

func TestRenderServerToolLineSanitizes(t *testing.T) {
	view := &View{Theme: Dark}
	b := provider.ServerToolBlock{Provider: "anthropic", Kind: provider.ServerToolKindAdvisorResult, ID: "srvtoolu_test1",
		AdvisorText: "use \x1b]52;c;ZXZpbA==\x07the cache\x1b[2J", Raw: json.RawMessage(`{"secret":"blob"}`)}
	out := strings.Join(view.renderServerToolLine(b, 80), "\n")
	if strings.Contains(out, "\x1b]52") || strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x07") || strings.Contains(out, "blob") {
		t.Fatalf("unsanitized output: %q", out)
	}
	if !strings.Contains(out, "the cache") {
		t.Fatalf("advice lost: %q", out)
	}
}

func TestHashMessageServerToolFields(t *testing.T) {
	base := provider.ServerToolBlock{Provider: "anthropic", Kind: provider.ServerToolKindAdvisorResult, ID: "x", Raw: json.RawMessage(`{}`)}
	variants := []provider.ServerToolBlock{base, base, base, base}
	variants[1].AdvisorText = "a"
	variants[2].ErrorCode = "e"
	variants[3].Redacted = true
	seen := map[uint64]bool{}
	for _, v := range variants {
		h := hashMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{v}})
		if seen[h] {
			t.Fatalf("hash collision for %+v", v)
		}
		seen[h] = true
	}
}
