package agent

import (
	"os"
	"testing"
)

func TestArgsForStdinSelectsPrintModeForPipe(t *testing.T) {
	args, err := ParseArgs(argsForStdin([]string{"summarize"}, os.ModeNamedPipe))
	if err != nil {
		t.Fatal(err)
	}
	if args.Mode != ModePrint || args.Prompt != "summarize" {
		t.Fatalf("Mode=%q Prompt=%q, want print mode with prompt", args.Mode, args.Prompt)
	}
}

func TestArgsForStdinPreservesTerminalAndExplicitModes(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		mode os.FileMode
		want Mode
	}{
		{name: "terminal stays interactive", mode: os.ModeCharDevice, want: ModeInteractive},
		{name: "explicit stream overrides pipe default", in: []string{"--stream", "prompt"}, mode: os.ModeNamedPipe, want: ModeStream},
		{name: "explicit json overrides redirected file default", in: []string{"--json", "prompt"}, want: ModeJSON},
		{name: "explicit rpc overrides pipe default", in: []string{"--rpc"}, mode: os.ModeNamedPipe, want: ModeRPC},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args, err := ParseArgs(argsForStdin(tc.in, tc.mode))
			if err != nil {
				t.Fatal(err)
			}
			if args.Mode != tc.want {
				t.Fatalf("Mode=%q, want %q", args.Mode, tc.want)
			}
		})
	}
}

func TestArgsForStdinAllowsPrintStats(t *testing.T) {
	args, err := ParseArgs(argsForStdin([]string{"--stats", "stats.json"}, os.ModeNamedPipe))
	if err != nil {
		t.Fatal(err)
	}
	if args.Mode != ModePrint || args.StatsPath != "stats.json" {
		t.Fatalf("Mode=%q StatsPath=%q, want print mode with stats path", args.Mode, args.StatsPath)
	}
}

func TestCombinePromptInput(t *testing.T) {
	tests := []struct {
		name       string
		stdin      string
		positional string
		want       string
	}{
		{name: "stdin and instruction", stdin: "input text\n", positional: "summarize it", want: "input text\nsummarize it"},
		{name: "stdin only", stdin: " input text\n", want: "input text"},
		{name: "positional only", positional: " summarize it ", want: "summarize it"},
		{name: "empty", stdin: " \n", positional: " ", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := combinePromptInput(tc.stdin, tc.positional); got != tc.want {
				t.Fatalf("combinePromptInput(%q, %q) = %q, want %q", tc.stdin, tc.positional, got, tc.want)
			}
		})
	}
}
