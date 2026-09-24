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
	corpus := map[string]string{
		"mixed":            liveTextSample,
		"fence-like table": "| a | b |\n| --- | --- |\n```x | y\n\ntext\n```\n\nm\n\nafter",
		"crlf":             "one\r\n\r\ntwo **b**\r\n\r\n- item\r\n",
		"blank spaces":     "para one\n  \n\t\t\npara two\n\n\n\npara three",
		"leading newlines": "\n\nstarts late\n\nand ends\n\n",
		"indented fence":   "list:\n\n  ```sh\n  echo hi\n\n  echo bye\n  ```\n\ndone",
		"trailing table":   "| h | i |\n| - | - |\n| 1 | 2 |\n\n\n",
	}
	for name, sample := range corpus {
		for _, width := range []int{10, 24, 80} {
			v := &View{Theme: Dark}
			for n := 1; n <= len(sample); n++ {
				prefix := sample[:n]
				got := strings.Join(v.liveTextRows(prefix, width), "\n")
				want := strings.Join(renderAssistantText(prefix, Dark, width), "\n")
				if got != want {
					t.Fatalf("%s, width %d, prefix %d %q:\nlive:\n%q\nfinal:\n%q", name, width, n, prefix, got, want)
				}
			}
		}
	}
}

// Width and theme changes must not reuse rows rendered for other settings.
func TestLiveTextRowsResetOnWidthAndTheme(t *testing.T) {
	const text = "first **block**\n\n```go\nx := 1\n```\n\ntail"
	v := &View{Theme: Dark}
	v.liveTextRows(text, 80)
	if v.liveText.src == "" {
		t.Fatal("no finished block was cached")
	}
	if got, want := strings.Join(v.liveTextRows(text, 12), "\n"), strings.Join(renderAssistantText(text, Dark, 12), "\n"); got != want {
		t.Fatalf("width change reused stale rows:\n%q\nwant\n%q", got, want)
	}
	v.Theme = Light
	if got, want := strings.Join(v.liveTextRows(text, 12), "\n"), strings.Join(renderAssistantText(text, Light, 12), "\n"); got != want {
		t.Fatalf("theme change reused stale rows:\n%q\nwant\n%q", got, want)
	}
}

// Render snapshots must not share the live-text cache with the owning view.
func TestCloneForRenderIsolatesLiveTextCache(t *testing.T) {
	v := &View{Theme: Dark}
	v.liveTextRows("block one\n\nblock two\n\n", 80)
	clone := v.CloneForRender()
	if clone.liveText.src != "" || clone.liveText.rows != nil {
		t.Fatal("clone shares the live-text cache")
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

func TestMarkdownBoundariesSkipOpenFence(t *testing.T) {
	lines := strings.Split("text\n\n```\ncode\n\nmore code\n", "\n")
	var got []int
	renderMarkdownRaw(lines, Dark, 80, func(line, _ int) { got = append(got, line) })
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("boundaries = %v, want [2] (none inside an open fence)", got)
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
