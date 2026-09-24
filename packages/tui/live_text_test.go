package tui

import (
	"strings"
	"testing"
)

const liveTextSample = "Intro paragraph with **bold** and `code`.\n\n" +
	"## Heading\n\n" +
	"- item one\n- item two\n  - nested\n\n" +
	"```go\nfunc main() {\n\n\tprintln(\"hi\")\n}\n```\n\n" +
	"| a | b |\n| --- | ---: |\n| 1 | 2 |\n\n\n" +
	"> quoted line\n\n" +
	"1. first\n2. second\n\n" +
	"A long closing paragraph that should wrap across several rows when the terminal is narrow enough to force it.\n"

// Live rows must equal the finished-message rows for every prefix of a reply,
// or rows already in terminal scrollback would diverge from the final text.
func TestLiveTextRowsMatchFinalRendering(t *testing.T) {
	for _, width := range []int{24, 80} {
		v := &View{Theme: Dark}
		for n := 1; n <= len(liveTextSample); n++ {
			prefix := liveTextSample[:n]
			got := strings.Join(v.liveTextRows(prefix, width), "\n")
			want := strings.Join(renderAssistantText(prefix, Dark, width), "\n")
			if got != want {
				t.Fatalf("width %d, prefix %d %q:\nlive:\n%s\nfinal:\n%s", width, n, prefix, got, want)
			}
		}
		if v.liveText.src == "" {
			t.Fatalf("width %d: no finished block was cached", width)
		}
	}
}

// A new reply that does not extend the cached text starts a fresh cache.
func TestLiveTextRowsResetOnNewReply(t *testing.T) {
	v := &View{Theme: Dark}
	v.liveTextRows("first reply\n\nmore", 80)
	got := strings.Join(v.liveTextRows("second", 80), "\n")
	want := strings.Join(renderAssistantText("second", Dark, 80), "\n")
	if got != want {
		t.Fatalf("stale cache leaked into a new reply:\n%s", got)
	}
}

func TestStableMarkdownPrefixSkipsOpenFence(t *testing.T) {
	src := "text\n\n```\ncode\n\nmore code\n"
	if got := stableMarkdownPrefix(src); got != len("text\n\n") {
		t.Fatalf("cut = %d, want %d (no split inside an open fence)", got, len("text\n\n"))
	}
}

func BenchmarkLiveTextStreaming(b *testing.B) {
	text := strings.Repeat(liveTextSample, 8)
	for b.Loop() {
		v := &View{Theme: Dark}
		for n := 64; n <= len(text); n += 64 {
			v.liveTextRows(text[:n], 100)
		}
	}
}

func BenchmarkFullRerenderStreaming(b *testing.B) {
	text := strings.Repeat(liveTextSample, 8)
	for b.Loop() {
		for n := 64; n <= len(text); n += 64 {
			renderAssistantText(text[:n], Dark, 100)
		}
	}
}
