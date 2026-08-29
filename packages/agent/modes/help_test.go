package modes

import (
	"strings"
	"testing"

	"github.com/bnema/zut/packages/tui"
)

func TestHelpShowsLlamaOnlyWhenConfigured(t *testing.T) {
	without := strings.Join(renderHelpBlock(tui.Theme{}, 80, false), "\n")
	if strings.Contains(without, "/llama") {
		t.Fatalf("help exposed /llama without login: %q", without)
	}
	with := strings.Join(renderHelpBlock(tui.Theme{}, 80, true), "\n")
	if !strings.Contains(with, "/llama") {
		t.Fatalf("help omitted /llama with login: %q", with)
	}
}
