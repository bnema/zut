package agent

import "testing"

func TestParseArgsDisablesRepetitionGuard(t *testing.T) {
	args, err := ParseArgs([]string{"--no-repetition-guard", "inspect this"})
	if err != nil {
		t.Fatalf("ParseArgs failed: %v", err)
	}
	if !args.DisableRepetitionGuard {
		t.Fatal("DisableRepetitionGuard = false, want true")
	}
	if args.Prompt != "inspect this" {
		t.Fatalf("prompt = %q, want %q", args.Prompt, "inspect this")
	}
}
