package provider

import "testing"

func TestAnthropicAdvisorCompatible(t *testing.T) {
	for _, tc := range []struct {
		exec, adv string
		want      bool
	}{
		{"claude-sonnet-5-5", "claude-opus-5-5", true},
		{"claude-sonnet-5-5", "claude-sonnet-5-5", true},
		{"claude-sonnet-5-5", "claude-opus-4-8", false},
		{"claude-haiku-5-5", "claude-opus-4-7", true},
		{"claude-haiku-5-5", "claude-opus-4-6", false},
		{"claude-opus-4-6", "claude-opus-4-6", true},
		{"claude-opus-4-6", "claude-haiku-5-5", true},
		{"claude-opus-4-7", "claude-opus-4-6", false},
		{"claude-fable-5-2", "claude-opus-5-5", false},
		{"claude-fable-5-2", "claude-mythos-5-2", true},
		{"claude-sonnet-4-6-20260101", "claude-opus-5-5-20261001", true}, // dated IDs use their alias
		{"claude-3-7-sonnet-20250219", "claude-opus-4-7", false},         // unknown executor
		{"claude-sonnet-4-6", "gpt-6.1", false},
		{"", "", false},
	} {
		if got := AnthropicAdvisorCompatible(tc.exec, tc.adv); got != tc.want {
			t.Errorf("%s -> %s = %v, want %v", tc.exec, tc.adv, got, tc.want)
		}
	}
}
